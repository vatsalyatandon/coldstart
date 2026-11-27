package collect

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
