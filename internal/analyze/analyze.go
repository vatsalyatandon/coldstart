// Package analyze turns a normalized bundle into a deterministic report. It
// performs no network or filesystem reads other than loading rule packs.
package analyze

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/vatsalyatandon/coldstart/internal/findings"
	"github.com/vatsalyatandon/coldstart/internal/model"
	"github.com/vatsalyatandon/coldstart/internal/rules"
)

type Options struct {
	RuleDir   string
	Container string
}

type candidate struct {
	phase    model.Phase
	priority int
}

var (
	durationRE = regexp.MustCompile(`(?i)(?:in|took|after)\s+([0-9]+(?:\.[0-9]+)?(?:ms|us|ns|h|m|s))+`)
	sizeRE     = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*(GiB|GB|MiB|MB|KiB|KB|B)\b`)
)

// Analyze is the pure core entry point. All attribution decisions are derived
// from the bundle and selected rule packs.
func Analyze(bundle model.Bundle, opts Options) (model.Report, error) {
	if bundle.Schema != "" && bundle.Schema != model.BundleSchema {
		return model.Report{}, fmt.Errorf("unsupported bundle schema %q", bundle.Schema)
	}
	t0 := bundle.Pod.CreationTimestamp.Time.UTC()
	if t0.IsZero() {
		return model.Report{}, fmt.Errorf("pod creationTimestamp is required")
	}
	readyAt := conditionTime(bundle.Pod.Status.Conditions, corev1.PodReady, corev1.ConditionTrue)
	capturedAt := bundle.CapturedAt.UTC()
	if capturedAt.IsZero() {
		capturedAt = latestBundleTime(bundle, t0)
		if readyAt != nil && readyAt.After(capturedAt) {
			capturedAt = *readyAt
		}
	}

	report := model.Report{
		Schema:     model.ReportSchema,
		Pod:        bundle.Pod.Namespace + "/" + bundle.Pod.Name,
		T0:         t0,
		CapturedAt: capturedAt,
		Warnings:   append([]string(nil), bundle.Notes...),
	}
	report.ReadyAt = readyAt
	if bundle.CapturedAt.IsZero() {
		report.Warnings = append(report.Warnings, "bundle capturedAt missing; inferred capture time from the latest input timestamp")
	}
	end := capturedAt
	if readyAt != nil && !readyAt.Before(t0) {
		end = *readyAt
	} else if readyAt != nil {
		report.Warnings = append(report.Warnings, "Ready condition predates pod creation; using capture time")
	}
	if bundle.Probe != nil && !bundle.Probe.FirstTokenAt.IsZero() && bundle.Probe.FirstTokenAt.After(end) {
		end = bundle.Probe.FirstTokenAt.UTC()
	}
	if end.Before(t0) {
		end = t0
	}
	report.EndAt = timePtr(end)
	report.TotalSecs = end.Sub(t0).Seconds()
	if readyAt == nil {
		report.Warnings = append(report.Warnings, "pod has no Ready=True condition; timeline ends at bundle capture time")
	}
	if capturedAt.Sub(t0) > 50*time.Minute {
		report.Warnings = append(report.Warnings, "events older than 1h may have expired; durable pod/status timestamps were used where possible")
	}

	report.Hardware = hardware(bundle, t0, readyAt)
	packs, err := rules.Load(opts.RuleDir)
	if err != nil {
		return model.Report{}, err
	}
	detectionBundle := bundle
	if opts.Container != "" {
		logs := make([]model.LogCapture, 0, len(bundle.Logs))
		for _, capture := range bundle.Logs {
			if capture.Container == opts.Container {
				logs = append(logs, capture)
			}
		}
		detectionBundle.Logs = logs
	}
	detection := rules.Detect(detectionBundle, packs)
	if opts.Container != "" {
		detection.Container = opts.Container
	}
	if detection.Container == "" {
		detection.Container = primaryContainerName(bundle.Pod)
	}
	if detection.Version == "" {
		detection.Version = versionFromContainer(bundle.Pod, detection.Container)
	}
	report.Engine = model.EngineInfo{Name: detection.Engine, Version: detection.Version, Container: detection.Container}

	selection, ok := rules.Select(packs, detection)
	if !ok {
		report.Warnings = append(report.Warnings, "engine not detected; only Kubernetes phases can be attributed")
	}
	if ok {
		report.RulePack = model.RulePackInfo{Engine: selection.Pack.Engine, Version: selection.Pack.Versions, Path: selection.Pack.Path, Warning: selection.Warning}
		report.Engine.RulePack = selection.Pack.Path
		if selection.Warning != "" {
			report.Warnings = append(report.Warnings, selection.Warning)
		}
	}

	status := containerStatus(bundle.Pod, detection.Container)
	containerStart := statusStart(status)
	lastInitFinish := lastInitFinish(bundle.Pod)
	if containerStart == nil && lastInitFinish != nil {
		containerStart = lastInitFinish
	}
	lines := logLines(bundle, detection.Container, false, time.Time{}, time.Time{})
	firstLog := firstTimestamp(lines)
	match := rules.MatchResult{Phases: map[string]rules.PhaseMatch{}, Flags: map[string]bool{}, Attrs: map[string]float64{}}
	if ok {
		match = rules.Match(lines, selection.Pack)
		if download := match.Phases["weights_download"]; download.Start != nil && download.End == nil {
			if load := match.Phases["weights_load"]; load.Start != nil && load.Start.After(*download.Start) {
				download.End = load.Start
				match.Phases["weights_download"] = download
			}
		}
		for key, value := range match.Attrs {
			if strings.HasPrefix(key, "weights") {
				if match.Attrs["weights_bytes"] < value {
					match.Attrs["weights_bytes"] = value
				}
			}
		}
	}

	candidates := kubernetesCandidates(bundle, t0, end, &report)
	candidates = append(candidates, engineCandidates(lines, match, firstLog, containerStart, lastInitFinish, readyAt, &end)...)
	probePeriod := readinessProbePeriod(bundle.Pod, detection.Container)
	for i := range candidates {
		if candidates[i].phase.ID == "readiness_lag" {
			if candidates[i].phase.Attrs == nil {
				candidates[i].phase.Attrs = make(map[string]any)
			}
			candidates[i].phase.Attrs["probe_period_secs"] = float64(probePeriod)
		}
	}
	if bundle.Probe != nil && !bundle.Probe.RequestSentAt.IsZero() && !bundle.Probe.FirstTokenAt.IsZero() {
		start := bundle.Probe.RequestSentAt.UTC()
		finish := bundle.Probe.FirstTokenAt.UTC()
		if finish.After(start) {
			candidates = append(candidates, makeCandidate("first_token", "first token", start, finish, "probe", "high", map[string]any{"model": bundle.Probe.Model}, 60))
		}
	}

	// Failed attempts are shown as crash loss in the overall timeline. Their
	// engine details are reconstructed separately in Attempts below.
	restartIntervals := failedAttemptIntervals(bundle, status, t0, containerStart)
	for _, interval := range restartIntervals {
		candidates = append(candidates, makeCandidate("restart_attempt", "failed attempt / crash", interval.start, interval.end, "status", "high", interval.attrs, 70))
	}

	report.Phases, report.UnattributedSecs = normalize(t0, end, candidates, &report.Warnings)
	if len(report.Phases) == 1 && report.Phases[0].ID == "unattributed" {
		report.Warnings = append(report.Warnings, "no startup phases could be attributed")
	}
	if report.UnattributedSecs > 0.10*report.TotalSecs && report.TotalSecs > 0 {
		report.Warnings = append(report.Warnings, "more than 10% of startup is unattributed; capture a bundle with -o bundle for investigation")
	}

	report.Attempts = buildAttempts(bundle, detection.Container, status, t0, end, readyAt, report.Phases, selection, ok)
	report.Findings = findings.Build(report, selection, match.Flags)
	report.Probe = normalizeProbe(bundle.Probe)
	return report, nil
}

func kubernetesCandidates(bundle model.Bundle, t0, end time.Time, report *model.Report) []candidate {
	var out []candidate
	events := append([]corev1.Event(nil), bundle.Events...)
	sort.SliceStable(events, func(i, j int) bool { return eventTime(events[i]).Before(eventTime(events[j])) })

	scheduled := conditionTime(bundle.Pod.Status.Conditions, corev1.PodScheduled, corev1.ConditionTrue)
	if scheduled != nil && scheduled.After(t0) && scheduled.Before(end) {
		nodeReady := nodeReadyTime(bundle.Node)
		provisionStart := t0
		provisioning := false
		for _, event := range events {
			reason := strings.ToLower(event.Reason)
			if strings.Contains(reason, "triggeredscaleup") || strings.Contains(reason, "nominated") || strings.Contains(reason, "scalingup") {
				at := eventTime(event)
				if !at.Before(t0) && at.Before(*scheduled) {
					if at.After(provisionStart) {
						provisionStart = at
					}
					provisioning = true
				}
			}
		}
		if bundle.Node != nil && !bundle.Node.CreationTimestamp.IsZero() && bundle.Node.CreationTimestamp.Time.After(t0) && bundle.Node.CreationTimestamp.Time.Before(*scheduled) {
			if bundle.Node.CreationTimestamp.Time.After(provisionStart) {
				provisionStart = bundle.Node.CreationTimestamp.Time
			}
			provisioning = true
		}
		cursor := t0
		if provisioning && nodeReady != nil && nodeReady.After(provisionStart) && nodeReady.Before(*scheduled) {
			if provisionStart.After(cursor) {
				out = append(out, makeCandidate("scheduling_wait", "scheduling wait", cursor, provisionStart, "status", "high", nil, 20))
			}
			out = append(out, makeCandidate("node_provisioning", "↳ node provisioning", provisionStart, *nodeReady, "event+node", "medium", nil, 30).withParent("scheduling_wait"))
			cursor = *nodeReady
			if gpuReady := gpuAllocatableTime(bundle, *nodeReady, *scheduled); gpuReady != nil && gpuReady.After(cursor) {
				out = append(out, makeCandidate("gpu_allocatable", "↳ GPU becomes allocatable", cursor, *gpuReady, "operator pod", "medium", nil, 30).withParent("scheduling_wait"))
				cursor = *gpuReady
			}
		}
		if scheduled.After(cursor) {
			out = append(out, makeCandidate("scheduling_wait", "scheduling wait", cursor, *scheduled, "status", "high", nil, 20))
		}
	} else if scheduled == nil {
		report.Warnings = append(report.Warnings, "PodScheduled=True transition not found; scheduling wait may be incomplete")
	}

	addImagePull := func(start, finish time.Time, message, imageMessage string) {
		if !finish.After(start) {
			return
		}
		attrs := map[string]any{"cached": strings.Contains(message, "already present") || strings.Contains(message, "already exists") || strings.Contains(message, "cached")}
		if image := imageFromMessage(imageMessage); image != "" {
			attrs["image"] = image
		}
		if size := parseSize(imageMessage); size > 0 {
			attrs["image_bytes"] = size
		}
		out = append(out, makeCandidate("image_pull", "image pull", start, finish, "event", "high", attrs, 30))
	}

	var pulling *corev1.Event
	for i := range events {
		event := &events[i]
		reason := strings.ToLower(event.Reason)
		message := strings.ToLower(event.Message)
		if (reason == "pulling" || strings.Contains(message, "pulling image")) && pulling == nil {
			pulling = event
			continue
		}
		pulled := reason == "pulled" || strings.Contains(message, "pulled image") || strings.Contains(message, "successfully pulled")
		if !pulled {
			continue
		}
		if pulling != nil {
			addImagePull(eventTime(*pulling), eventTime(*event), message, event.Message)
			pulling = nil
			continue
		}
		// If the Pulling event expired but Pulled retained an explicit
		// duration, reconstruct the interval from the durable end event.
		if secs := parseEventDuration(event.Message); secs > 0 {
			finish := eventTime(*event)
			addImagePull(finish.Add(-time.Duration(secs*float64(time.Second))), finish, message, event.Message)
		}
	}
	if pulling != nil {
		report.Warnings = append(report.Warnings, "image Pulling event has no matching Pulled event; image pull duration may be incomplete")
	}

	for i := range bundle.Pod.Status.InitContainerStatuses {
		status := bundle.Pod.Status.InitContainerStatuses[i]
		terminated := status.State.Terminated
		if terminated == nil || terminated.StartedAt.IsZero() || terminated.FinishedAt.IsZero() || !terminated.FinishedAt.After(terminated.StartedAt.Time) {
			continue
		}
		attrs := map[string]any{"container": status.Name}
		if terminated.ExitCode != 0 {
			attrs["exit_code"] = terminated.ExitCode
		}
		remote := initContainerRemote(bundle.Pod, status.Name)
		if remote {
			attrs["remote_download"] = true
		}
		out = append(out, makeCandidate("init_containers", "init containers", terminated.StartedAt.Time, terminated.FinishedAt.Time, "status", "high", attrs, 35))
	}
	return out
}

func engineCandidates(lines []model.LogLine, match rules.MatchResult, firstLog, containerStart, lastInitFinish, readyAt, end *time.Time) []candidate {
	var out []candidate
	if len(lines) == 0 || firstLog == nil {
		if containerStart != nil && containerStart.Before(*end) {
			reason := "no engine logs"
			if len(lines) > 0 {
				reason = "no timestamped engine logs"
			}
			out = append(out, makeCandidate("pre_engine_unknown", "pre-engine (unknown)", *containerStart, *end, "status", "low", map[string]any{"reason": reason}, 10))
		}
		return out
	}

	// Find the first explicit engine phase boundary. Bootstrap is whatever
	// remains between the first engine log and that boundary.
	var boundaries []time.Time
	for _, id := range []string{"weights_download", "weights_load", "torch_compile", "cuda_graph"} {
		if phase := match.Phases[id]; phase.Start != nil {
			boundaries = append(boundaries, *phase.Start)
		}
	}
	if len(boundaries) > 0 {
		boundary := minTime(boundaries)
		if boundary.After(*firstLog) {
			out = append(out, makeCandidate("framework_bootstrap", "framework bootstrap", *firstLog, boundary, "log", "high", nil, 40))
		}
	}

	if containerStart != nil {
		start := *containerStart
		if lastInitFinish != nil && lastInitFinish.After(start) {
			start = *lastInitFinish
		}
		if firstLog.After(start) {
			out = append(out, makeCandidate("container_boot", "container start → engine boot", start, *firstLog, "status+log", "medium", nil, 25))
		}
	}

	for _, item := range []struct {
		id, name string
		priority int
	}{
		{"weights_download", "weights download", 50},
		{"weights_load", "weights load", 50},
		{"torch_compile", "torch.compile", 50},
		{"cuda_graph", "CUDA graph capture", 50},
	} {
		phase := match.Phases[item.id]
		if phase.Start == nil || phase.End == nil || !phase.End.After(*phase.Start) {
			continue
		}
		start, finish := *phase.Start, *phase.End
		attrs := map[string]any{}
		if phase.ReportedSecs != nil {
			attrs["engine_reported_secs"] = *phase.ReportedSecs
			timestampSecs := finish.Sub(start).Seconds()
			if timestampSecs > 0 && math.Abs(timestampSecs-*phase.ReportedSecs) > 0.01 {
				attrs["timestamp_secs"] = timestampSecs
			}
			// Engine-reported durations are preferred. Anchor them at the end
			// marker so the timeline remains contiguous and never negative.
			start = finish.Add(-time.Duration(*phase.ReportedSecs * float64(time.Second)))
		}
		if item.id == "torch_compile" {
			if match.Flags["compile_cache_hit"] {
				attrs["cache"] = "hit"
			} else if match.Flags["compile_cache_miss"] {
				attrs["cache"] = "miss"
			} else {
				attrs["cache"] = "unknown"
			}
		}
		if item.id == "weights_load" {
			if bytes, ok := match.Attrs["weights_bytes"]; ok {
				attrs["bytes"] = bytes
			}
			attrs["storage"] = ""
		}
		out = append(out, makeCandidate(item.id, item.name, start, finish, "log", "high", attrs, item.priority))
	}

	var finished []time.Time
	for _, id := range []string{"weights_load", "torch_compile", "cuda_graph"} {
		if phase := match.Phases[id]; phase.End != nil {
			finished = append(finished, *phase.End)
		}
	}
	engineInitEnd := (*time.Time)(nil)
	if phase := match.Phases["engine_init"]; phase.End != nil {
		engineInitEnd = phase.End
	}
	serverReady := (*time.Time)(nil)
	if phase := match.Phases["server_ready"]; phase.End != nil {
		serverReady = phase.End
	}

	kvStart := (*time.Time)(nil)
	if len(finished) > 0 {
		value := maxTime(finished)
		kvStart = &value
	}
	if kvStart != nil && engineInitEnd != nil && engineInitEnd.After(*kvStart) {
		out = append(out, makeCandidate("kv_cache_warmup", "KV cache / warmup", *kvStart, *engineInitEnd, "log", "medium", nil, 45))
	}
	if serverReady != nil {
		apiStart := engineInitEnd
		if apiStart == nil {
			apiStart = kvStart
		}
		if apiStart == nil {
			apiStart = firstLog
		}
		if serverReady.After(*apiStart) {
			out = append(out, makeCandidate("api_server_up", "API server up", *apiStart, *serverReady, "log", "high", nil, 45))
		}
		if readyAt != nil && readyAt.After(*serverReady) {
			out = append(out, makeCandidate("readiness_lag", "readiness lag", *serverReady, *readyAt, "log+status", "high", map[string]any{"probe_period_secs": 30.0}, 55))
		}
	}
	return out
}

func normalize(start, end time.Time, input []candidate, warnings *[]string) ([]model.Phase, float64) {
	if !end.After(start) {
		return nil, 0
	}
	sort.SliceStable(input, func(i, j int) bool {
		if input[i].phase.Start == nil || input[j].phase.Start == nil {
			return input[i].priority > input[j].priority
		}
		if !input[i].phase.Start.Equal(*input[j].phase.Start) {
			return input[i].phase.Start.Before(*input[j].phase.Start)
		}
		if input[i].priority != input[j].priority {
			return input[i].priority > input[j].priority
		}
		return input[i].phase.ID < input[j].phase.ID
	})
	var phases []model.Phase
	unattributed := 0.0
	cursor := start
	for _, item := range input {
		if item.phase.Start == nil || item.phase.End == nil {
			continue
		}
		phaseStart, phaseEnd := *item.phase.Start, *item.phase.End
		if phaseStart.Before(start) || phaseEnd.After(end) {
			*warnings = append(*warnings, fmt.Sprintf("clock skew clamped in %s phase", item.phase.ID))
		}
		if phaseStart.Before(start) {
			phaseStart = start
		}
		if phaseEnd.After(end) {
			phaseEnd = end
		}
		if phaseStart.Before(cursor) {
			if phaseStart.Before(start) {
				// Out-of-window skew was already reported above.
			} else {
				*warnings = append(*warnings, fmt.Sprintf("overlapping markers clamped in %s phase", item.phase.ID))
			}
			phaseStart = cursor
		}
		if !phaseEnd.After(phaseStart) {
			continue
		}
		if phaseStart.After(cursor) {
			gap := phaseStart.Sub(cursor).Seconds()
			if gap > 0.001 {
				phases = append(phases, model.Phase{ID: "unattributed", Name: "unattributed", Start: timePtr(cursor), End: timePtr(phaseStart), Secs: gap, Source: "gap", Confidence: "low", Attrs: map[string]any{"reason": "no matching marker"}})
				unattributed += gap
			}
		}
		secs := phaseEnd.Sub(phaseStart).Seconds()
		phase := item.phase
		phase.Start, phase.End, phase.Secs = timePtr(phaseStart), timePtr(phaseEnd), secs
		phases = append(phases, phase)
		cursor = phaseEnd
	}
	if cursor.Before(end) {
		gap := end.Sub(cursor).Seconds()
		if gap > 0.001 {
			phases = append(phases, model.Phase{ID: "unattributed", Name: "unattributed", Start: timePtr(cursor), End: timePtr(end), Secs: gap, Source: "gap", Confidence: "low", Attrs: map[string]any{"reason": "no matching marker"}})
			unattributed += gap
		}
	}
	return phases, unattributed
}

func buildAttempts(bundle model.Bundle, container string, status corev1.ContainerStatus, t0, end time.Time, readyAt *time.Time, overall []model.Phase, selection rules.Selection, hasRules bool) []model.Attempt {
	restartCount := int(status.RestartCount)
	currentStart := statusStart(status)
	if currentStart == nil {
		currentStart = &t0
	}
	outcome := "running"
	if readyAt != nil {
		outcome = "ready"
	} else if status.State.Terminated != nil {
		if status.State.Terminated.ExitCode == 0 {
			outcome = "exited"
		} else {
			outcome = "failed"
		}
	}

	attempts := make([]model.Attempt, 0, restartCount+1)
	if restartCount == 0 {
		lost := 0.0
		if outcome == "failed" && currentStart != nil && end.After(*currentStart) {
			lost = end.Sub(*currentStart).Seconds()
		}
		attempts = append(attempts, model.Attempt{Index: 0, Outcome: outcome, Start: &t0, End: &end, LostSecs: lost, Phases: clonePhases(overall)})
		return attempts
	}

	// Current attempt.
	currentPhases := intersectPhases(overall, *currentStart, end)
	if len(currentPhases) == 0 {
		currentPhases, _ = normalize(*currentStart, end, nil, &[]string{})
	}
	currentLost := 0.0
	if outcome == "failed" && end.After(*currentStart) {
		currentLost = end.Sub(*currentStart).Seconds()
	}
	attempts = append(attempts, model.Attempt{Index: restartCount, Outcome: outcome, Start: currentStart, End: &end, CrashReason: terminationReason(status.State.Terminated), LostSecs: currentLost, Phases: currentPhases})

	previous := status.LastTerminationState.Terminated
	if previous != nil && !previous.StartedAt.IsZero() && !previous.FinishedAt.IsZero() && previous.FinishedAt.After(previous.StartedAt.Time) {
		start, finish := previous.StartedAt.Time.UTC(), previous.FinishedAt.Time.UTC()
		phases := previousAttemptPhases(bundle, container, selection, hasRules, start, finish)
		attempts = append(attempts, model.Attempt{Index: restartCount - 1, Outcome: "failed", Start: &start, End: &finish, CrashReason: terminationReason(previous), LostSecs: finish.Sub(start).Seconds(), Phases: phases})
		if restartCount > 1 {
			unknownEnd := start
			phases, _ := normalize(t0, unknownEnd, []candidate{makeCandidate("restart_attempt", "earlier failed attempts (details unavailable)", t0, unknownEnd, "status", "medium", map[string]any{"attempts": restartCount - 1}, 50)}, &[]string{})
			attempts = append(attempts, model.Attempt{Index: 0, Outcome: "failed", Start: &t0, End: &unknownEnd, CrashReason: "earlier attempt details unavailable", LostSecs: unknownEnd.Sub(t0).Seconds(), Phases: phases})
		}
	} else if currentStart.After(t0) {
		unknownEnd := *currentStart
		phases, _ := normalize(t0, unknownEnd, []candidate{makeCandidate("restart_attempt", "failed attempt (details unavailable)", t0, unknownEnd, "status", "medium", map[string]any{"attempts": restartCount}, 50)}, &[]string{})
		attempts = append(attempts, model.Attempt{Index: 0, Outcome: "failed", Start: &t0, End: &unknownEnd, CrashReason: "previous termination state unavailable", LostSecs: unknownEnd.Sub(t0).Seconds(), Phases: phases})
	}
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].Index < attempts[j].Index })
	return attempts
}

func previousAttemptPhases(bundle model.Bundle, container string, selection rules.Selection, hasRules bool, start, end time.Time) []model.Phase {
	lines := logLines(bundle, container, true, start, end)
	var candidates []candidate
	if hasRules {
		match := rules.Match(lines, selection.Pack)
		candidates = engineCandidates(lines, match, firstTimestamp(lines), &start, nil, nil, &end)
	}
	phases, _ := normalize(start, end, candidates, &[]string{})
	return phases
}

type failedInterval struct {
	start, end time.Time
	attrs      map[string]any
}

func failedAttemptIntervals(bundle model.Bundle, status corev1.ContainerStatus, t0 time.Time, currentStart *time.Time) []failedInterval {
	if status.RestartCount == 0 {
		return nil
	}
	var out []failedInterval
	if previous := status.LastTerminationState.Terminated; previous != nil && !previous.StartedAt.IsZero() && !previous.FinishedAt.IsZero() && previous.FinishedAt.After(previous.StartedAt.Time) {
		start, end := previous.StartedAt.Time.UTC(), previous.FinishedAt.Time.UTC()
		out = append(out, failedInterval{start: start, end: end, attrs: map[string]any{"reason": terminationReason(previous), "exit_code": previous.ExitCode}})
		if status.RestartCount > 1 && start.After(t0) {
			out = append(out, failedInterval{start: t0, end: start, attrs: map[string]any{"reason": "earlier attempts unavailable", "attempts": int(status.RestartCount) - 1}})
		}
	} else if currentStart != nil && currentStart.After(t0) {
		out = append(out, failedInterval{start: t0, end: *currentStart, attrs: map[string]any{"reason": "previous termination state unavailable"}})
	}
	return out
}

func intersectPhases(phases []model.Phase, start, end time.Time) []model.Phase {
	var out []model.Phase
	for _, phase := range phases {
		if phase.Start == nil || phase.End == nil || !phase.End.After(start) || !phase.Start.Before(end) {
			continue
		}
		copy := phase
		if copy.Start.Before(start) {
			copy.Start = timePtr(start)
		}
		if copy.End.After(end) {
			copy.End = timePtr(end)
		}
		copy.Secs = copy.End.Sub(*copy.Start).Seconds()
		out = append(out, copy)
	}
	return out
}

func clonePhases(phases []model.Phase) []model.Phase {
	out := make([]model.Phase, len(phases))
	copy(out, phases)
	return out
}

func makeCandidate(id, name string, start, end time.Time, source, confidence string, attrs map[string]any, priority int) candidate {
	return candidate{phase: model.Phase{ID: id, Name: name, Start: timePtr(start), End: timePtr(end), Secs: end.Sub(start).Seconds(), Source: source, Confidence: confidence, Attrs: attrs}, priority: priority}
}

func (c candidate) withParent(parent string) candidate {
	c.phase.ParentID = parent
	return c
}

func conditionTime(conditions []corev1.PodCondition, conditionType corev1.PodConditionType, status corev1.ConditionStatus) *time.Time {
	for _, condition := range conditions {
		if condition.Type == conditionType && condition.Status == status && !condition.LastTransitionTime.IsZero() {
			value := condition.LastTransitionTime.Time.UTC()
			return &value
		}
	}
	return nil
}

func nodeReadyTime(node *corev1.Node) *time.Time {
	if node == nil {
		return nil
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue && !condition.LastTransitionTime.IsZero() {
			value := condition.LastTransitionTime.Time.UTC()
			return &value
		}
	}
	return nil
}

func gpuAllocatableTime(bundle model.Bundle, nodeReady, scheduled time.Time) *time.Time {
	if bundle.Node == nil {
		return nil
	}
	gpu := bundle.Node.Status.Allocatable["nvidia.com/gpu"]
	if gpu.IsZero() {
		return nil
	}
	var readyTimes []time.Time
	for _, pod := range bundle.OperatorPods {
		if pod.Spec.NodeName != bundle.Node.Name || !isGPUPod(pod) {
			continue
		}
		if at := conditionTime(pod.Status.Conditions, corev1.PodReady, corev1.ConditionTrue); at != nil && at.After(nodeReady) && at.Before(scheduled) {
			readyTimes = append(readyTimes, *at)
		}
	}
	if len(readyTimes) == 0 {
		return nil
	}
	return timePtr(maxTime(readyTimes))
}

func isGPUPod(pod corev1.Pod) bool {
	text := strings.ToLower(pod.Name + " " + pod.Namespace)
	for _, token := range []string{"gpu-operator", "device-plugin", "amd-device-plugin"} {
		if strings.Contains(text, token) {
			return true
		}
	}
	return false
}

func hardware(bundle model.Bundle, t0 time.Time, readyAt *time.Time) model.Hardware {
	hw := model.Hardware{Node: bundle.Pod.Spec.NodeName}
	if bundle.Node == nil {
		return hw
	}
	nvidiaGPU := bundle.Node.Status.Allocatable["nvidia.com/gpu"]
	amdGPU := bundle.Node.Status.Allocatable["amd.com/gpu"]
	hw.Count = int(nvidiaGPU.Value())
	if hw.Count == 0 {
		hw.Count = int(amdGPU.Value())
	}
	labels := bundle.Node.Labels
	for _, key := range []string{"nvidia.com/gpu.product", "GPU_PRODUCT", "amd.com/gpu.device-name"} {
		if labels[key] != "" {
			hw.GPU = labels[key]
			break
		}
	}
	if hw.GPU == "" {
		capacity := bundle.Node.Status.Capacity["nvidia.com/gpu"]
		if capacity.Value() > 0 {
			hw.GPU = "NVIDIA GPU"
		}
	}
	allLabels := strings.ToLower(fmt.Sprint(labels))
	switch {
	case strings.Contains(allLabels, "nvme"), strings.Contains(allLabels, "local-ssd"):
		hw.Storage = "local-nvme"
	case strings.Contains(allLabels, "ebs"), strings.Contains(allLabels, "gcepersistentdisk"), strings.Contains(allLabels, "azure-disk"):
		hw.Storage = "network"
	}
	nodeReady := nodeReadyTime(bundle.Node)
	hw.NodeFresh = !bundle.Node.CreationTimestamp.IsZero() && bundle.Node.CreationTimestamp.Time.After(t0) && (readyAt == nil || bundle.Node.CreationTimestamp.Time.Before(*readyAt))
	if nodeReady != nil && readyAt != nil && nodeReady.Before(t0) {
		hw.NodeFresh = false
	}
	return hw
}

func readinessProbePeriod(pod corev1.Pod, containerName string) int32 {
	for _, container := range pod.Spec.Containers {
		if container.Name == containerName && container.ReadinessProbe != nil && container.ReadinessProbe.PeriodSeconds > 0 {
			return container.ReadinessProbe.PeriodSeconds
		}
	}
	return 30
}

func primaryContainerName(pod corev1.Pod) string {
	if len(pod.Spec.Containers) == 0 {
		return ""
	}
	return pod.Spec.Containers[0].Name
}

func containerStatus(pod corev1.Pod, container string) corev1.ContainerStatus {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == container {
			return status
		}
	}
	if len(pod.Status.ContainerStatuses) > 0 {
		return pod.Status.ContainerStatuses[0]
	}
	return corev1.ContainerStatus{}
}

func statusStart(status corev1.ContainerStatus) *time.Time {
	if status.State.Running != nil && !status.State.Running.StartedAt.IsZero() {
		value := status.State.Running.StartedAt.Time.UTC()
		return &value
	}
	if status.State.Terminated != nil && !status.State.Terminated.StartedAt.IsZero() {
		value := status.State.Terminated.StartedAt.Time.UTC()
		return &value
	}
	return nil
}

func lastInitFinish(pod corev1.Pod) *time.Time {
	var latest *time.Time
	for _, status := range pod.Status.InitContainerStatuses {
		if status.State.Terminated != nil && !status.State.Terminated.FinishedAt.IsZero() {
			value := status.State.Terminated.FinishedAt.Time.UTC()
			if latest == nil || value.After(*latest) {
				latest = &value
			}
		}
	}
	return latest
}

func logLines(bundle model.Bundle, container string, previous bool, start, end time.Time) []model.LogLine {
	var lines []model.LogLine
	for _, capture := range bundle.Logs {
		if capture.Previous != previous {
			continue
		}
		if container != "" && capture.Container != container {
			continue
		}
		for _, line := range capture.Lines {
			if !start.IsZero() && line.Timestamp.Before(start) {
				continue
			}
			if !end.IsZero() && line.Timestamp.After(end) {
				continue
			}
			lines = append(lines, model.LogLine{Timestamp: line.Timestamp.UTC(), Text: line.Text})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Timestamp.Before(lines[j].Timestamp) })
	return lines
}

func firstTimestamp(lines []model.LogLine) *time.Time {
	for _, line := range lines {
		if !line.Timestamp.IsZero() {
			value := line.Timestamp
			return &value
		}
	}
	return nil
}

func latestBundleTime(bundle model.Bundle, fallback time.Time) time.Time {
	latest := fallback.UTC()
	consider := func(value time.Time) {
		if !value.IsZero() && value.After(latest) {
			latest = value.UTC()
		}
	}
	considerStatus := func(status corev1.ContainerStatus) {
		if status.State.Running != nil {
			consider(status.State.Running.StartedAt.Time)
		}
		if status.State.Terminated != nil {
			consider(status.State.Terminated.StartedAt.Time)
			consider(status.State.Terminated.FinishedAt.Time)
		}
		if status.LastTerminationState.Terminated != nil {
			consider(status.LastTerminationState.Terminated.StartedAt.Time)
			consider(status.LastTerminationState.Terminated.FinishedAt.Time)
		}
	}
	for _, condition := range bundle.Pod.Status.Conditions {
		consider(condition.LastTransitionTime.Time)
	}
	for _, status := range bundle.Pod.Status.InitContainerStatuses {
		considerStatus(status)
	}
	for _, status := range bundle.Pod.Status.ContainerStatuses {
		considerStatus(status)
	}
	for _, event := range bundle.Events {
		consider(eventTime(event))
	}
	for _, capture := range bundle.Logs {
		for _, line := range capture.Lines {
			consider(line.Timestamp)
		}
	}
	if bundle.Node != nil {
		consider(bundle.Node.CreationTimestamp.Time)
		if ready := nodeReadyTime(bundle.Node); ready != nil {
			consider(*ready)
		}
	}
	if bundle.Probe != nil {
		consider(bundle.Probe.RequestSentAt)
		consider(bundle.Probe.FirstTokenAt)
	}
	return latest
}

func eventTime(event corev1.Event) time.Time {
	if !event.EventTime.IsZero() {
		return event.EventTime.Time.UTC()
	}
	if !event.FirstTimestamp.IsZero() {
		return event.FirstTimestamp.Time.UTC()
	}
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time.UTC()
	}
	return event.CreationTimestamp.Time.UTC()
}

func imageFromMessage(message string) string {
	re := regexp.MustCompile(`(?i)(?:pulling|pulled)\s+(?:image\s+)?["']?([^\s"']+)`)
	if match := re.FindStringSubmatch(message); len(match) > 1 {
		return match[1]
	}
	return ""
}

func parseSize(message string) float64 {
	match := sizeRE.FindStringSubmatch(message)
	if len(match) < 3 {
		return 0
	}
	value, _ := strconv.ParseFloat(match[1], 64)
	units := map[string]float64{"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "KiB": 1024, "MiB": 1024 * 1024, "GiB": 1024 * 1024 * 1024}
	return value * units[strings.ToUpper(match[2])]
}

func parseEventDuration(message string) float64 {
	match := durationRE.FindStringSubmatch(message)
	if len(match) < 2 {
		return 0
	}
	duration, err := time.ParseDuration(strings.ToLower(match[1]))
	if err != nil {
		return 0
	}
	return duration.Seconds()
}

func initContainerRemote(pod corev1.Pod, name string) bool {
	for _, container := range pod.Spec.InitContainers {
		if container.Name != name {
			continue
		}
		text := strings.ToLower(strings.Join(container.Command, " ") + " " + strings.Join(container.Args, " "))
		for _, token := range []string{"http://", "https://", "huggingface", "hf_hub", "snapshot_download", "s3://", "gs://", "download"} {
			if strings.Contains(text, token) {
				return true
			}
		}
	}
	return false
}

func versionFromContainer(pod corev1.Pod, containerName string) string {
	for _, container := range pod.Spec.Containers {
		if container.Name != containerName {
			continue
		}
		text := strings.Join(container.Args, " ") + " " + container.Image
		re := regexp.MustCompile(`(?i)(?:vllm|sglang)[^0-9]{0,20}([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)
		if match := re.FindStringSubmatch(text); len(match) > 1 {
			return match[1]
		}
		for _, env := range container.Env {
			if strings.Contains(strings.ToLower(env.Name), "version") {
				if match := re.FindStringSubmatch(env.Value); len(match) > 1 {
					return match[1]
				}
			}
		}
	}
	return ""
}

func terminationReason(terminated *corev1.ContainerStateTerminated) string {
	if terminated == nil {
		return ""
	}
	if terminated.Reason == "OOMKilled" {
		return "OOMKilled"
	}
	if terminated.Reason != "" {
		return terminated.Reason
	}
	return fmt.Sprintf("exit code %d", terminated.ExitCode)
}

func minTime(values []time.Time) time.Time {
	result := values[0]
	for _, value := range values[1:] {
		if value.Before(result) {
			result = value
		}
	}
	return result
}

func maxTime(values []time.Time) time.Time {
	result := values[0]
	for _, value := range values[1:] {
		if value.After(result) {
			result = value
		}
	}
	return result
}

func normalizeProbe(probe *model.ProbeResult) *model.ProbeResult {
	if probe == nil {
		return nil
	}
	copy := *probe
	copy.RequestSentAt = copy.RequestSentAt.UTC()
	copy.FirstTokenAt = copy.FirstTokenAt.UTC()
	return &copy
}

func timePtr(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}
