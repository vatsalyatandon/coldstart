// Package render formats a normalized coldstart report for people and tools.
package render

import (
	"context"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

const (
	FormatText     = "text"
	FormatJSON     = "json"
	FormatMarkdown = "markdown"
	FormatHTML     = "html"
)

// Render writes report in the requested format. Supported formats are text,
// json, markdown, and html.
func Render(ctx context.Context, w io.Writer, report model.Report, format string) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case FormatText:
		return renderText(ctx, w, report)
	case FormatJSON:
		return renderJSON(ctx, w, report)
	case FormatMarkdown:
		return renderMarkdown(ctx, w, report)
	case FormatHTML:
		return renderHTML(ctx, w, report)
	default:
		return fmt.Errorf("unsupported output format %q (want text, json, markdown, or html)", format)
	}
}

func renderJSON(ctx context.Context, w io.Writer, report model.Report) error {
	// The schema is part of the public contract. Do not allow an omitted or
	// stale schema on an input report to change the output contract.
	report.Schema = model.ReportSchema
	encoded, err := marshalIndentedJSON(report)
	if err != nil {
		return fmt.Errorf("encode coldstart report: %w", err)
	}
	return writeBytes(ctx, w, encoded)
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

func formatPercent(secs, total float64) string {
	if total <= 0 || math.IsNaN(total) || math.IsInf(total, 0) || math.IsNaN(secs) || math.IsInf(secs, 0) {
		return "0.0%"
	}
	return fmt.Sprintf("%.1f%%", secs/total*100)
}

// waterfallBar returns a bar whose filled proportion is secs/total. Sub-cell
// blocks keep very short, nonzero phases visible.
func waterfallBar(secs, total float64, width int) string {
	if width <= 0 || total <= 0 || math.IsNaN(total) || math.IsInf(total, 0) || secs <= 0 || math.IsNaN(secs) || math.IsInf(secs, 0) {
		return ""
	}
	cells := secs / total * float64(width)
	if cells > float64(width) {
		cells = float64(width)
	}
	full := int(math.Floor(cells))
	if full > width {
		full = width
	}
	var b strings.Builder
	b.WriteString(strings.Repeat("█", full))
	if full == width {
		return b.String()
	}
	fraction := cells - float64(full)
	if fraction <= 0 {
		return b.String()
	}
	partials := []rune("▏▎▍▌▋▊▉")
	index := int(math.Round(fraction*8)) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(partials) {
		index = len(partials) - 1
	}
	b.WriteRune(partials[index])
	return b.String()
}

func phaseName(phase model.Phase) string {
	if phase.Name != "" {
		return phase.Name
	}
	if phase.ID != "" {
		return phase.ID
	}
	return "unnamed phase"
}

func phaseDepths(phases []model.Phase) map[string]int {
	byID := make(map[string]model.Phase, len(phases))
	for _, phase := range phases {
		if phase.ID != "" {
			byID[phase.ID] = phase
		}
	}

	depths := make(map[string]int, len(phases))
	for _, phase := range phases {
		depth := 0
		seen := map[string]bool{phase.ID: true}
		current := phase
		for current.ParentID != "" {
			depth++
			if seen[current.ParentID] {
				break
			}
			seen[current.ParentID] = true
			parent, ok := byID[current.ParentID]
			if !ok {
				break
			}
			current = parent
		}
		depths[phase.ID] = depth
	}
	return depths
}

func phaseLabel(report model.Report, id string) string {
	for _, phase := range report.Phases {
		if phase.ID == id {
			return phaseName(phase)
		}
	}
	for _, attempt := range report.Attempts {
		for _, phase := range attempt.Phases {
			if phase.ID == id {
				return phaseName(phase)
			}
		}
	}
	if id == "" {
		return "unknown phase"
	}
	return id
}

func rankedFindings(findings []model.Finding) []model.Finding {
	ordered := append([]model.Finding(nil), findings...)
	sortFindings(ordered)
	return ordered
}

func reportWarnings(report model.Report) []string {
	warnings := append([]string(nil), report.Warnings...)
	if report.RulePack.Warning != "" {
		alreadyPresent := false
		for _, warning := range warnings {
			if warning == report.RulePack.Warning {
				alreadyPresent = true
				break
			}
		}
		if !alreadyPresent {
			warnings = append(warnings, report.RulePack.Warning)
		}
	}
	return warnings
}

func findingBody(finding model.Finding) string {
	var body string
	if finding.Text != "" {
		body = finding.Text
	} else if finding.Condition != "" {
		body = finding.Condition
	} else if finding.Recommendation != "" {
		body = finding.Recommendation
	} else {
		body = "No finding details were provided."
	}

	tradeoff := strings.TrimSpace(finding.Tradeoff)
	if tradeoff == "" {
		return body
	}
	suffix := fmt.Sprintf("(trade-off: %s)", tradeoff)
	if strings.Contains(strings.ToLower(body), strings.ToLower(suffix)) {
		return body
	}
	return body + " " + suffix
}

func engineContainerLabel(engine model.EngineInfo) string {
	if engine.Container == "" {
		return ""
	}
	source := engine.ContainerSource
	if source == "" {
		source = "detected"
	}
	return fmt.Sprintf("engine_container: %s (%s)", engine.Container, source)
}

func engineLabel(engine model.EngineInfo) string {
	name := engine.Name
	switch strings.ToLower(name) {
	case "vllm":
		name = "vLLM"
	case "sglang":
		name = "SGLang"
	}
	if name == "" {
		name = "unknown engine"
	}
	if engine.Version != "" {
		return name + " " + engine.Version
	}
	return name
}

func hardwareLabel(hardware model.Hardware) string {
	var parts []string
	gpu := hardware.GPU
	if gpu == "" {
		gpu = "GPU unknown"
	}
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
	} else if hardware.NodeFresh {
		parts = append(parts, "fresh node")
	}
	if hardware.Storage != "" {
		parts = append(parts, "storage "+hardware.Storage)
	}
	return strings.Join(parts, " · ")
}

func timelinePhases(report model.Report) []model.Phase {
	return phasesWithUnattributed(report.Phases, report.UnattributedSecs)
}

func phasesWithUnattributed(phases []model.Phase, unattributedSecs float64) []model.Phase {
	phases = append([]model.Phase(nil), phases...)
	hasUnattributed := false
	for _, phase := range phases {
		if phase.ID == "unattributed" {
			hasUnattributed = true
			break
		}
	}
	if !hasUnattributed && unattributedSecs > 0.001 {
		phases = append(phases, model.Phase{
			ID:         "unattributed",
			Name:       "unattributed",
			Secs:       unattributedSecs,
			Source:     "gap",
			Confidence: "low",
		})
	}
	return phases
}

func selectedTimeline(report model.Report) []model.Phase {
	if len(report.Phases) > 0 {
		return timelinePhases(report)
	}
	for _, attempt := range report.Attempts {
		if attempt.Outcome == "ready" && len(attempt.Phases) > 0 {
			return phasesWithUnattributed(attempt.Phases, report.UnattributedSecs)
		}
	}
	if len(report.Attempts) > 0 {
		last := report.Attempts[len(report.Attempts)-1]
		if len(last.Phases) > 0 {
			return phasesWithUnattributed(last.Phases, report.UnattributedSecs)
		}
	}
	return timelinePhases(report)
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return "—"
	}
	return value.Format(time.RFC3339)
}
