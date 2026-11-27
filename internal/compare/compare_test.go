package compare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

func comparisonReports() (model.Report, model.Report) {
	before := model.Report{
		Pod: "inference/before", Engine: model.EngineInfo{Name: "vllm", Version: "0.30.0"},
		Hardware: model.Hardware{GPU: "H100", Count: 1}, TotalSecs: 200, UnattributedSecs: 20,
		Phases: []model.Phase{
			{ID: "z_last", Name: "last", Secs: 10},
			{ID: "shared", Name: "old shared name", Secs: 100},
			{ID: "a_first", Name: "first", Secs: 50},
		},
		Attempts: []model.Attempt{{Index: 0, LostSecs: 30}, {Index: 1, LostSecs: 10}},
		Findings: []model.Finding{
			{Phase: "image_pull", Condition: "cold image", Recommendation: "pre-pull", Text: "old image finding", EstRecoverableSecs: 10, Confidence: "high"},
			{Phase: "weights_load", Condition: "remote weights", Recommendation: "pre-stage", Text: "old weights finding", EstRecoverableSecs: 80, Confidence: "high"},
		},
	}
	after := model.Report{
		Pod: "inference/after", Engine: model.EngineInfo{Name: "vllm", Version: "0.31.0"},
		Hardware: model.Hardware{GPU: "H100", Count: 1}, TotalSecs: 150, UnattributedSecs: 10,
		Phases: []model.Phase{
			{ID: "shared", Name: "new shared name", Secs: 60},
			{ID: "b_new", Name: "new phase", Secs: 30},
			{ID: "a_first", Name: "first", Secs: 40},
		},
		Attempts: []model.Attempt{{Index: 0, LostSecs: 5}},
		Findings: []model.Finding{
			{Phase: "gpu", Condition: "gpu unavailable", Recommendation: "reserve capacity", Text: "new gpu finding", EstRecoverableSecs: 5, Confidence: "medium"},
			{Phase: "weights_load", Condition: "remote weights", Recommendation: "pre-stage", Text: "updated weights finding", EstRecoverableSecs: 90, Confidence: "high"},
		},
	}
	return before, after
}

func TestBuildUsesSortedPhaseUnionAndComputesDeltas(t *testing.T) {
	before, after := comparisonReports()
	got := Build(before, after)

	var ids []string
	for _, phase := range got.Phases {
		ids = append(ids, phase.ID)
	}
	wantIDs := []string{"a_first", "b_new", "shared", "z_last"}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("phase IDs = %v, want %v", ids, wantIDs)
	}

	byID := make(map[string]PhaseDelta)
	for _, phase := range got.Phases {
		byID[phase.ID] = phase
	}
	if phase := byID["shared"]; phase.BeforeSecs != 100 || phase.AfterSecs != 60 || phase.DeltaSecs != -40 || phase.Name != "new shared name" {
		t.Fatalf("shared delta = %+v", phase)
	}
	if phase := byID["b_new"]; !phase.AfterPresent || phase.BeforePresent || phase.DeltaSecs != 30 || phase.PercentChange != nil {
		t.Fatalf("new phase delta = %+v", phase)
	}
	if phase := byID["z_last"]; !phase.BeforePresent || phase.AfterPresent || phase.DeltaSecs != -10 {
		t.Fatalf("removed phase delta = %+v", phase)
	}
	if got.Totals.Startup.DeltaSecs != -50 || got.Totals.Startup.PercentChange == nil || *got.Totals.Startup.PercentChange != -25 {
		t.Fatalf("startup total delta = %+v", got.Totals.Startup)
	}
	if got.Totals.Unattributed.DeltaSecs != -10 || got.Totals.CrashLoss.BeforeSecs != 40 || got.Totals.CrashLoss.AfterSecs != 5 || got.Totals.CrashLoss.DeltaSecs != -35 {
		t.Fatalf("secondary totals = %+v", got.Totals)
	}
}

func TestBuildFallsBackToAttemptPhases(t *testing.T) {
	before := model.Report{Attempts: []model.Attempt{
		{Index: 0, Phases: []model.Phase{{ID: "load", Name: "load", Secs: 10}}},
		{Index: 1, Phases: []model.Phase{{ID: "load", Name: "load", Secs: 20}}},
	}}
	after := model.Report{Attempts: []model.Attempt{
		{Index: 0, Phases: []model.Phase{{ID: "load", Name: "load", Secs: 15}}},
	}}
	got := Build(before, after)
	if len(got.Phases) != 1 || got.Phases[0].BeforeSecs != 30 || got.Phases[0].AfterSecs != 15 || got.Phases[0].DeltaSecs != -15 {
		t.Fatalf("attempt aggregation = %+v", got.Phases)
	}
}

func TestBuildSummariesRankAndMatchFindings(t *testing.T) {
	before, after := comparisonReports()
	got := Build(before, after)
	if got.Findings.BeforeCount != 2 || got.Findings.AfterCount != 2 {
		t.Fatalf("finding counts = %+v", got.Findings)
	}
	if got.Findings.Before[0].Phase != "weights_load" || got.Findings.After[0].Phase != "weights_load" {
		t.Fatalf("findings are not ranked by recoverable seconds: %+v", got.Findings)
	}
	if len(got.Findings.Added) != 1 || got.Findings.Added[0].Phase != "gpu" {
		t.Fatalf("added findings = %+v", got.Findings.Added)
	}
	if len(got.Findings.Removed) != 1 || got.Findings.Removed[0].Phase != "image_pull" {
		t.Fatalf("removed findings = %+v", got.Findings.Removed)
	}
}

func TestComparisonRenderers(t *testing.T) {
	before, after := comparisonReports()
	comparison := Build(before, after)
	for _, format := range []string{FormatText, FormatMarkdown, FormatJSON} {
		t.Run(format, func(t *testing.T) {
			var out bytes.Buffer
			if err := Render(context.Background(), &out, comparison, format); err != nil {
				t.Fatalf("Render(%s): %v", format, err)
			}
			got := out.String()
			for _, want := range []string{"shared", "new phase", "weights_load", "pre-stage", "gpu", "image_pull"} {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q\n%s", want, got)
				}
			}
			if format == FormatText && !strings.Contains(got, "Phase deltas") {
				t.Error("text comparison missing phase table")
			}
			if format == FormatMarkdown && !strings.Contains(got, "| Phase | Before | After | Delta (after - before) |") {
				t.Error("Markdown comparison missing phase table")
			}
			if format == FormatJSON {
				var decoded Comparison
				if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
					t.Fatalf("decode comparison JSON: %v", err)
				}
				if !reflect.DeepEqual(decoded, comparison) {
					t.Fatalf("JSON round trip changed comparison:\n got %+v\nwant %+v", decoded, comparison)
				}
			}
		})
	}
}

func TestComparisonRenderRejectsUnsupportedAndCanceledRequests(t *testing.T) {
	before, after := comparisonReports()
	comparison := Build(before, after)
	var out bytes.Buffer
	if err := Render(context.Background(), &out, comparison, "html"); err == nil {
		t.Fatal("Render(html) succeeded, want unsupported-format error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Render(ctx, &out, comparison, FormatJSON); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Render error = %v, want context.Canceled", err)
	}
}
