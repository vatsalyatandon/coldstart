package findings

import (
	"math"
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

func TestCompileCacheMissRanksAboveEagerModeByWeightedRecoverableSeconds(t *testing.T) {
	report := model.Report{TotalSecs: 100, Phases: []model.Phase{{ID: "torch_compile", Secs: 120, Attrs: map[string]any{"cache": "miss"}}}}
	findings := Build(report, rules.Selection{}, nil)
	if len(findings) != 2 {
		t.Fatalf("findings = %+v", findings)
	}
	if findings[0].Condition != "compile cache miss" || findings[0].Confidence != "high" || findings[0].EstRecoverableSecs != 84 {
		t.Fatalf("cache miss should rank first with 84 weighted seconds: %+v", findings[0])
	}
	if findings[1].Condition != "large compile phase and latency-tolerant workload" || findings[1].Confidence != "low" || findings[1].EstRecoverableSecs != 120 {
		t.Fatalf("unexpected eager-mode finding: %+v", findings[1])
	}
	if findings[1].Tradeoff != "lower throughput" {
		t.Fatalf("eager-mode tradeoff = %q, want %q", findings[1].Tradeoff, "lower throughput")
	}
}

func TestFindingsRankingPreservesTieBreaks(t *testing.T) {
	findings := []model.Finding{
		{Phase: "z_phase", ObservedSecs: 10, EstRecoverableSecs: 10, Confidence: "high"},
		{Phase: "b_phase", ObservedSecs: 20, EstRecoverableSecs: 10, Confidence: "high"},
		{Phase: "a_phase", ObservedSecs: 20, EstRecoverableSecs: 10, Confidence: "high"},
	}
	sortFindings(findings)
	for i, want := range []string{"a_phase", "b_phase", "z_phase"} {
		if findings[i].Phase != want {
			t.Fatalf("findings[%d].Phase = %q, want %q: %+v", i, findings[i].Phase, want, findings)
		}
	}
}

func TestPhaseTotalRecoverableUsesLargestWeightedFinding(t *testing.T) {
	findings := []model.Finding{
		{Phase: "torch_compile", EstRecoverableSecs: 120, Confidence: "low"},
		{Phase: "torch_compile", EstRecoverableSecs: 100, Confidence: "medium"},
		{Phase: "torch_compile", EstRecoverableSecs: 84, Confidence: "high"},
		{Phase: "image_pull", EstRecoverableSecs: 1000, Confidence: "high"},
	}
	if got := PhaseTotalRecoverable(findings, "torch_compile"); got != 84 {
		t.Fatalf("PhaseTotalRecoverable(torch_compile) = %v, want 84", got)
	}
	if got := PhaseTotalRecoverable(findings, "image_pull"); got != 1000 {
		t.Fatalf("PhaseTotalRecoverable(image_pull) = %v, want 1000", got)
	}
	if got := PhaseTotalRecoverable(findings, "missing"); got != 0 {
		t.Fatalf("PhaseTotalRecoverable(missing) = %v, want 0", got)
	}
}

func TestPhaseTotalRecoverableAppliesConfidenceWeights(t *testing.T) {
	for _, test := range []struct {
		confidence string
		want       float64
	}{
		{confidence: "high", want: 120},
		{confidence: "medium", want: 72},
		{confidence: "low", want: 36},
	} {
		t.Run(test.confidence, func(t *testing.T) {
			findings := []model.Finding{{Phase: "weights_load", EstRecoverableSecs: 120, Confidence: test.confidence}}
			if got := PhaseTotalRecoverable(findings, "weights_load"); got != test.want {
				t.Fatalf("PhaseTotalRecoverable(%s) = %v, want %v", test.confidence, got, test.want)
			}
		})
	}
}

func TestPhaseTotalRecoverableIgnoresInvalidEstimatesAndUnknownConfidence(t *testing.T) {
	findings := []model.Finding{
		{Phase: "weights_load", EstRecoverableSecs: -10, Confidence: "high"},
		{Phase: "weights_load", EstRecoverableSecs: math.NaN(), Confidence: "high"},
		{Phase: "weights_load", EstRecoverableSecs: math.Inf(1), Confidence: "high"},
		{Phase: "weights_load", EstRecoverableSecs: 100, Confidence: "unknown"},
	}
	if got := PhaseTotalRecoverable(findings, "weights_load"); got != 0 {
		t.Fatalf("PhaseTotalRecoverable(weights_load) = %v, want 0", got)
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
