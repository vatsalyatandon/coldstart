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

func TestEveryShippedRegexHasEvidenceComment(t *testing.T) {
	err := filepath.WalkDir(filepath.Join("..", "..", "rules"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
			return nil
		}
		data, err := os.ReadFile(path) // #nosec G304 -- the rule-pack root is fixed relative to this test.
		if err != nil {
			return err
		}
		lines := strings.Split(string(data), "\n")
		section := ""
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			if indent == 0 && strings.HasSuffix(trimmed, ":") {
				section = strings.TrimSuffix(trimmed, ":")
				continue
			}
			isRegex := strings.HasPrefix(trimmed, "start:") || strings.HasPrefix(trimmed, "end:") || strings.HasPrefix(trimmed, "pattern:")
			isRegex = isRegex || (section == "detect" && strings.HasPrefix(trimmed, "- '"))
			isRegex = isRegex || (section == "flags" && indent == 2 && strings.Contains(trimmed, ": '"))
			if !isRegex {
				continue
			}
			context := line
			for j := i - 1; j >= 0; j-- {
				previous := strings.TrimSpace(lines[j])
				if previous == "" {
					break
				}
				if !strings.HasPrefix(previous, "#") {
					break
				}
				context += "\n" + previous
			}
			if !strings.Contains(context, "# src:") && !strings.Contains(context, "# UNVERIFIED") {
				t.Errorf("%s:%d regex has no evidence comment: %s", path, i+1, trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTensorParallelUsesFirstMarkers(t *testing.T) {
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
	if got := *matched.Phases["weights_load"].ReportedSecs; got != 5.71 {
		t.Fatalf("got %v, want first worker marker 5.71", got)
	}
}

func TestRepeatedGraphMarkersUseFirstStartAndFirstEnd(t *testing.T) {
	packs, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 27, 2, 41, 9, 0, time.UTC)
	lines := []model.LogLine{
		{Timestamp: base.Add(10 * time.Second), Text: "Graph capturing finished in 10 secs"},
		{Timestamp: base.Add(6 * time.Second), Text: "Graph capturing"},
		{Timestamp: base.Add(5 * time.Second), Text: "Graph capturing finished in 5 secs"},
		{Timestamp: base, Text: "Graph capturing"},
	}
	selection, _ := Select(packs, Detection{Engine: "vllm", Version: "0.30.0"})
	matched := Match(lines, selection.Pack)
	phase := matched.Phases["cuda_graph"]
	if phase.Start == nil || !phase.Start.Equal(base) || phase.End == nil || !phase.End.Equal(base.Add(5*time.Second)) || *phase.ReportedSecs != 5 {
		t.Fatalf("phase = %+v, want first start/end pair", phase)
	}
}

func TestRiskyLinesDoNotMatchDownloadOrCompile(t *testing.T) {
	packs, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	selection, _ := Select(packs, Detection{Engine: "vllm", Version: "0.30.0"})
	lines := []model.LogLine{
		{Timestamp: time.Unix(1, 0), Text: "Fetching tokenizer config from https://huggingface.co/Qwen/test"},
		{Timestamp: time.Unix(2, 0), Text: "Loading safetensors checkpoint shards: 50%"},
		{Timestamp: time.Unix(3, 0), Text: `127.0.0.1 - "POST /v1/completions" 200 Compiling`},
		{Timestamp: time.Unix(4, 0), Text: "Loading weights took 1.00 seconds"},
	}
	matched := Match(lines, selection.Pack)
	if matched.Flags["weights_remote_download"] {
		t.Fatal("tokenizer-only HF fetch was classified as a remote weights download")
	}
	if _, ok := matched.Phases["weights_download"]; ok {
		t.Fatal("tokenizer/load-progress lines were classified as weights download")
	}
	if _, ok := matched.Phases["torch_compile"]; ok {
		t.Fatal("request log containing Compiling was classified as torch.compile")
	}
	if matched.Phases["weights_load"].Start == nil {
		t.Fatal("checkpoint-shard progress was not retained as a load-start signal")
	}
}

func TestStartingServerIsDetectionOnly(t *testing.T) {
	packs, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	selection, _ := Select(packs, Detection{Engine: "vllm", Version: "0.30.0"})
	matched := Match([]model.LogLine{{Timestamp: time.Unix(1, 0), Text: "Starting vLLM API server on http://0.0.0.0:8000"}}, selection.Pack)
	if _, ok := matched.Phases["server_ready"]; ok {
		t.Fatal("Starting vLLM server was incorrectly treated as readiness")
	}
}
