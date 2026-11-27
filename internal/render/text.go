package render

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

func marshalIndentedJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sortFindings(findings []model.Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		left := findingRankScore(findings[i])
		right := findingRankScore(findings[j])
		if left != right {
			return left > right
		}
		if findings[i].ObservedSecs != findings[j].ObservedSecs {
			return findings[i].ObservedSecs > findings[j].ObservedSecs
		}
		return findings[i].Phase < findings[j].Phase
	})
}

func findingRankScore(finding model.Finding) float64 {
	secs := finding.EstRecoverableSecs
	if secs < 0 || math.IsNaN(secs) || math.IsInf(secs, 0) {
		return 0
	}
	switch strings.ToLower(strings.TrimSpace(finding.Confidence)) {
	case "high":
		return secs
	case "medium":
		return secs * 0.6
	case "low":
		return secs * 0.3
	default:
		return 0
	}
}

func renderText(ctx context.Context, w io.Writer, report model.Report) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  ·  %s  ·  %s  ·  total %s\n",
		report.Pod, engineLabel(report.Engine), hardwareLabel(report.Hardware), formatDuration(report.TotalSecs))
	if label := engineContainerLabel(report.Engine); label != "" {
		fmt.Fprintf(&b, "%s\n", label)
	}
	b.WriteByte('\n')

	phases := selectedTimeline(report)
	const barWidth = 40
	depths := phaseDepths(phases)
	labelWidth := len("Phase")
	for _, phase := range phases {
		label := strings.Repeat("  ", depths[phase.ID]) + phaseName(phase)
		if width := utf8.RuneCountInString(label); width > labelWidth {
			labelWidth = width
		}
	}

	b.WriteString("Startup waterfall:\n")
	fmt.Fprintf(&b, "  %-*s  %-*s  %8s  %6s\n", labelWidth, "Phase", barWidth, "", "Duration", "% total")
	for _, phase := range phases {
		label := strings.Repeat("  ", depths[phase.ID]) + phaseName(phase)
		bar := waterfallBar(phase.Secs, report.TotalSecs, barWidth)
		fmt.Fprintf(&b, "  %s  %s  %8s  %6s\n",
			padRight(label, labelWidth), padRight(bar, barWidth), formatDuration(phase.Secs), formatPercent(phase.Secs, report.TotalSecs))
	}

	b.WriteString("\nAttempts:\n")
	if len(report.Attempts) == 0 {
		b.WriteString("  none reported\n")
	} else {
		totalLost := 0.0
		for _, attempt := range report.Attempts {
			totalLost += attempt.LostSecs
			fmt.Fprintf(&b, "  Attempt %d: %s · lost %s · %s to %s\n",
				attempt.Index, attemptOutcome(attempt.Outcome), formatDuration(attempt.LostSecs),
				formatOptionalTime(attempt.Start), formatOptionalTime(attempt.End))
			if attempt.CrashReason != "" {
				fmt.Fprintf(&b, "    crash reason: %s\n", attempt.CrashReason)
			}
		}
		if totalLost > 0 {
			fmt.Fprintf(&b, "  Crash loss total: %s\n", formatDuration(totalLost))
		}
	}

	b.WriteString("\nFindings (ranked by confidence-weighted recoverable seconds):\n")
	findings := rankedFindings(report.Findings)
	if len(findings) == 0 {
		b.WriteString("  none\n")
	}
	for i, finding := range findings {
		body := findingBody(finding)
		fmt.Fprintf(&b, "  %d. %s: %s\n", i+1, phaseLabel(report, finding.Phase), body)
		fmt.Fprintf(&b, "     observed %s · recoverable %s · confidence %s\n",
			formatDuration(finding.ObservedSecs), formatDuration(finding.EstRecoverableSecs), findingConfidence(finding.Confidence))
		if finding.Recommendation != "" && !strings.Contains(body, finding.Recommendation) {
			fmt.Fprintf(&b, "     fix: %s\n", finding.Recommendation)
		}
	}

	b.WriteString("\nWarnings:\n")
	warnings := reportWarnings(report)
	if len(warnings) == 0 {
		b.WriteString("  none\n")
	}
	for _, warning := range warnings {
		fmt.Fprintf(&b, "  - %s\n", warning)
	}

	return writeBytes(ctx, w, []byte(b.String()))
}

func padRight(value string, width int) string {
	padding := width - utf8.RuneCountInString(value)
	if padding <= 0 {
		return value
	}
	return value + strings.Repeat(" ", padding)
}

func attemptOutcome(outcome string) string {
	if outcome == "" {
		return "unknown outcome"
	}
	return outcome
}

func findingConfidence(confidence string) string {
	if confidence == "" {
		return "unknown"
	}
	return confidence
}

func markdownCell(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.ReplaceAll(value, "\n", "<br>")
}

func renderMarkdown(ctx context.Context, w io.Writer, report model.Report) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# coldstart startup report: %s\n\n", markdownCell(report.Pod))
	b.WriteString("| Pod | Engine | Hardware | Total |\n|---|---|---|---|\n")
	fmt.Fprintf(&b, "| %s | %s | %s | %s |\n\n",
		markdownCell(report.Pod), markdownCell(engineLabel(report.Engine)), markdownCell(hardwareLabel(report.Hardware)), formatDuration(report.TotalSecs))
	if label := engineContainerLabel(report.Engine); label != "" {
		fmt.Fprintf(&b, "**%s**\n\n", markdownCell(label))
	}

	b.WriteString("## Startup waterfall\n\n")
	b.WriteString("| Phase | Duration | % of total | Source | Confidence |\n|---|---:|---:|---|---|\n")
	phases := selectedTimeline(report)
	depths := phaseDepths(phases)
	for _, phase := range phases {
		label := strings.Repeat("↳ ", depths[phase.ID]) + phaseName(phase)
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			markdownCell(label), formatDuration(phase.Secs), formatPercent(phase.Secs, report.TotalSecs),
			markdownCell(phase.Source), markdownCell(findingConfidence(phase.Confidence)))
	}

	b.WriteString("\n## Attempts\n\n")
	if len(report.Attempts) == 0 {
		b.WriteString("_No attempt details reported._\n")
	} else {
		b.WriteString("| Attempt | Outcome | Start | End | Crash reason | Time lost |\n|---:|---|---|---|---|---:|\n")
		for _, attempt := range report.Attempts {
			fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s |\n",
				attempt.Index, markdownCell(attemptOutcome(attempt.Outcome)), formatOptionalTime(attempt.Start),
				formatOptionalTime(attempt.End), markdownCell(attempt.CrashReason), formatDuration(attempt.LostSecs))
		}
	}

	b.WriteString("\n## Findings (ranked by confidence-weighted recoverable seconds)\n\n")
	findings := rankedFindings(report.Findings)
	if len(findings) == 0 {
		b.WriteString("_No findings._\n")
	} else {
		b.WriteString("| Rank | Phase | Observed | Recoverable | Confidence | Finding | Recommendation |\n|---:|---|---:|---:|---|---|---|\n")
		for i, finding := range findings {
			fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s | %s |\n",
				i+1, markdownCell(phaseLabel(report, finding.Phase)), formatDuration(finding.ObservedSecs),
				formatDuration(finding.EstRecoverableSecs), markdownCell(findingConfidence(finding.Confidence)),
				markdownCell(findingBody(finding)), markdownCell(finding.Recommendation))
		}
	}

	b.WriteString("\n## Warnings\n\n")
	warnings := reportWarnings(report)
	if len(warnings) == 0 {
		b.WriteString("_No warnings._\n")
	} else {
		for _, warning := range warnings {
			fmt.Fprintf(&b, "- %s\n", markdownCell(warning))
		}
	}

	return writeBytes(ctx, w, []byte(b.String()))
}
