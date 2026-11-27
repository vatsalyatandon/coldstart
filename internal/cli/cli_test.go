package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vatsalyatandon/coldstart/internal/bundleio"
	"github.com/vatsalyatandon/coldstart/internal/model"
)

func TestOfflineJSONReplay(t *testing.T) {
	bundlePath := writeGoldenBundle(t, "warm-vllm-cache-hit")
	stdout, stderr, err := execute(t, "--from-bundle", bundlePath, "-o", "json")
	if err != nil {
		t.Fatalf("execute: %v\nstderr: %s", err, stderr)
	}
	var report model.Report
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.Schema != model.ReportSchema || report.Pod != "inference/qwen3" || report.TotalSecs != 200 {
		t.Fatalf("report = %+v", report)
	}
	if report.Engine.Name != "vllm" || len(report.Phases) == 0 {
		t.Fatalf("report missing engine/phases: %+v", report)
	}
}

func TestOfflineMarkdownReplay(t *testing.T) {
	bundlePath := writeGoldenBundle(t, "cold-vllm-cache-miss")
	stdout, _, err := execute(t, "--from-bundle", bundlePath, "-o", "markdown")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# coldstart startup report", "Startup waterfall", "Findings", "weights download"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

func TestBundleOutputRoundTrip(t *testing.T) {
	bundlePath := writeGoldenBundle(t, "warm-vllm-cache-hit")
	stdout, _, err := execute(t, "--from-bundle", bundlePath, "-o", "bundle")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := bundleio.Read(strings.NewReader(stdout))
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Schema != model.BundleSchema || bundle.Pod.Name != "qwen3" {
		t.Fatalf("bundle = %+v", bundle)
	}
}

func TestOfflineProbeRejected(t *testing.T) {
	bundlePath := writeGoldenBundle(t, "warm-vllm-cache-hit")
	_, _, err := execute(t, "--from-bundle", bundlePath, "--probe")
	if err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("error = %v", err)
	}
}

func TestResourceRequired(t *testing.T) {
	_, _, err := execute(t)
	if err == nil || !strings.Contains(err.Error(), "argument is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestCompareBundleOutputRejected(t *testing.T) {
	_, _, err := execute(t, "pod/a", "--compare", "pod/b", "-o", "bundle")
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("error = %v", err)
	}
}

func TestNegativeLogCapRejected(t *testing.T) {
	_, _, err := execute(t, "pod/a", "--max-log-bytes", "-1")
	if err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("error = %v", err)
	}
}

func writeGoldenBundle(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "synthetic", name+".bundle.json")) // #nosec G304 -- fixture names are fixed test cases below testdata/fixtures/synthetic.
	if err != nil {
		t.Fatal(err)
	}
	var bundle model.Bundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := bundleio.Write(&output, bundle); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name+".tgz")
	if err := os.WriteFile(path, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func execute(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewRootCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}
