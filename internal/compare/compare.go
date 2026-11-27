// Package compare builds deterministic before/after startup comparisons.
package compare

import (
	"sort"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

// Delta describes an after-minus-before change. PercentChange is nil when the
// before value is zero, because a percentage change is then undefined.
type Delta struct {
	BeforeSecs    float64  `json:"beforeSecs"`
	AfterSecs     float64  `json:"afterSecs"`
	DeltaSecs     float64  `json:"deltaSecs"`
	PercentChange *float64 `json:"percentChange,omitempty"`
}

// Totals contains the overall startup and secondary time totals.
type Totals struct {
	Startup      Delta `json:"startup"`
	Unattributed Delta `json:"unattributed"`
	CrashLoss    Delta `json:"crashLoss"`
}

// ReportSummary identifies the reports being compared without duplicating
// their complete timelines.
type ReportSummary struct {
	Pod              string           `json:"pod"`
	Engine           model.EngineInfo `json:"engine"`
	Hardware         model.Hardware   `json:"hardware"`
	TotalSecs        float64          `json:"totalSecs"`
	UnattributedSecs float64          `json:"unattributedSecs"`
	CrashLossSecs    float64          `json:"crashLossSecs"`
}

// PhaseDelta is one entry in the sorted union of before and after phase IDs.
type PhaseDelta struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ParentID      string   `json:"parentId,omitempty"`
	BeforeSecs    float64  `json:"beforeSecs"`
	AfterSecs     float64  `json:"afterSecs"`
	DeltaSecs     float64  `json:"deltaSecs"`
	PercentChange *float64 `json:"percentChange,omitempty"`
	BeforePresent bool     `json:"beforePresent"`
	AfterPresent  bool     `json:"afterPresent"`
}

// FindingSummary is the stable, renderer-friendly portion of a finding.
type FindingSummary struct {
	Phase           string  `json:"phase"`
	ObservedSecs    float64 `json:"observedSecs"`
	RecoverableSecs float64 `json:"recoverableSecs"`
	Confidence      string  `json:"confidence"`
	Condition       string  `json:"condition,omitempty"`
	Recommendation  string  `json:"recommendation,omitempty"`
	Text            string  `json:"text,omitempty"`
}

// FindingsComparison contains ranked summaries and findings added or removed
// between the two reports. A finding's identity is its phase and diagnostic
// rule (condition plus recommendation), not its measured duration.
type FindingsComparison struct {
	BeforeCount int              `json:"beforeCount"`
	AfterCount  int              `json:"afterCount"`
	Before      []FindingSummary `json:"before"`
	After       []FindingSummary `json:"after"`
	Added       []FindingSummary `json:"added"`
	Removed     []FindingSummary `json:"removed"`
}

// Comparison is the stable result of Build.
type Comparison struct {
	Before   ReportSummary      `json:"before"`
	After    ReportSummary      `json:"after"`
	Totals   Totals             `json:"totals"`
	Phases   []PhaseDelta       `json:"phases"`
	Findings FindingsComparison `json:"findings"`
}

type phaseAggregate struct {
	name     string
	parentID string
	secs     float64
	present  bool
}

// Build compares before with after. Phase IDs are unioned and sorted so the
// result does not depend on map iteration or the input phase ordering. If a
// report has no top-level phases, its attempt phases are aggregated instead.
func Build(before, after model.Report) Comparison {
	beforePhases := aggregatePhases(before)
	afterPhases := aggregatePhases(after)

	idSet := make(map[string]struct{}, len(beforePhases)+len(afterPhases))
	for id := range beforePhases {
		idSet[id] = struct{}{}
	}
	for id := range afterPhases {
		idSet[id] = struct{}{}
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	phases := make([]PhaseDelta, 0, len(ids))
	for _, id := range ids {
		b := beforePhases[id]
		a := afterPhases[id]
		name := a.name
		if name == "" {
			name = b.name
		}
		if name == "" {
			name = id
		}
		parentID := a.parentID
		if parentID == "" {
			parentID = b.parentID
		}
		phases = append(phases, PhaseDelta{
			ID:            id,
			Name:          name,
			ParentID:      parentID,
			BeforeSecs:    b.secs,
			AfterSecs:     a.secs,
			DeltaSecs:     a.secs - b.secs,
			PercentChange: percentChange(b.secs, a.secs),
			BeforePresent: b.present,
			AfterPresent:  a.present,
		})
	}

	beforeSummary := summarizeReport(before)
	afterSummary := summarizeReport(after)
	return Comparison{
		Before: beforeSummary,
		After:  afterSummary,
		Totals: Totals{
			Startup:      makeDelta(before.TotalSecs, after.TotalSecs),
			Unattributed: makeDelta(before.UnattributedSecs, after.UnattributedSecs),
			CrashLoss:    makeDelta(beforeSummary.CrashLossSecs, afterSummary.CrashLossSecs),
		},
		Phases:   phases,
		Findings: compareFindings(before.Findings, after.Findings),
	}
}

func aggregatePhases(report model.Report) map[string]phaseAggregate {
	phases := append([]model.Phase(nil), report.Phases...)
	if len(phases) == 0 {
		for _, attempt := range report.Attempts {
			phases = append(phases, attempt.Phases...)
		}
	}

	aggregates := make(map[string]phaseAggregate)
	for _, phase := range phases {
		aggregate := aggregates[phase.ID]
		aggregate.secs += phase.Secs
		aggregate.present = true
		if aggregate.name == "" {
			aggregate.name = phase.Name
		}
		if aggregate.parentID == "" {
			aggregate.parentID = phase.ParentID
		}
		aggregates[phase.ID] = aggregate
	}
	return aggregates
}

func summarizeReport(report model.Report) ReportSummary {
	crashLoss := 0.0
	for _, attempt := range report.Attempts {
		crashLoss += attempt.LostSecs
	}
	return ReportSummary{
		Pod:              report.Pod,
		Engine:           report.Engine,
		Hardware:         report.Hardware,
		TotalSecs:        report.TotalSecs,
		UnattributedSecs: report.UnattributedSecs,
		CrashLossSecs:    crashLoss,
	}
}

func makeDelta(before, after float64) Delta {
	return Delta{
		BeforeSecs:    before,
		AfterSecs:     after,
		DeltaSecs:     after - before,
		PercentChange: percentChange(before, after),
	}
}

func percentChange(before, after float64) *float64 {
	if before == 0 {
		return nil
	}
	change := (after - before) / before * 100
	return &change
}

func compareFindings(before, after []model.Finding) FindingsComparison {
	orderedBefore := append([]model.Finding(nil), before...)
	orderedAfter := append([]model.Finding(nil), after...)
	sort.SliceStable(orderedBefore, func(i, j int) bool {
		return orderedBefore[i].EstRecoverableSecs > orderedBefore[j].EstRecoverableSecs
	})
	sort.SliceStable(orderedAfter, func(i, j int) bool {
		return orderedAfter[i].EstRecoverableSecs > orderedAfter[j].EstRecoverableSecs
	})

	matchedAfter := make([]bool, len(orderedAfter))
	removed := make([]FindingSummary, 0)
	for _, finding := range orderedBefore {
		key := findingKey(finding)
		matched := false
		for i, candidate := range orderedAfter {
			if !matchedAfter[i] && findingKey(candidate) == key {
				matchedAfter[i] = true
				matched = true
				break
			}
		}
		if !matched {
			removed = append(removed, summarizeFinding(finding))
		}
	}

	added := make([]FindingSummary, 0)
	beforeSummaries := make([]FindingSummary, 0, len(orderedBefore))
	afterSummaries := make([]FindingSummary, 0, len(orderedAfter))
	for _, finding := range orderedBefore {
		beforeSummaries = append(beforeSummaries, summarizeFinding(finding))
	}
	for i, finding := range orderedAfter {
		afterSummaries = append(afterSummaries, summarizeFinding(finding))
		if !matchedAfter[i] {
			added = append(added, summarizeFinding(finding))
		}
	}

	return FindingsComparison{
		BeforeCount: len(before),
		AfterCount:  len(after),
		Before:      beforeSummaries,
		After:       afterSummaries,
		Added:       added,
		Removed:     removed,
	}
}

func findingKey(finding model.Finding) string {
	detail := finding.Condition + "\x00" + finding.Recommendation
	if finding.Condition == "" && finding.Recommendation == "" {
		detail = finding.Text
	}
	return finding.Phase + "\x00" + detail
}

func summarizeFinding(finding model.Finding) FindingSummary {
	return FindingSummary{
		Phase:           finding.Phase,
		ObservedSecs:    finding.ObservedSecs,
		RecoverableSecs: finding.EstRecoverableSecs,
		Confidence:      finding.Confidence,
		Condition:       finding.Condition,
		Recommendation:  finding.Recommendation,
		Text:            finding.Text,
	}
}
