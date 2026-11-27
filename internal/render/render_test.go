package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

func sampleReport() model.Report {
	t0 := time.Date(2026, 11, 27, 2, 41, 9, 0, time.UTC)
	ready := t0.Add(200 * time.Second)
	end := ready
	return model.Report{
		Schema: "stale/schema",
		Pod:    "inference/qwen3-35b",
		Engine: model.EngineInfo{Name: "vllm", Version: "0.30.0", Container: "engine"},
		Hardware: model.Hardware{
			GPU: "H100-80GB", Count: 1, Node: "gpu-node-1", NodeFresh: true, Storage: "nvme",
		},
		T0:               t0,
		ReadyAt:          &ready,
		EndAt:            &end,
		TotalSecs:        200,
		UnattributedSecs: 10,
		Phases: []model.Phase{
			{ID: "scheduling", Name: "scheduling wait", Secs: 50, Source: "pod", Confidence: "high"},
			{ID: "node_bringup", Name: "node provisioning", ParentID: "scheduling", Secs: 20, Source: "event", Confidence: "medium"},
			{ID: "image_pull", Name: "image pull", Secs: 100, Source: "event", Confidence: "high", Attrs: map[string]any{"cached": false}},
			{ID: "weights_load", Name: "weights load", Secs: 20, Source: "log", Confidence: "high"},
		},
		Attempts: []model.Attempt{
			{Index: 0, Outcome: "crashed", CrashReason: "OOMKilled", LostSecs: 30, Phases: []model.Phase{{ID: "kv_warmup", Name: "kv cache warmup", Secs: 30}}},
			{Index: 1, Outcome: "ready", Phases: []model.Phase{{ID: "weights_load", Name: "weights load", Secs: 20}}},
		},
		Findings: []model.Finding{
			{
				Phase: "image_pull", ObservedSecs: 100, Condition: "image was not cached",
				Recommendation: "Pre-pull the image", EstRecoverableSecs: 70, Confidence: "high",
				Text: "Image pull dominates cold starts",
			},
			{
				Phase: "weights_load", ObservedSecs: 20, Condition: "weights downloaded at startup",
				Recommendation: "Pre-stage weights on a PVC", EstRecoverableSecs: 100, Confidence: "high",
				Text: "Weights are downloaded during startup",
			},
		},
		Warnings: []string{"events may have expired"},
		RulePack: model.RulePackInfo{
			Engine: "vllm", Version: "0.30", Path: "rules/vllm/0.30.yaml",
			Warning: "rule pack for vllm 0.31 not found; used 0.30 rules",
		},
		CapturedAt: ready,
	}
}

func TestRenderJSONUsesStableReportSchema(t *testing.T) {
	var out bytes.Buffer
	if err := Render(context.Background(), &out, sampleReport(), FormatJSON); err != nil {
		t.Fatalf("Render(json): %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("decode JSON object: %v", err)
	}
	if got := raw["schema"]; got != model.ReportSchema {
		t.Fatalf("schema = %v, want %q", got, model.ReportSchema)
	}

	var decoded model.Report
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("decode model.Report: %v", err)
	}
	if decoded.Pod != "inference/qwen3-35b" || decoded.TotalSecs != 200 {
		t.Fatalf("decoded report lost identity or total: %+v", decoded)
	}
	if len(decoded.Phases) != 4 || len(decoded.Attempts) != 2 || len(decoded.Findings) != 2 {
		t.Fatalf("decoded report lost nested data: %+v", decoded)
	}
	if !strings.Contains(out.String(), "\n  \"schema\": \"coldstart/v1\",\n") {
		t.Fatalf("JSON is not indented as expected:\n%s", out.String())
	}
}

func TestHTMLIsSelfContainedInteractiveAndAccessible(t *testing.T) {
	report := sampleReport()
	report.Findings[0].Text = `<script>alert("unsafe")</script>`
	var out bytes.Buffer
	if err := Render(context.Background(), &out, report, FormatHTML); err != nil {
		t.Fatalf("Render(html): %v", err)
	}
	html := out.String()
	lower := strings.ToLower(html)

	for _, want := range []string{
		"<!doctype html>", `<html lang="en">`, "<style>", "<script>",
		`id="phase-filter"`, `id="attempt-filter"`, `id="confidence-filter"`,
		`data-phase`, `role="status"`, `<th scope="col">`, "Startup timeline", "Findings", "Warnings",
	} {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Errorf("HTML missing %q", want)
		}
	}
	for _, external := range []string{"http://", "https://", "<script src", "<link", "@import", "url("} {
		if strings.Contains(lower, external) {
			t.Errorf("HTML contains external asset marker %q", external)
		}
	}
	if strings.Contains(html, `<script>alert("unsafe")</script>`) {
		t.Fatal("HTML contains unescaped report content")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal("HTML did not escape report content")
	}
}

func TestTextAndMarkdownContainReportDetails(t *testing.T) {
	for _, format := range []string{FormatText, FormatMarkdown} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer
			if err := Render(context.Background(), &out, sampleReport(), format); err != nil {
				t.Fatalf("Render(%s): %v", format, err)
			}
			got := out.String()
			for _, want := range []string{
				"inference/qwen3-35b", "vLLM 0.30.0", "H100-80GB", "total", "3m 20s",
				"unattributed", "Weights are downloaded during startup", "Pre-stage weights on a PVC",
				"events may have expired", "rule pack for vllm 0.31 not found; used 0.30 rules",
				"OOMKilled", "30s", "Attempt",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q\n%s", want, got)
				}
			}
			if strings.Index(got, "Weights are downloaded during startup") > strings.Index(got, "Image pull dominates cold starts") {
				t.Error("findings are not ranked by recoverable seconds")
			}
			if format == FormatText {
				if !strings.Contains(got, "    node provisioning") {
					t.Error("nested phase is not indented")
				}
			} else if !strings.Contains(got, "| Phase | Duration |") || !strings.Contains(got, "| Rank | Phase |") {
				t.Error("Markdown is missing report tables")
			}
		})
	}
}

func TestWaterfallBarsAreProportionalToTotal(t *testing.T) {
	if got, want := waterfallBar(50, 200, 40), strings.Repeat("█", 10); got != want {
		t.Fatalf("50/200 bar = %q, want %q", got, want)
	}
	if got, want := waterfallBar(25, 200, 40), strings.Repeat("█", 5); got != want {
		t.Fatalf("25/200 bar = %q, want %q", got, want)
	}
	if got := waterfallBar(0.4, 200, 40); got != "▏" {
		t.Fatalf("sub-cell bar = %q, want a partial block", got)
	}
	if got := waterfallBar(10, 0, 40); got != "" {
		t.Fatalf("zero-total bar = %q, want empty", got)
	}
}

func TestAttemptOnlyTimelineIncludesUnattributed(t *testing.T) {
	report := sampleReport()
	report.Phases = nil
	report.Attempts = []model.Attempt{{
		Index: 0, Outcome: "ready", Phases: []model.Phase{{ID: "weights_load", Name: "weights load", Secs: 20}},
	}}
	for _, format := range []string{FormatText, FormatMarkdown, FormatHTML} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer
			if err := Render(context.Background(), &out, report, format); err != nil {
				t.Fatalf("Render(%s): %v", format, err)
			}
			if !strings.Contains(out.String(), "unattributed") || !strings.Contains(out.String(), "10s") {
				t.Fatalf("attempt-only %s output omitted unattributed time:\n%s", format, out.String())
			}
		})
	}
}

func TestRenderRejectsUnsupportedAndCanceledRequests(t *testing.T) {
	var out bytes.Buffer
	if err := Render(context.Background(), &out, sampleReport(), "yaml"); err == nil {
		t.Fatal("Render(yaml) succeeded, want unsupported-format error")
	}
	if out.Len() != 0 {
		t.Fatal("unsupported format wrote output")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Render(ctx, &out, sampleReport(), FormatText); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Render error = %v, want context.Canceled", err)
	}
}
