package analyze

import (
	"math"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

var fixtureT0 = time.Date(2026, 11, 27, 2, 41, 9, 0, time.UTC)

func TestColdVLLMBundleAccountingAndFindings(t *testing.T) {
	bundle := coldVLLMBundle()
	bundle.CapturedAt = fixtureT0.Add(10 * time.Minute)
	report, err := Analyze(bundle, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertAccounting(t, report)
	if report.Engine.Name != "vllm" || report.Engine.Version != "0.30.0" {
		t.Fatalf("engine = %+v", report.Engine)
	}
	if report.Hardware.Count != 1 || report.Hardware.GPU != "NVIDIA H100 80GB HBM3" || !report.Hardware.NodeFresh {
		t.Fatalf("hardware = %+v", report.Hardware)
	}
	want := map[string]float64{
		"scheduling_wait": 26, "node_provisioning": 36, "image_pull": 118,
		"init_containers": 171, "framework_bootstrap": 9, "weights_download": 142,
		"weights_load": 29, "torch_compile": 53, "cuda_graph": 21,
		"kv_cache_warmup": 12, "api_server_up": 24, "readiness_lag": 24,
	}
	for id, secs := range want {
		if got := sumPhase(report.Phases, id); math.Abs(got-secs) > 0.01 {
			t.Errorf("%s = %.3f, want %.3f", id, got, secs)
		}
	}
	if len(report.Findings) == 0 || report.Findings[0].Phase != "weights_download" {
		t.Fatalf("findings not ranked by recoverable seconds: %+v", report.Findings)
	}
	if got := sumPhase(report.Phases, "unattributed"); got != 0 {
		t.Fatalf("unexpected unattributed %.3fs", got)
	}
}

func TestWarmNodeCacheHitHasNoMissFinding(t *testing.T) {
	bundle := warmBundle()
	report, err := Analyze(bundle, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertAccounting(t, report)
	if got := sumPhase(report.Phases, "image_pull"); got != 0 {
		t.Fatalf("warm image_pull = %v", got)
	}
	compilePhase := findPhase(report.Phases, "torch_compile")
	if compilePhase == nil || compilePhase.Attrs["cache"] != "hit" {
		t.Fatalf("compile phase = %+v", compilePhase)
	}
	for _, finding := range report.Findings {
		if finding.Condition == "compile cache miss" {
			t.Fatal("cache-hit run produced a cache-miss finding")
		}
	}
}

func TestCrashLoopAttributesLostAttempt(t *testing.T) {
	bundle := crashBundle()
	report, err := Analyze(bundle, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertAccounting(t, report)
	if len(report.Attempts) != 2 {
		t.Fatalf("attempts = %+v", report.Attempts)
	}
	if report.Attempts[0].CrashReason != "OOMKilled" || report.Attempts[0].LostSecs != 70 {
		t.Fatalf("failed attempt = %+v", report.Attempts[0])
	}
	if got := sumPhase(report.Phases, "restart_attempt"); got != 70 {
		t.Fatalf("restart phase = %v", got)
	}
	found := false
	for _, finding := range report.Findings {
		if finding.Phase == "restart_attempt" && finding.EstRecoverableSecs == 70 {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing crash finding: %+v", report.Findings)
	}
}

func TestExpiredEventsWarnAndUseDurableStatus(t *testing.T) {
	bundle := warmBundle()
	bundle.Events = nil
	bundle.CapturedAt = fixtureT0.Add(2 * time.Hour)
	report, err := Analyze(bundle, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertAccounting(t, report)
	expired := false
	for _, warning := range report.Warnings {
		if strings.Contains(warning, "events older than 1h may have expired") {
			expired = true
		}
	}
	if !expired {
		t.Fatalf("warnings = %v", report.Warnings)
	}
	if got := sumPhase(report.Phases, "scheduling_wait"); got != 10 {
		t.Fatalf("scheduling wait = %v", got)
	}
}

func TestClockSkewNeverProducesNegativePhase(t *testing.T) {
	bundle := warmBundle()
	start := fixtureT0.Add(-time.Minute)
	bundle.Pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(start)
	bundle.Logs[0].Lines[0].Timestamp = fixtureT0.Add(-30 * time.Second)
	report, err := Analyze(bundle, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertAccounting(t, report)
	for _, phase := range report.Phases {
		if phase.Secs < 0 || phase.End.Before(*phase.Start) {
			t.Fatalf("negative phase: %+v", phase)
		}
	}
}

func TestSGLangTimeline(t *testing.T) {
	bundle := sglangBundle()
	report, err := Analyze(bundle, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertAccounting(t, report)
	if report.Engine.Name != "sglang" {
		t.Fatalf("engine = %+v", report.Engine)
	}
	if got := sumPhase(report.Phases, "cuda_graph"); got != 3 {
		t.Fatalf("cuda graph = %v", got)
	}
}

func TestMissingCaptureTimeIsDeterministic(t *testing.T) {
	bundle := warmBundle()
	bundle.CapturedAt = time.Time{}
	first, err := Analyze(bundle, Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Analyze(bundle, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !first.CapturedAt.Equal(*first.ReadyAt) || !first.CapturedAt.Equal(second.CapturedAt) {
		t.Fatalf("capture times are not deterministic: %v, %v", first.CapturedAt, second.CapturedAt)
	}
}

func TestOptionalProbeExtendsTimeline(t *testing.T) {
	bundle := warmBundle()
	request := fixtureT0.Add(200 * time.Second)
	first := request.Add(400 * time.Millisecond)
	bundle.Probe = &model.ProbeResult{RequestSentAt: request, FirstTokenAt: first}
	report, err := Analyze(bundle, Options{})
	if err != nil {
		t.Fatal(err)
	}
	assertAccounting(t, report)
	if got := sumPhase(report.Phases, "first_token"); math.Abs(got-0.4) > 0.001 {
		t.Fatalf("first token = %v", got)
	}
}

func coldVLLMBundle() model.Bundle {
	bundle := baseBundle(665)
	bundle.Node = nodeFixture(5, 41)
	bundle.Events = []corev1.Event{
		{Reason: "TriggeredScaleUp", Message: "Triggered scale up", EventTime: metav1.NewMicroTime(fixtureT0.Add(5 * time.Second))},
		{Reason: "Pulling", Message: `Pulling image "vllm/vllm-openai:v0.30.0"`, EventTime: metav1.NewMicroTime(fixtureT0.Add(62 * time.Second))},
		{Reason: "Pulled", Message: `Successfully pulled image "vllm/vllm-openai:v0.30.0" in 1m58s (11.2 GB)`, EventTime: metav1.NewMicroTime(fixtureT0.Add(180 * time.Second))},
	}
	bundle.Pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "weights", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: metav1.NewTime(fixtureT0.Add(180 * time.Second)), FinishedAt: metav1.NewTime(fixtureT0.Add(351 * time.Second)), ExitCode: 0}},
	}}
	bundle.Pod.Spec.InitContainers = []corev1.Container{{Name: "weights", Args: []string{"snapshot_download", "https://huggingface.co/Qwen/test"}}}
	bundle.Pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(fixtureT0.Add(351 * time.Second))
	bundle.Logs = []model.LogCapture{{Container: "engine", Lines: []model.LogLine{
		at(351, "vLLM API server version 0.30.0"),
		at(360, "Downloading model shards from huggingface.co"),
		at(502, "Loading weights"),
		at(531, "Loading weights took 29 seconds"),
		at(531, "torch.compile cache miss; Compiling"),
		at(584, "torch.compile takes **53.00 s** in total"),
		at(584, "Graph capturing"),
		at(605, "Graph capturing finished in 21 secs, took 1.17 GiB"),
		at(617, "init engine (profile, create kv cache, warmup model) took 12 seconds"),
		at(641, "Application startup complete"),
	}}}
	return bundle
}

func warmBundle() model.Bundle {
	bundle := baseBundle(200)
	bundle.Pod.Status.Conditions[0].LastTransitionTime = metav1.NewTime(fixtureT0.Add(10 * time.Second))
	bundle.Node = nodeFixture(-3600, -3500)
	bundle.Pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(fixtureT0.Add(30 * time.Second))
	bundle.Logs = []model.LogCapture{{Container: "engine", Lines: []model.LogLine{
		at(30, "vLLM API server version 0.30.0"),
		at(30, "Loading weights"),
		at(60, "Loading weights took 30 seconds"),
		at(60, "Directly load the compiled graph from the cache"),
		at(90, "torch.compile takes **30 s** in total"),
		at(90, "Graph capturing"),
		at(100, "Graph capturing finished in 10 secs"),
		at(110, "init engine (profile, create kv cache, warmup model) took 10 seconds"),
		at(120, "Application startup complete"),
	}}}
	return bundle
}

func crashBundle() model.Bundle {
	bundle := baseBundle(200)
	bundle.Pod.Status.Conditions[0].LastTransitionTime = metav1.NewTime(fixtureT0.Add(10 * time.Second))
	bundle.Node = nodeFixture(-3600, -3500)
	status := &bundle.Pod.Status.ContainerStatuses[0]
	status.RestartCount = 1
	status.State.Running.StartedAt = metav1.NewTime(fixtureT0.Add(100 * time.Second))
	status.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{StartedAt: metav1.NewTime(fixtureT0.Add(20 * time.Second)), FinishedAt: metav1.NewTime(fixtureT0.Add(90 * time.Second)), ExitCode: 137, Reason: "OOMKilled"}
	bundle.Logs = []model.LogCapture{
		{Container: "engine", Lines: []model.LogLine{at(100, "vLLM API server version 0.30.0"), at(100, "Loading weights"), at(130, "Loading weights took 30 seconds"), at(160, "Application startup complete")}},
		{Container: "engine", Previous: true, Lines: []model.LogLine{at(20, "vLLM API server version 0.30.0"), at(80, "CUDA out of memory")}},
	}
	return bundle
}

func sglangBundle() model.Bundle {
	bundle := baseBundle(80)
	bundle.Pod.Status.Conditions[0].LastTransitionTime = metav1.NewTime(fixtureT0.Add(10 * time.Second))
	bundle.Node = nodeFixture(-3600, -3500)
	bundle.Pod.Spec.Containers[0].Name = "sglang"
	bundle.Pod.Spec.Containers[0].Image = "lmsysorg/sglang:v0.4.0"
	bundle.Pod.Status.ContainerStatuses[0].Name = "sglang"
	bundle.Pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(fixtureT0.Add(10 * time.Second))
	bundle.Logs = []model.LogCapture{{Container: "sglang", Lines: []model.LogLine{
		at(10, "(SGLangEngine pid=1)"),
		at(20, "Load weight start"),
		at(40, "Load weight end. elapsed=20.00 s, type=Qwen3ForCausalLM"),
		at(40, "Capture cuda graph start"),
		at(43, "Capture cuda graph end. Time elapsed: 3.00 s."),
		at(45, "max_total_num_tokens=712400"),
		at(50, "The server is fired up and ready to roll!"),
	}}}
	return bundle
}

func baseBundle(total int) model.Bundle {
	totalDuration := time.Duration(total) * time.Second
	return model.Bundle{
		Schema: model.BundleSchema, Source: "fixture", CapturedAt: fixtureT0.Add(totalDuration),
		Pod: corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "qwen3", Namespace: "inference", CreationTimestamp: metav1.NewTime(fixtureT0)},
			Spec:       corev1.PodSpec{NodeName: "gpu-node-1", Containers: []corev1.Container{{Name: "engine", Image: "vllm/vllm-openai:v0.30.0"}}},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(fixtureT0.Add(62 * time.Second))},
					{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(fixtureT0.Add(totalDuration))},
				},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "engine", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
			},
		},
	}
}

func nodeFixture(created, ready int) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1", CreationTimestamp: metav1.NewTime(fixtureT0.Add(time.Duration(created) * time.Second)), Labels: map[string]string{"nvidia.com/gpu.product": "NVIDIA H100 80GB HBM3"}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(fixtureT0.Add(time.Duration(ready) * time.Second))}},
		},
	}
}

func at(seconds int, text string) model.LogLine {
	return model.LogLine{Timestamp: fixtureT0.Add(time.Duration(seconds) * time.Second), Text: text}
}

func assertAccounting(t *testing.T, report model.Report) {
	t.Helper()
	var sum float64
	for _, phase := range report.Phases {
		sum += phase.Secs
	}
	if math.Abs(sum-report.TotalSecs) > 0.01 {
		t.Fatalf("phase sum %.6f != total %.6f; phases=%+v", sum, report.TotalSecs, report.Phases)
	}
	if math.Abs(report.UnattributedSecs-sumPhase(report.Phases, "unattributed")) > 0.001 {
		t.Fatalf("unattributed mismatch: %v", report.UnattributedSecs)
	}
}

func sumPhase(phases []model.Phase, id string) float64 {
	var total float64
	for _, phase := range phases {
		if phase.ID == id {
			total += phase.Secs
		}
	}
	return total
}

func findPhase(phases []model.Phase, id string) *model.Phase {
	for i := range phases {
		if phases[i].ID == id {
			return &phases[i]
		}
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
