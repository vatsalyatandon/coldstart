package render

import (
	"bytes"
	"context"
	"fmt"
	htmltemplate "html/template"
	"io"
	"math"
	"strings"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

type htmlReportData struct {
	Pod       string
	Engine    string
	Hardware  string
	Total     string
	T0        string
	Timelines []htmlTimeline
	Findings  []model.Finding
	Attempts  []htmlAttempt
	Warnings  []string
}

type htmlTimeline struct {
	Key    string
	Title  string
	Meta   string
	Phases []htmlPhase
}

type htmlPhase struct {
	Name          string
	ID            string
	Duration      string
	Percent       string
	WidthStyle    htmltemplate.CSS
	Source        string
	Confidence    string
	ConfidenceKey string
	Search        string
}

type htmlAttempt struct {
	Index       int
	Outcome     string
	Start       string
	End         string
	CrashReason string
	Lost        string
}

var reportHTMLTemplate = htmltemplate.Must(htmltemplate.New("report").Funcs(htmltemplate.FuncMap{
	"add":         func(a, b int) int { return a + b },
	"duration":    formatDuration,
	"confidence":  findingConfidence,
	"findingBody": findingBody,
}).Parse(htmlDocument))

func renderHTML(ctx context.Context, w io.Writer, report model.Report) error {
	data := htmlReportData{
		Pod:       report.Pod,
		Engine:    engineLabel(report.Engine),
		Hardware:  hardwareLabel(report.Hardware),
		Total:     formatDuration(report.TotalSecs),
		T0:        report.T0.Format("2006-01-02 15:04:05 MST"),
		Findings:  rankedFindings(report.Findings),
		Warnings:  reportWarnings(report),
		Timelines: htmlTimelines(report),
	}
	for _, attempt := range report.Attempts {
		data.Attempts = append(data.Attempts, htmlAttempt{
			Index:       attempt.Index,
			Outcome:     attemptOutcome(attempt.Outcome),
			Start:       formatOptionalTime(attempt.Start),
			End:         formatOptionalTime(attempt.End),
			CrashReason: attempt.CrashReason,
			Lost:        formatDuration(attempt.LostSecs),
		})
	}

	var buf bytes.Buffer
	if err := reportHTMLTemplate.Execute(&buf, data); err != nil {
		return fmt.Errorf("render coldstart HTML: %w", err)
	}
	return writeBytes(ctx, w, buf.Bytes())
}

func htmlTimelines(report model.Report) []htmlTimeline {
	var timelines []htmlTimeline
	if len(report.Phases) > 0 || report.UnattributedSecs != 0 || len(report.Attempts) == 0 {
		timelines = append(timelines, htmlTimeline{
			Key:    "current",
			Title:  "Current report",
			Meta:   "The normalized phase timeline for this report.",
			Phases: htmlPhases(timelinePhases(report), report.TotalSecs),
		})
	}
	for _, attempt := range report.Attempts {
		if len(attempt.Phases) == 0 {
			continue
		}
		phases := append([]model.Phase(nil), attempt.Phases...)
		timelines = append(timelines, htmlTimeline{
			Key:    fmt.Sprintf("attempt-%d", attempt.Index),
			Title:  fmt.Sprintf("Attempt %d", attempt.Index),
			Meta:   fmt.Sprintf("Outcome: %s · time lost: %s", attemptOutcome(attempt.Outcome), formatDuration(attempt.LostSecs)),
			Phases: htmlPhases(phases, report.TotalSecs),
		})
	}
	if len(timelines) == 0 {
		timelines = append(timelines, htmlTimeline{
			Key:    "current",
			Title:  "Current report",
			Meta:   "The normalized phase timeline for this report.",
			Phases: htmlPhases(timelinePhases(report), report.TotalSecs),
		})
	}
	return timelines
}

func htmlPhases(phases []model.Phase, total float64) []htmlPhase {
	depths := phaseDepths(phases)
	result := make([]htmlPhase, 0, len(phases))
	for _, phase := range phases {
		percent := 0.0
		if total > 0 && !math.IsNaN(phase.Secs) && !math.IsInf(phase.Secs, 0) {
			percent = phase.Secs / total * 100
		}
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
		confidence := findingConfidence(phase.Confidence)
		result = append(result, htmlPhase{
			Name:          strings.Repeat("↳ ", depths[phase.ID]) + phaseName(phase),
			ID:            phase.ID,
			Duration:      formatDuration(phase.Secs),
			Percent:       formatPercent(phase.Secs, total),
			WidthStyle:    htmltemplate.CSS(fmt.Sprintf("width: %.4f%%", percent)),
			Source:        phase.Source,
			Confidence:    confidence,
			ConfidenceKey: strings.ToLower(confidence),
			Search:        strings.Join([]string{phase.ID, phaseName(phase), phase.Source, confidence}, " "),
		})
	}
	return result
}

const htmlDocument = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>coldstart startup report · {{.Pod}}</title>
  <style>
    :root {
      color-scheme: light dark;
      --bg: #f6f8fa;
      --surface: #ffffff;
      --text: #1f2328;
      --muted: #59636e;
      --border: #d0d7de;
      --accent: #0969da;
      --bar: #2da44e;
      --warning-bg: #fff8c5;
      --warning-text: #4d2d00;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      background: var(--bg);
      color: var(--text);
      font: 16px/1.5 system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }
    .skip-link {
      position: absolute;
      left: -9999px;
      top: 0;
      padding: .5rem 1rem;
      background: var(--surface);
      color: var(--accent);
      z-index: 2;
    }
    .skip-link:focus { left: .5rem; top: .5rem; }
    .report-header, main, .report-footer {
      max-width: 72rem;
      margin-inline: auto;
      padding: 1.25rem;
    }
    .report-header { padding-top: 2rem; }
    h1, h2, h3 { line-height: 1.2; }
    h1 { margin: .25rem 0 .75rem; overflow-wrap: anywhere; }
    h2 { margin-top: 0; }
    h3 { margin-bottom: .25rem; }
    .eyebrow {
      color: var(--accent);
      font-weight: 700;
      letter-spacing: .08em;
      margin: 0;
      text-transform: uppercase;
    }
    .summary { font-size: 1.05rem; }
    .muted { color: var(--muted); }
    section.card {
      background: var(--surface);
      border: 1px solid var(--border);
      border-radius: .75rem;
      margin-bottom: 1.25rem;
      padding: 1.25rem;
    }
    .filters {
      display: flex;
      flex-wrap: wrap;
      align-items: end;
      gap: .75rem;
      margin-block: 1rem;
    }
    .field { display: grid; gap: .25rem; }
    .field.search { flex: 1 1 16rem; }
    label { font-weight: 600; }
    input, select, button {
      font: inherit;
      color: var(--text);
      background: var(--surface);
      border: 1px solid var(--border);
      border-radius: .4rem;
      padding: .5rem .65rem;
      min-height: 2.75rem;
    }
    button {
      background: var(--accent);
      border-color: var(--accent);
      color: #ffffff;
      cursor: pointer;
      font-weight: 600;
    }
    input:focus-visible, select:focus-visible, button:focus-visible, [tabindex]:focus-visible {
      outline: 3px solid var(--accent);
      outline-offset: 2px;
    }
    .timeline { margin-top: 1.5rem; }
    .timeline > p { margin-top: 0; }
    .table-scroll {
      overflow-x: auto;
      border: 1px solid var(--border);
      border-radius: .5rem;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      min-width: 42rem;
    }
    caption {
      padding: .65rem .75rem;
      text-align: left;
      font-weight: 600;
      background: var(--bg);
    }
    th, td {
      padding: .6rem .75rem;
      border-top: 1px solid var(--border);
      text-align: left;
      vertical-align: middle;
    }
    thead th { background: var(--bg); }
    td.numeric, th.numeric { text-align: right; font-variant-numeric: tabular-nums; white-space: nowrap; }
    .phase-name { overflow-wrap: anywhere; }
    .phase-id { color: var(--muted); font-size: .85rem; }
    .bar-track {
      width: 12rem;
      height: .8rem;
      border-radius: 999px;
      background: var(--bg);
      border: 1px solid var(--border);
      overflow: hidden;
    }
    .bar-fill { display: block; height: 100%; background: var(--bar); }
    .findings-table { min-width: 52rem; }
    .warning-list {
      margin: .5rem 0 0;
      padding-left: 1.4rem;
    }
    .warning-list li {
      background: var(--warning-bg);
      color: var(--warning-text);
      border-radius: .35rem;
      margin-block: .5rem;
      padding: .6rem .75rem;
      overflow-wrap: anywhere;
    }
    [hidden] { display: none !important; }
    @media (max-width: 42rem) {
      .report-header, main, .report-footer { padding: .85rem; }
      section.card { padding: .85rem; }
      .filters { align-items: stretch; }
      .field, .field.search { flex-basis: 100%; }
      input, select, button { width: 100%; }
    }
    @media (prefers-color-scheme: dark) {
      :root {
        --bg: #0d1117;
        --surface: #161b22;
        --text: #e6edf3;
        --muted: #9198a1;
        --border: #30363d;
        --accent: #58a6ff;
        --bar: #3fb950;
        --warning-bg: #3b2e00;
        --warning-text: #ffdf5d;
      }
    }
  </style>
</head>
<body>
  <a class="skip-link" href="#main">Skip to report</a>
  <header class="report-header">
    <p class="eyebrow">coldstart</p>
    <h1>Startup report: {{.Pod}}</h1>
    <p class="summary">{{.Engine}} · {{.Hardware}} · total <strong>{{.Total}}</strong></p>
    <p class="muted">Timeline origin: {{.T0}}</p>
  </header>
  <main id="main">
    <section class="card" aria-labelledby="timeline-heading">
      <h2 id="timeline-heading">Startup timeline</h2>
      <p id="filter-help">Filter by phase, attempt, or confidence. Bars are scaled to the report total.</p>
      <div class="filters" role="search" aria-label="Timeline filters">
        <div class="field search">
          <label for="phase-filter">Phase search</label>
          <input id="phase-filter" type="search" placeholder="e.g. weights, compile, event" autocomplete="off" aria-controls="timeline-list">
        </div>
        <div class="field">
          <label for="attempt-filter">Attempt</label>
          <select id="attempt-filter" aria-controls="timeline-list">
            <option value="all">All attempts</option>
            {{range .Timelines}}<option value="{{.Key}}">{{.Title}}</option>{{end}}
          </select>
        </div>
        <div class="field">
          <label for="confidence-filter">Confidence</label>
          <select id="confidence-filter" aria-controls="timeline-list">
            <option value="all">All confidence levels</option>
            <option value="high">High</option>
            <option value="medium">Medium</option>
            <option value="low">Low</option>
            <option value="unknown">Unknown</option>
          </select>
        </div>
        <button id="reset-filters" type="button">Reset filters</button>
      </div>
      <p id="filter-status" class="muted" role="status" aria-live="polite"></p>
      <p id="filter-empty" hidden>No phases match the selected filters.</p>
      <noscript><p>JavaScript is disabled. All timelines are shown and the filters are inactive.</p></noscript>
      <div id="timeline-list">
        {{range .Timelines}}
        <section class="timeline" data-timeline="{{.Key}}" aria-labelledby="timeline-title-{{.Key}}">
          <h3 id="timeline-title-{{.Key}}">{{.Title}}</h3>
          <p class="muted">{{.Meta}}</p>
          <div class="table-scroll" tabindex="0" role="region" aria-label="{{.Title}} phase table">
            <table>
              <caption>{{.Title}} phase durations</caption>
              <thead>
                <tr>
                  <th scope="col">Phase</th>
                  <th scope="col" class="numeric">Duration</th>
                  <th scope="col" class="numeric">% of total</th>
                  <th scope="col">Relative duration</th>
                  <th scope="col">Source</th>
                  <th scope="col">Confidence</th>
                </tr>
              </thead>
              <tbody>
                {{range .Phases}}
                <tr data-phase data-search="{{.Search}}" data-confidence="{{.ConfidenceKey}}">
                  <td>
                    <div class="phase-name">{{.Name}}</div>
                    <div class="phase-id">{{.ID}}</div>
                  </td>
                  <td class="numeric">{{.Duration}}</td>
                  <td class="numeric">{{.Percent}}</td>
                  <td>
                    <div class="bar-track" aria-hidden="true"><span class="bar-fill" style="{{.WidthStyle}}"></span></div>
                  </td>
                  <td>{{.Source}}</td>
                  <td>{{.Confidence}}</td>
                </tr>
                {{end}}
              </tbody>
            </table>
          </div>
        </section>
        {{end}}
      </div>
    </section>

    <section class="card" aria-labelledby="findings-heading">
      <h2 id="findings-heading">Findings</h2>
      {{if .Findings}}
      <div class="table-scroll">
        <table class="findings-table">
          <caption>Findings ranked by estimated recoverable seconds</caption>
          <thead>
            <tr>
              <th scope="col" class="numeric">Rank</th>
              <th scope="col">Phase</th>
              <th scope="col" class="numeric">Observed</th>
              <th scope="col" class="numeric">Recoverable</th>
              <th scope="col">Confidence</th>
              <th scope="col">Finding</th>
              <th scope="col">Recommendation</th>
            </tr>
          </thead>
          <tbody>
            {{range $index, $finding := .Findings}}
            <tr>
              <td class="numeric">{{add $index 1}}</td>
              <td>{{$finding.Phase}}</td>
              <td class="numeric">{{duration $finding.ObservedSecs}}</td>
              <td class="numeric">{{duration $finding.EstRecoverableSecs}}</td>
              <td>{{confidence $finding.Confidence}}</td>
              <td>{{findingBody $finding}}</td>
              <td>{{$finding.Recommendation}}</td>
            </tr>
            {{end}}
          </tbody>
        </table>
      </div>
      {{else}}<p>No findings.</p>{{end}}
    </section>

    <section class="card" aria-labelledby="attempts-heading">
      <h2 id="attempts-heading">Attempts and crash loss</h2>
      {{if .Attempts}}
      <div class="table-scroll">
        <table>
          <caption>Container startup attempts</caption>
          <thead>
            <tr>
              <th scope="col" class="numeric">Attempt</th>
              <th scope="col">Outcome</th>
              <th scope="col">Start</th>
              <th scope="col">End</th>
              <th scope="col">Crash reason</th>
              <th scope="col" class="numeric">Time lost</th>
            </tr>
          </thead>
          <tbody>
            {{range .Attempts}}
            <tr>
              <td class="numeric">{{.Index}}</td>
              <td>{{.Outcome}}</td>
              <td>{{.Start}}</td>
              <td>{{.End}}</td>
              <td>{{.CrashReason}}</td>
              <td class="numeric">{{.Lost}}</td>
            </tr>
            {{end}}
          </tbody>
        </table>
      </div>
      {{else}}<p>No attempt details reported.</p>{{end}}
    </section>

    <section class="card" aria-labelledby="warnings-heading">
      <h2 id="warnings-heading">Warnings</h2>
      {{if .Warnings}}
      <ul class="warning-list">
        {{range .Warnings}}<li>{{.}}</li>{{end}}
      </ul>
      {{else}}<p>No warnings.</p>{{end}}
    </section>
  </main>
  <footer class="report-footer muted">Generated by coldstart.</footer>
  <script>
    (() => {
      "use strict";
      const search = document.getElementById("phase-filter");
      const attempt = document.getElementById("attempt-filter");
      const confidence = document.getElementById("confidence-filter");
      const reset = document.getElementById("reset-filters");
      const status = document.getElementById("filter-status");
      const empty = document.getElementById("filter-empty");
      const timelines = Array.from(document.querySelectorAll("[data-timeline]"));
      const rows = Array.from(document.querySelectorAll("[data-phase]"));
      const total = rows.length;

      function applyFilters() {
        const query = search.value.trim().toLowerCase();
        const selectedAttempt = attempt.value;
        const selectedConfidence = confidence.value;
        let visible = 0;

        timelines.forEach((timeline) => {
          let visibleInTimeline = 0;
          timeline.querySelectorAll("[data-phase]").forEach((row) => {
            const matchesSearch = !query || row.dataset.search.toLowerCase().includes(query);
            const matchesAttempt = selectedAttempt === "all" || timeline.dataset.timeline === selectedAttempt;
            const matchesConfidence = selectedConfidence === "all" || row.dataset.confidence === selectedConfidence;
            const show = matchesSearch && matchesAttempt && matchesConfidence;
            row.hidden = !show;
            if (show) {
              visible++;
              visibleInTimeline++;
            }
          });
          timeline.hidden = visibleInTimeline === 0;
        });

        empty.hidden = visible !== 0;
        status.textContent = visible === 0
          ? "No matching phases."
          : "Showing " + visible + " of " + total + " phases.";
      }

      search.addEventListener("input", applyFilters);
      attempt.addEventListener("change", applyFilters);
      confidence.addEventListener("change", applyFilters);
      reset.addEventListener("click", () => {
        search.value = "";
        attempt.value = "all";
        confidence.value = "all";
        applyFilters();
        search.focus();
      });
      applyFilters();
    })();
  </script>
</body>
</html>
`
