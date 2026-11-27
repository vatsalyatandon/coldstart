package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

func TestLoadEmbeddedPacks(t *testing.T) {
	packs, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) < 3 {
		t.Fatalf("got %d packs, want at least 3", len(packs))
	}
}

func TestVLLMMarkersFromPublicLogs(t *testing.T) {
	packs, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 27, 2, 41, 9, 0, time.UTC)
	lines := []model.LogLine{
		{Timestamp: base, Text: "vLLM API server version 0.30.0"},
		{Timestamp: base.Add(10 * time.Second), Text: "INFO [loader.py:429] Loading weights took 6.74 seconds"},
		{Timestamp: base.Add(20 * time.Second), Text: "INFO [monitor.py:33] torch.compile takes **7.51 s** in total"},
		{Timestamp: base.Add(30 * time.Second), Text: "INFO [gpu_model_runner.py:3480] Graph capturing finished in 12 secs, took 0.29 GiB"},
		{Timestamp: base.Add(40 * time.Second), Text: "INFO [core.py:210] init engine (profile, create kv cache, warmup model) took 31.64 seconds"},
		{Timestamp: base.Add(50 * time.Second), Text: "INFO: Application startup complete."},
	}
	detection := Detect(model.Bundle{Logs: []model.LogCapture{{Container: "engine", Lines: lines}}}, packs)
	if detection.Engine != "vllm" || detection.Version != "0.30.0" {
		t.Fatalf("unexpected detection: %+v", detection)
	}
	selection, ok := Select(packs, detection)
	if !ok || selection.Warning != "" {
		t.Fatalf("unexpected selection: %+v", selection)
	}
	matched := Match(lines, selection.Pack)
	for id, want := range map[string]float64{"weights_load": 6.74, "torch_compile": 7.51, "cuda_graph": 12, "engine_init": 31.64} {
		phase := matched.Phases[id]
		if phase.ReportedSecs == nil || *phase.ReportedSecs != want {
			t.Errorf("%s reported secs = %v, want %v", id, phase.ReportedSecs, want)
		}
	}
	if matched.Phases["server_ready"].End == nil {
		t.Error("server_ready marker not matched")
	}
}

func TestSGLangMarkersFromPublicLogs(t *testing.T) {
	packs, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 27, 2, 41, 9, 0, time.UTC)
	lines := []model.LogLine{
		{Timestamp: base, Text: "(SGLangEngine pid=96824) [2026-01-23 15:16:46] Capture cuda graph end. Time elapsed: 1.64 s. mem usage=0.11 GB. avail mem=53.41 GB."},
		{Timestamp: base.Add(time.Second), Text: "[2026-01-23 15:16:46] The server is fired up and ready to roll!"},
	}
	detection := Detect(model.Bundle{Logs: []model.LogCapture{{Container: "engine", Lines: lines}}}, packs)
	if detection.Engine != "sglang" {
		t.Fatalf("engine = %q", detection.Engine)
	}
	selection, ok := Select(packs, detection)
	if !ok {
		t.Fatal("no SGLang pack")
	}
	matched := Match(lines, selection.Pack)
	if phase := matched.Phases["cuda_graph"]; phase.ReportedSecs == nil || *phase.ReportedSecs != 1.64 {
		t.Fatalf("cuda graph = %+v", phase)
	}
	if matched.Phases["server_ready"].End == nil {
		t.Fatal("server ready not matched")
	}
}

func TestNearestPackFallbackWarns(t *testing.T) {
	packs, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	selection, ok := Select(packs, Detection{Engine: "vllm", Version: "0.50.0"})
	if !ok || selection.Warning == "" || !strings.Contains(selection.Warning, "not found") {
		t.Fatalf("selection = %+v, ok=%v", selection, ok)
	}
	if selection.Pack.Versions != ">=0.11,<0.40" {
		t.Fatalf("nearest pack = %q, want modern vLLM pack", selection.Pack.Versions)
	}
}

func TestCustomPackOverridesEmbeddedConstraint(t *testing.T) {
	dir := t.TempDir()
	manifest := []byte("engine: vllm\nversions: '=0.30.0'\ndetect: ['CUSTOM vLLM (?P<version>\\S+)']\nphases:\n  server_ready:\n    end: 'CUSTOM READY'\n")
	if err := os.WriteFile(filepath.Join(dir, "custom.yaml"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	packs, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	selection, ok := Select(packs, Detection{Engine: "vllm", Version: "0.30.0"})
	if !ok || selection.Pack.Path != filepath.Join(dir, "custom.yaml") {
		t.Fatalf("selection = %+v, ok=%v", selection, ok)
	}
}

func TestTensorParallelUsesSlowestReportedDuration(t *testing.T) {
	packs, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 27, 2, 41, 9, 0, time.UTC)
	lines := []model.LogLine{
		{Timestamp: base, Text: "(Worker_TP0 pid=1) Loading weights took 5.71 seconds"},
		{Timestamp: base.Add(time.Second), Text: "(Worker_TP1 pid=2) Loading weights took 7.10 seconds"},
	}
	selection, _ := Select(packs, Detection{Engine: "vllm", Version: "0.30.0"})
	matched := Match(lines, selection.Pack)
	if got := *matched.Phases["weights_load"].ReportedSecs; got != 7.10 {
		t.Fatalf("got %v, want slowest worker 7.10", got)
	}
}
