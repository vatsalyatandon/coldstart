package collect

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	fakerest "k8s.io/client-go/rest/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

func TestParseResource(t *testing.T) {
	tests := []struct {
		input string
		want  ResourceRef
	}{
		{input: "pod/qwen", want: ResourceRef{Kind: "pod", Name: "qwen"}},
		{input: " pods/qwen ", want: ResourceRef{Kind: "pod", Name: "qwen"}},
		{input: "deploy/qwen", want: ResourceRef{Kind: "deployment", Name: "qwen"}},
		{input: "deployment/qwen", want: ResourceRef{Kind: "deployment", Name: "qwen"}},
		{input: "deployments/qwen", want: ResourceRef{Kind: "deployment", Name: "qwen"}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseResource(tt.input)
			if err != nil {
				t.Fatalf("ParseResource() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ParseResource() = %#v, want %#v", got, tt.want)
			}
		})
	}

	for _, input := range []string{"", "qwen", "pod/", "pod/a/b", "statefulset/qwen", "pod/bad name"} {
		t.Run("invalid_"+input, func(t *testing.T) {
			if _, err := ParseResource(input); err == nil {
				t.Fatalf("ParseResource(%q) unexpectedly succeeded", input)
			}
		})
	}
}

func TestParseLogStream(t *testing.T) {
	raw := []byte("2026-11-27T02:41:09.123456789Z loading weights  \n" +
		"2026-11-27T02:41:10.5+00:00\t  indented text\n" +
		"not a timestamp\n")

	lines, malformed := parseLogStream(raw)
	if malformed != 1 {
		t.Fatalf("malformed = %d, want 1", malformed)
	}
	if len(lines) != 3 {
		t.Fatalf("len(lines) = %d, want 3", len(lines))
	}

	wantFirst := time.Date(2026, 11, 27, 2, 41, 9, 123456789, time.UTC)
	if !lines[0].Timestamp.Equal(wantFirst) {
		t.Errorf("first timestamp = %s, want %s", lines[0].Timestamp, wantFirst)
	}
	if lines[0].Text != "loading weights  " {
		t.Errorf("first raw text = %q, want trailing spaces preserved", lines[0].Text)
	}
	if lines[1].Text != "  indented text" {
		t.Errorf("second raw text = %q, want whitespace after the separator preserved", lines[1].Text)
	}
	if !lines[2].Timestamp.IsZero() || lines[2].Text != "not a timestamp" {
		t.Errorf("malformed line = %#v, want raw text with zero timestamp", lines[2])
	}
}

func TestParseLogStreamEmptyAndTrailingNewline(t *testing.T) {
	lines, malformed := parseLogStream(nil)
	if lines != nil || malformed != 0 {
		t.Fatalf("parseLogStream(nil) = %#v, %d; want nil, 0", lines, malformed)
	}

	lines, malformed = parseLogStream([]byte("2026-11-27T02:41:09Z ready\n"))
	if malformed != 0 || len(lines) != 1 || lines[0].Text != "ready" {
		t.Fatalf("parseLogStream(trailing newline) = %#v, %d", lines, malformed)
	}
}

func TestNewestPod(t *testing.T) {
	older := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "old", CreationTimestamp: metav1.NewTime(older)}},
		{ObjectMeta: metav1.ObjectMeta{Name: "new", CreationTimestamp: metav1.NewTime(newer)}},
	}
	// Verify deterministic ordering and the lexical tie-break for pods created
	// in the same API timestamp tick.
	if got := newestPod(pods); got == nil || got.Name != "new" {
		t.Fatalf("newestPod() = %#v, want new", got)
	}
	if got := newestPod(append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", CreationTimestamp: metav1.NewTime(newer)}})); got == nil || got.Name != "new" {
		t.Fatalf("newestPod() tie break = %#v, want new", got)
	}
}

func TestEventTimeFallbacks(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC))
	event := corev1.Event{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: created}}
	if got := eventTime(event); !got.Equal(created.Time) {
		t.Fatalf("eventTime() = %s, want %s", got, created.Time)
	}
}

func TestCollectLogsBoundsStreamsAndKeepsInit(t *testing.T) {
	started := time.Date(2026, 11, 27, 2, 41, 9, 0, time.UTC)
	initStarted := started.Add(-20 * time.Second)
	previousStarted := started.Add(-2 * time.Minute)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "engine", Namespace: "test"},
		Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "engine"}},
			InitContainers: []corev1.Container{{Name: "setup"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "engine",
				RestartCount: 1,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
					StartedAt: metav1.NewTime(started),
				}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					StartedAt:  metav1.NewTime(previousStarted),
					FinishedAt: metav1.NewTime(started.Add(-5 * time.Second)),
					ExitCode:   1,
				}},
			}},
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "setup",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					StartedAt:  metav1.NewTime(initStarted),
					FinishedAt: metav1.NewTime(initStarted.Add(2 * time.Second)),
					ExitCode:   0,
				}},
			}},
		},
	}
	responses := map[string]string{
		"engine":          timestampedLog(started.Add(2*time.Second), "engine ready"),
		"engine/previous": timestampedLog(previousStarted.Add(2*time.Second), "previous engine ready"),
		"setup":           timestampedLog(initStarted.Add(time.Second), "setup complete"),
	}
	client, fakeClient := newTestLogClient(pod, responses)
	var bundle model.Bundle
	collectLogs(context.Background(), client, pod, Options{IncludePrevious: true}, &bundle)

	if len(bundle.Logs) != 3 {
		t.Fatalf("len(bundle.Logs) = %d, want current, previous, and init logs: %#v", len(bundle.Logs), bundle.Logs)
	}
	if bundle.Logs[0].Container != "engine" || bundle.Logs[0].Previous || bundle.Logs[0].Lines[0].Text != "engine ready" {
		t.Errorf("unexpected current engine capture: %#v", bundle.Logs[0])
	}
	if bundle.Logs[1].Container != "engine" || !bundle.Logs[1].Previous || bundle.Logs[1].Lines[0].Text != "previous engine ready" {
		t.Errorf("unexpected previous engine capture: %#v", bundle.Logs[1])
	}
	if bundle.Logs[2].Container != "setup" || bundle.Logs[2].Previous || bundle.Logs[2].Lines[0].Text != "setup complete" {
		t.Errorf("unexpected init capture: %#v", bundle.Logs[2])
	}
	if len(bundle.Notes) != 0 {
		t.Fatalf("unexpected notes: %v", bundle.Notes)
	}

	options := recordedLogOptions(t, fakeClient.Actions())
	assertLogSinceTime(t, options["engine"], started.Add(-5*time.Second))
	assertLogSinceTime(t, options["engine/previous"], previousStarted.Add(-5*time.Second))
	assertLogSinceTime(t, options["setup"], initStarted.Add(-5*time.Second))
	if options["engine"].Previous || !options["engine/previous"].Previous || options["setup"].Previous {
		t.Fatalf("unexpected previous flags: %#v", options)
	}
	for name, got := range options {
		if got.LimitBytes == nil || *got.LimitBytes != DefaultMaxLogBytes {
			t.Errorf("%s LimitBytes = %v, want %d", name, got.LimitBytes, DefaultMaxLogBytes)
		}
		if !got.Timestamps {
			t.Errorf("%s Timestamps = false, want true", name)
		}
	}
}

func TestCollectLogsDetectsRotatedFirstTimestamp(t *testing.T) {
	started := time.Date(2026, 11, 27, 2, 41, 9, 0, time.UTC)
	previousStarted := started.Add(-time.Minute)
	wantWarning := "startup logs appear rotated away (first line at +45s); run coldstart soon after startup or use a log backend"

	for _, previous := range []bool{false, true} {
		name := "current"
		if previous {
			name = "previous"
		}
		t.Run(name, func(t *testing.T) {
			attemptStart := started
			if previous {
				attemptStart = previousStarted
			}
			status := corev1.ContainerStatus{
				Name: "engine",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
					StartedAt: metav1.NewTime(started),
				}},
			}
			if previous {
				status.RestartCount = 1
				status.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
					StartedAt:  metav1.NewTime(previousStarted),
					FinishedAt: metav1.NewTime(started.Add(-time.Second)),
					ExitCode:   1,
				}
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "engine", Namespace: "test"},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "engine"}}},
				Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{status}},
			}
			lateFirst := attemptStart.Add(45 * time.Second)
			lateLogs := "not timestamped\n" + timestampedLog(lateFirst, "finally started")
			responses := map[string]string{"engine": timestampedLog(started.Add(time.Second), "ready")}
			if previous {
				responses["engine/previous"] = lateLogs
			} else {
				responses["engine"] = lateLogs
			}
			client, _ := newTestLogClient(pod, responses)
			var bundle model.Bundle
			collectLogs(context.Background(), client, pod, Options{IncludePrevious: previous, MaxLogBytes: 4096}, &bundle)

			if got := countNote(bundle.Notes, wantWarning); got != 1 {
				t.Fatalf("rotation warning count = %d, want 1; notes: %v", got, bundle.Notes)
			}
			var capture model.LogCapture
			for _, got := range bundle.Logs {
				if got.Previous == previous {
					capture = got
					break
				}
			}
			if capture.Container != "engine" || len(capture.Lines) != 2 {
				t.Fatalf("unexpected rotated capture: %#v", capture)
			}
			if capture.Truncated {
				t.Fatal("small log unexpectedly marked truncated")
			}
			if !capture.Lines[0].Timestamp.IsZero() || capture.Lines[0].Text != "not timestamped" {
				t.Errorf("malformed first line was not preserved: %#v", capture.Lines[0])
			}
			if !capture.Lines[1].Timestamp.Equal(lateFirst) || capture.Lines[1].Text != "finally started" {
				t.Errorf("first valid line = %#v, want timestamp %s", capture.Lines[1], lateFirst)
			}

			prefix := "logs.engine.current"
			if previous {
				prefix = "logs.engine.previous"
			}
			if got := bundle.Metadata[prefix+".attemptStart"]; got != attemptStart.Format(time.RFC3339Nano) {
				t.Errorf("attemptStart metadata = %q, want %q", got, attemptStart.Format(time.RFC3339Nano))
			}
			if got := bundle.Metadata[prefix+".firstLine"]; got != lateFirst.Format(time.RFC3339Nano) {
				t.Errorf("firstLine metadata = %q, want %q", got, lateFirst.Format(time.RFC3339Nano))
			}
			if got := bundle.Metadata[prefix+".startupGapSecs"]; got != "45" {
				t.Errorf("startupGapSecs metadata = %q, want 45", got)
			}
		})
	}
}

func TestCollectLogsStopsAtCap(t *testing.T) {
	started := time.Date(2026, 11, 27, 2, 41, 9, 0, time.UTC)
	first := timestampedLog(started.Add(time.Second), "first")
	second := timestampedLog(started.Add(2*time.Second), "second line")
	secondTextOffset := strings.Index(second, "second ")
	cap := int64(len(first) + secondTextOffset + len("second "))
	body := first + second + timestampedLog(started.Add(3*time.Second), "must not be captured")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "engine", Namespace: "test"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "engine"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "engine",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
				StartedAt: metav1.NewTime(started),
			}},
		}},
		},
	}
	client, fakeClient := newTestLogClient(pod, map[string]string{"engine": body})
	var bundle model.Bundle
	collectLogs(context.Background(), client, pod, Options{MaxLogBytes: cap}, &bundle)

	if len(bundle.Logs) != 1 {
		t.Fatalf("len(bundle.Logs) = %d, want 1", len(bundle.Logs))
	}
	capture := bundle.Logs[0]
	if !capture.Truncated {
		t.Fatal("capture.Truncated = false, want true")
	}
	if len(capture.Lines) != 2 || capture.Lines[0].Text != "first" || capture.Lines[1].Text != "second " {
		t.Fatalf("captured lines did not stop at cap: %#v", capture.Lines)
	}
	options := recordedLogOptions(t, fakeClient.Actions())
	if options["engine"].LimitBytes == nil || *options["engine"].LimitBytes != cap {
		t.Fatalf("LimitBytes = %v, want %d", options["engine"].LimitBytes, cap)
	}
	wantNote := fmt.Sprintf("logs for container engine reached the %d-byte cap; output was truncated", cap)
	if got := countNote(bundle.Notes, wantNote); got != 1 {
		t.Fatalf("cap note count = %d, want 1; notes: %v", got, bundle.Notes)
	}
}

func TestCollectLogsDefaultsNonPositiveCap(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "engine", Namespace: "test"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "engine"}}},
	}
	for _, configured := range []int64{0, -1} {
		t.Run(fmt.Sprintf("max_%d", configured), func(t *testing.T) {
			client := fake.NewSimpleClientset(pod.DeepCopy())
			var bundle model.Bundle
			collectLogs(context.Background(), client, pod, Options{MaxLogBytes: configured}, &bundle)
			options := recordedLogOptions(t, client.Actions())
			if options["engine"].LimitBytes == nil || *options["engine"].LimitBytes != DefaultMaxLogBytes {
				t.Fatalf("LimitBytes = %v, want default %d", options["engine"].LimitBytes, DefaultMaxLogBytes)
			}
		})
	}
}

func TestCollectLogsSeparatesInitAndMainStatusWithSameName(t *testing.T) {
	mainStart := time.Date(2026, 11, 27, 2, 41, 9, 0, time.UTC)
	initStart := mainStart.Add(-time.Minute)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "engine", Namespace: "test"},
		Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "engine"}},
			InitContainers: []corev1.Container{{Name: "engine"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{Name: "engine", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(mainStart)}}}},
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "engine", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				StartedAt: metav1.NewTime(initStart), FinishedAt: metav1.NewTime(mainStart),
			}}}},
		},
	}
	client, fakeClient := newTestLogClient(pod, map[string]string{"engine": timestampedLog(mainStart, "main")})
	var bundle model.Bundle
	collectLogs(context.Background(), client, pod, Options{}, &bundle)
	if len(bundle.Logs) != 2 || bundle.Logs[0].Init || !bundle.Logs[1].Init {
		t.Fatalf("log captures = %+v, want main then init", bundle.Logs)
	}
	var since []time.Time
	for _, action := range fakeClient.Actions() {
		if action.GetSubresource() != "log" {
			continue
		}
		opts := action.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		since = append(since, opts.SinceTime.Time)
	}
	if len(since) != 2 || !since[0].Equal(mainStart.Add(-5*time.Second)) || !since[1].Equal(initStart.Add(-5*time.Second)) {
		t.Fatalf("SinceTime values = %v, want separate main/init attempt bounds", since)
	}
}

func timestampedLog(at time.Time, text string) string {
	return at.UTC().Format(time.RFC3339Nano) + " " + text + "\n"
}

// client-go's generated fake GetLogs always returns "fake logs". These small
// wrappers keep the fake clientset and its action history while supplying the
// stream bodies needed to exercise collection behavior.
type testLogClient struct {
	kubernetes.Interface
	core typedcorev1.CoreV1Interface
}

func (c testLogClient) CoreV1() typedcorev1.CoreV1Interface {
	return c.core
}

type testLogCore struct {
	typedcorev1.CoreV1Interface
	pods typedcorev1.PodInterface
}

func (c testLogCore) Pods(string) typedcorev1.PodInterface {
	return c.pods
}

type testLogPods struct {
	typedcorev1.PodInterface
	namespace string
	responses map[string]string
}

func (p *testLogPods) GetLogs(name string, opts *corev1.PodLogOptions) *rest.Request {
	// The generated fake records the action, including PodLogOptions, before it
	// constructs its fixed-response request. That request is intentionally unused.
	p.PodInterface.GetLogs(name, opts)
	key := opts.Container
	if opts.Previous {
		key += "/previous"
	}
	body := p.responses[key]
	fakeREST := &fakerest.RESTClient{
		NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		GroupVersion:         corev1.SchemeGroupVersion,
		VersionedAPIPath:     fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", p.namespace, name),
		Client: fakerest.CreateHTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}
	return fakeREST.Request().VersionedParams(opts, scheme.ParameterCodec)
}

func newTestLogClient(pod *corev1.Pod, responses map[string]string) (kubernetes.Interface, *fake.Clientset) {
	clientset := fake.NewSimpleClientset(pod.DeepCopy())
	core := clientset.CoreV1()
	pods := core.Pods(pod.Namespace)
	wrappedCore := testLogCore{
		CoreV1Interface: core,
		pods: &testLogPods{
			PodInterface: pods,
			namespace:    pod.Namespace,
			responses:    responses,
		},
	}
	return testLogClient{Interface: clientset, core: wrappedCore}, clientset
}

func recordedLogOptions(t *testing.T, actions []k8stesting.Action) map[string]*corev1.PodLogOptions {
	t.Helper()
	options := make(map[string]*corev1.PodLogOptions)
	for _, action := range actions {
		if action.GetSubresource() != "log" {
			continue
		}
		generic, ok := action.(k8stesting.GenericAction)
		if !ok {
			t.Fatalf("log action has type %T, want GenericAction", action)
		}
		opts, ok := generic.GetValue().(*corev1.PodLogOptions)
		if !ok {
			t.Fatalf("log action value has type %T, want *PodLogOptions", generic.GetValue())
		}
		key := opts.Container
		if opts.Previous {
			key += "/previous"
		}
		if _, exists := options[key]; exists {
			t.Fatalf("duplicate log action for %s", key)
		}
		options[key] = opts
	}
	return options
}

func assertLogSinceTime(t *testing.T, opts *corev1.PodLogOptions, want time.Time) {
	t.Helper()
	if opts == nil {
		t.Fatal("missing log options")
	}
	if opts.SinceTime == nil || !opts.SinceTime.Time.Equal(want) {
		t.Fatalf("SinceTime = %v, want %s", opts.SinceTime, want)
	}
}

func countNote(notes []string, want string) int {
	count := 0
	for _, note := range notes {
		if note == want {
			count++
		}
	}
	return count
}
