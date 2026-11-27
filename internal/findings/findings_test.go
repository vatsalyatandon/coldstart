package findings

import (
	"testing"

	"github.com/vatsalyatandon/coldstart/internal/model"
	"github.com/vatsalyatandon/coldstart/internal/rules"
)

func TestFindingsRankByRecoverableSeconds(t *testing.T) {
	report := model.Report{
		TotalSecs: 400,
		Phases: []model.Phase{
			{ID: "framework_bootstrap", Secs: 300},
			{ID: "image_pull", Secs: 100, Attrs: map[string]any{"cached": false}},
		},
	}
	findings := Build(report, rules.Selection{}, nil)
	if len(findings) != 1 || findings[0].Phase != "image_pull" {
		t.Fatalf("findings = %+v", findings)
	}
	if findings[0].EstRecoverableSecs != 75 {
		t.Fatalf("recoverable = %v", findings[0].EstRecoverableSecs)
	}
}

func TestCachedImageDoesNotProducePrepullFinding(t *testing.T) {
	report := model.Report{TotalSecs: 100, Phases: []model.Phase{{ID: "image_pull", Secs: 80, Attrs: map[string]any{"cached": true}}}}
	if findings := Build(report, rules.Selection{}, nil); len(findings) != 0 {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestReadinessRecoversOnlyProbePeriod(t *testing.T) {
	report := model.Report{TotalSecs: 100, Phases: []model.Phase{{ID: "readiness_lag", Secs: 55}}}
	findings := Build(report, rules.Selection{}, nil)
	if len(findings) != 1 || findings[0].EstRecoverableSecs != 25 {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestReadinessUsesConfiguredProbePeriod(t *testing.T) {
	report := model.Report{TotalSecs: 100, Phases: []model.Phase{{ID: "readiness_lag", Secs: 25, Attrs: map[string]any{"probe_period_secs": 10.0}}}}
	findings := Build(report, rules.Selection{}, nil)
	if len(findings) != 1 || findings[0].EstRecoverableSecs != 15 {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestCompileMissAndEagerModeAreRanked(t *testing.T) {
	report := model.Report{TotalSecs: 100, Phases: []model.Phase{{ID: "torch_compile", Secs: 60, Attrs: map[string]any{"cache": "miss"}}}}
	findings := Build(report, rules.Selection{}, nil)
	if len(findings) != 2 {
		t.Fatalf("findings = %+v", findings)
	}
	if findings[0].EstRecoverableSecs != 60 || findings[1].EstRecoverableSecs != 42 {
		t.Fatalf("unexpected ranking/estimates: %+v", findings)
	}
}

func TestLargeCompileWithoutHitMarkerIsMediumConfidenceMiss(t *testing.T) {
	report := model.Report{TotalSecs: 100, Phases: []model.Phase{{ID: "torch_compile", Secs: 60, Attrs: map[string]any{"cache": "unknown"}}}}
	findings := Build(report, rules.Selection{}, nil)
	found := false
	for _, finding := range findings {
		if finding.Condition == "compile cache miss inferred: no cache-hit marker observed" {
			found = true
			if finding.Confidence != "medium" {
				t.Fatalf("finding = %+v", finding)
			}
		}
	}
	if !found {
		t.Fatalf("missing inferred cache-miss finding: %+v", findings)
	}
}
