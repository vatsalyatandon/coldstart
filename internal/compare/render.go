package compare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

const (
	FormatText     = "text"
	FormatJSON     = "json"
	FormatMarkdown = "markdown"
)

// Render writes a comparison in text, markdown, or json format.
func Render(ctx context.Context, w io.Writer, comparison Comparison, format string) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case FormatText:
		return RenderText(ctx, w, comparison)
	case FormatJSON:
		return RenderJSON(ctx, w, comparison)
	case FormatMarkdown:
		return RenderMarkdown(ctx, w, comparison)
	default:
		return fmt.Errorf("unsupported comparison format %q (want text, markdown, or json)", format)
	}
}

// RenderText writes an aligned, human-readable comparison.
func RenderText(ctx context.Context, w io.Writer, comparison Comparison) error {
	var b strings.Builder
	b.WriteString("Startup comparison\n")
	fmt.Fprintf(&b, "Before: %s · %s · total %s\n", comparison.Before.Pod, engineLabel(comparison.Before.Engine), formatDuration(comparison.Before.TotalSecs))
	fmt.Fprintf(&b, "After:  %s · %s · total %s\n\n", comparison.After.Pod, engineLabel(comparison.After.Engine), formatDuration(comparison.After.TotalSecs))
	writeDeltaLine(&b, "Total startup", comparison.Totals.Startup)
	writeDeltaLine(&b, "Unattributed", comparison.Totals.Unattributed)
	writeDeltaLine(&b, "Crash loss", comparison.Totals.CrashLoss)

	b.WriteString("\nPhase deltas (after - before):\n")
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "Phase\tBefore\tAfter\tDelta")
	for _, phase := range comparison.Phases {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			phase.Name, optionalDuration(phase.BeforeSecs, phase.BeforePresent),
			optionalDuration(phase.AfterSecs, phase.AfterPresent), signedDelta(phase.DeltaSecs))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("render comparison text: %w", err)
	}

	fmt.Fprintf(&b, "\nFindings: before %d, after %d\n", comparison.Findings.BeforeCount, comparison.Findings.AfterCount)
	writeTextFindings(&b, "Before", comparison.Findings.Before)
	writeTextFindings(&b, "After", comparison.Findings.After)
	writeTextFindings(&b, "Added", comparison.Findings.Added)
	writeTextFindings(&b, "Removed", comparison.Findings.Removed)

	return writeBytes(ctx, w, []byte(b.String()))
}

// RenderMarkdown writes GitHub-flavored Markdown suitable for an issue.
func RenderMarkdown(ctx context.Context, w io.Writer, comparison Comparison) error {
	var b strings.Builder
	b.WriteString("# coldstart startup comparison\n\n")
	b.WriteString("| Report | Pod | Engine | Hardware | Total |\n|---|---|---|---|---:|\n")
	fmt.Fprintf(&b, "| Before | %s | %s | %s | %s |\n",
		markdownCell(comparison.Before.Pod), markdownCell(engineLabel(comparison.Before.Engine)),
		markdownCell(hardwareLabel(comparison.Before.Hardware)), formatDuration(comparison.Before.TotalSecs))
	fmt.Fprintf(&b, "| After | %s | %s | %s | %s |\n\n",
		markdownCell(comparison.After.Pod), markdownCell(engineLabel(comparison.After.Engine)),
		markdownCell(hardwareLabel(comparison.After.Hardware)), formatDuration(comparison.After.TotalSecs))

	b.WriteString("## Totals\n\n")
	b.WriteString("| Metric | Before | After | Delta (after - before) | Change |\n|---|---:|---:|---:|---:|\n")
	writeMarkdownDelta(&b, "Total startup", comparison.Totals.Startup)
	writeMarkdownDelta(&b, "Unattributed", comparison.Totals.Unattributed)
	writeMarkdownDelta(&b, "Crash loss", comparison.Totals.CrashLoss)

	b.WriteString("\n## Phase deltas\n\n")
	b.WriteString("| Phase | Before | After | Delta (after - before) | Change |\n|---|---:|---:|---:|---:|\n")
	for _, phase := range comparison.Phases {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			markdownCell(phase.Name), optionalDuration(phase.BeforeSecs, phase.BeforePresent),
			optionalDuration(phase.AfterSecs, phase.AfterPresent), signedDelta(phase.DeltaSecs),
			formatPercentChange(phase.PercentChange))
	}

	fmt.Fprintf(&b, "\n## Findings\n\nBefore: %d · After: %d · Added: %d · Removed: %d\n",
		comparison.Findings.BeforeCount, comparison.Findings.AfterCount,
		len(comparison.Findings.Added), len(comparison.Findings.Removed))
	writeMarkdownFindings(&b, "Before", comparison.Findings.Before)
	writeMarkdownFindings(&b, "After", comparison.Findings.After)
	writeMarkdownFindings(&b, "Added", comparison.Findings.Added)
	writeMarkdownFindings(&b, "Removed", comparison.Findings.Removed)

	return writeBytes(ctx, w, []byte(b.String()))
}

// RenderJSON writes the stable comparison representation.
func RenderJSON(ctx context.Context, w io.Writer, comparison Comparison) error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(comparison); err != nil {
		return fmt.Errorf("encode coldstart comparison: %w", err)
	}
	return writeBytes(ctx, w, buf.Bytes())
}

func writeBytes(ctx context.Context, w io.Writer, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n, err := w.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func writeDeltaLine(b *strings.Builder, label string, delta Delta) {
	fmt.Fprintf(b, "%s: %s -> %s (%s, %s)\n", label,
		formatDuration(delta.BeforeSecs), formatDuration(delta.AfterSecs),
		signedDelta(delta.DeltaSecs), formatPercentChange(delta.PercentChange))
}

func writeMarkdownDelta(b *strings.Builder, label string, delta Delta) {
	fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", markdownCell(label),
		formatDuration(delta.BeforeSecs), formatDuration(delta.AfterSecs),
		signedDelta(delta.DeltaSecs), formatPercentChange(delta.PercentChange))
}

func signedDelta(secs float64) string {
	return formatSignedDuration(secs)
}

func formatPercentChange(percent *float64) string {
	if percent == nil {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%%", *percent)
}

func optionalDuration(secs float64, present bool) string {
	if !present {
		return "—"
	}
	return formatDuration(secs)
}

func writeTextFindings(b *strings.Builder, label string, findings []FindingSummary) {
	fmt.Fprintf(b, "\n%s findings:\n", label)
	if len(findings) == 0 {
		b.WriteString("  none\n")
		return
	}
	for i, finding := range findings {
		body := findingBody(finding)
		fmt.Fprintf(b, "  %d. %s: %s (recoverable %s, confidence %s)\n",
			i+1, finding.Phase, body, formatDuration(finding.RecoverableSecs), confidenceLabel(finding.Confidence))
		if finding.Recommendation != "" && finding.Recommendation != body {
			fmt.Fprintf(b, "     fix: %s\n", finding.Recommendation)
		}
	}
}

func writeMarkdownFindings(b *strings.Builder, label string, findings []FindingSummary) {
	fmt.Fprintf(b, "\n### %s (%d)\n\n", label, len(findings))
	if len(findings) == 0 {
		b.WriteString("_None._\n")
		return
	}
	b.WriteString("| Rank | Phase | Observed | Recoverable | Confidence | Finding | Recommendation |\n|---:|---|---:|---:|---|---|---|\n")
	for i, finding := range findings {
		fmt.Fprintf(b, "| %d | %s | %s | %s | %s | %s | %s |\n",
			i+1, markdownCell(finding.Phase), formatDuration(finding.ObservedSecs),
			formatDuration(finding.RecoverableSecs), markdownCell(confidenceLabel(finding.Confidence)),
			markdownCell(findingBody(finding)), markdownCell(finding.Recommendation))
	}
}

func findingBody(finding FindingSummary) string {
	if finding.Text != "" {
		return finding.Text
	}
	if finding.Condition != "" {
		return finding.Condition
	}
	if finding.Recommendation != "" {
		return finding.Recommendation
	}
	return "No finding details were provided."
}

func confidenceLabel(confidence string) string {
	if confidence == "" {
		return "unknown"
	}
	return confidence
}

func engineLabel(engine model.EngineInfo) string {
	name := engine.Name
	if name == "" {
		name = "unknown engine"
	}
	if engine.Version != "" {
		return name + " " + engine.Version
	}
	return name
}

func hardwareLabel(hardware model.Hardware) string {
	gpu := hardware.GPU
	if gpu == "" {
		gpu = "GPU unknown"
	}
	var parts []string
	if hardware.Count > 0 {
		parts = append(parts, fmt.Sprintf("%dx %s", hardware.Count, gpu))
	} else {
		parts = append(parts, gpu)
	}
	if hardware.Node != "" {
		node := "node " + hardware.Node
		if hardware.NodeFresh {
			node += " (fresh)"
		}
		parts = append(parts, node)
	}
	if hardware.Storage != "" {
		parts = append(parts, "storage "+hardware.Storage)
	}
	return strings.Join(parts, " · ")
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

func formatDuration(secs float64) string {
	if math.IsNaN(secs) || math.IsInf(secs, 0) {
		return "n/a"
	}
	if secs < 0 {
		return "-" + formatDuration(-secs)
	}
	if secs == 0 {
		return "0s"
	}
	if secs < 1 {
		return fmt.Sprintf("%.1fs", secs)
	}
	d := time.Duration(math.Round(secs) * float64(time.Second))
	if d < time.Minute {
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
	hours := int64(d / time.Hour)
	minutes := int64(d/time.Minute) % 60
	seconds := int64(d/time.Second) % 60
	if hours > 0 {
		if seconds == 0 {
			return fmt.Sprintf("%dh %02dm", hours, minutes)
		}
		return fmt.Sprintf("%dh %02dm %02ds", hours, minutes, seconds)
	}
	if seconds == 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%dm %02ds", minutes, seconds)
}

func formatSignedDuration(secs float64) string {
	if math.IsNaN(secs) || math.IsInf(secs, 0) || secs == 0 {
		return formatDuration(secs)
	}
	if secs > 0 {
		return "+" + formatDuration(secs)
	}
	return formatDuration(secs)
}
