package collect

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

const logStartLookback = 5 * time.Second

type logContainerKey struct {
	name string
	init bool
}

type logContainer struct {
	container corev1.Container
	init      bool
}

func collectLogs(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod, opts Options, bundle *model.Bundle) {
	statuses := make(map[logContainerKey]corev1.ContainerStatus, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		statuses[logContainerKey{name: status.Name}] = status
	}
	for _, status := range pod.Status.InitContainerStatuses {
		statuses[logContainerKey{name: status.Name, init: true}] = status
	}

	containers := make([]logContainer, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	for _, container := range pod.Spec.Containers {
		containers = append(containers, logContainer{container: container})
	}
	for _, container := range pod.Spec.InitContainers {
		containers = append(containers, logContainer{container: container, init: true})
	}

	maxLogBytes := opts.MaxLogBytes
	if maxLogBytes <= 0 {
		maxLogBytes = DefaultMaxLogBytes
	}

	foundContainer := false
	for _, target := range containers {
		container := target.container
		if opts.Container != "" && container.Name != opts.Container {
			continue
		}
		foundContainer = true
		key := logContainerKey{name: container.Name, init: target.init}
		status, hasStatus := statuses[key]

		logOptions := corev1.PodLogOptions{
			Container:  container.Name,
			Timestamps: true,
			LimitBytes: &maxLogBytes,
		}
		logOptions.SinceTime = currentLogSinceTime(status, hasStatus, target.init)
		raw, truncated, err := readLogStream(ctx, client, pod, logOptions, maxLogBytes)
		if err != nil {
			bundle.Notes = append(bundle.Notes, fmt.Sprintf("logs for container %s unavailable: %v", container.Name, err))
		} else {
			lines, malformed := parseLogStream(raw)
			bundle.Logs = append(bundle.Logs, model.LogCapture{
				Container: container.Name,
				Init:      target.init,
				Truncated: truncated,
				Lines:     lines,
			})
			noteMalformedLogLines(bundle, container.Name, false, malformed)
			if truncated {
				noteLogTruncation(bundle, container.Name, false, maxLogBytes)
			}
			noteRotatedLogs(bundle, container.Name, false, logAttemptStart(status, hasStatus, false), lines)
		}

		if !opts.IncludePrevious || !hasStatus || status.RestartCount == 0 {
			continue
		}
		previousOptions := corev1.PodLogOptions{
			Container:  container.Name,
			Timestamps: true,
			Previous:   true,
			LimitBytes: &maxLogBytes,
		}
		previousOptions.SinceTime = previousLogSinceTime(status)
		previous, truncated, err := readLogStream(ctx, client, pod, previousOptions, maxLogBytes)
		if err != nil {
			bundle.Notes = append(bundle.Notes, fmt.Sprintf("previous logs for container %s unavailable: %v", container.Name, err))
			continue
		}
		lines, malformed := parseLogStream(previous)
		bundle.Logs = append(bundle.Logs, model.LogCapture{
			Container: container.Name,
			Init:      target.init,
			Previous:  true,
			Truncated: truncated,
			Lines:     lines,
		})
		noteMalformedLogLines(bundle, container.Name, true, malformed)
		if truncated {
			noteLogTruncation(bundle, container.Name, true, maxLogBytes)
		}
		noteRotatedLogs(bundle, container.Name, true, logAttemptStart(status, hasStatus, true), lines)
	}

	if opts.Container != "" && !foundContainer {
		bundle.Notes = append(bundle.Notes, fmt.Sprintf("container %s not found in pod %s", opts.Container, pod.Name))
	}
}

// readLogStream reads at most maxLogBytes from the stream. Reaching the cap is
// treated conservatively as truncation because the Kubernetes API may have
// applied LimitBytes without exposing whether the underlying log was longer.
func readLogStream(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod, opts corev1.PodLogOptions, maxLogBytes int64) ([]byte, bool, error) {
	stream, err := client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &opts).Stream(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		// The bounded read is complete; closing the stream cannot change the
		// captured bytes.
		_ = stream.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(stream, maxLogBytes))
	if err != nil {
		return nil, false, err
	}
	return raw, int64(len(raw)) >= maxLogBytes, nil
}

func currentLogSinceTime(status corev1.ContainerStatus, hasStatus, init bool) *metav1.Time {
	if !hasStatus {
		return nil
	}
	if init {
		if status.State.Terminated != nil && !status.State.Terminated.StartedAt.IsZero() {
			value := metav1.NewTime(status.State.Terminated.StartedAt.Add(-logStartLookback))
			return &value
		}
		return nil
	}
	if status.State.Running != nil && !status.State.Running.StartedAt.IsZero() {
		value := metav1.NewTime(status.State.Running.StartedAt.Add(-logStartLookback))
		return &value
	}
	if status.State.Terminated != nil && !status.State.Terminated.StartedAt.IsZero() {
		value := metav1.NewTime(status.State.Terminated.StartedAt.Add(-logStartLookback))
		return &value
	}
	return nil
}

func previousLogSinceTime(status corev1.ContainerStatus) *metav1.Time {
	if status.LastTerminationState.Terminated == nil || status.LastTerminationState.Terminated.StartedAt.IsZero() {
		return nil
	}
	value := metav1.NewTime(status.LastTerminationState.Terminated.StartedAt.Add(-logStartLookback))
	return &value
}

func logAttemptStart(status corev1.ContainerStatus, hasStatus, previous bool) *time.Time {
	if !hasStatus {
		return nil
	}
	var started metav1.Time
	if previous {
		if status.LastTerminationState.Terminated == nil {
			return nil
		}
		started = status.LastTerminationState.Terminated.StartedAt
	} else {
		if status.State.Running != nil && !status.State.Running.StartedAt.IsZero() {
			started = status.State.Running.StartedAt
		} else if status.State.Terminated != nil && !status.State.Terminated.StartedAt.IsZero() {
			started = status.State.Terminated.StartedAt
		} else {
			return nil
		}
	}
	if started.IsZero() {
		return nil
	}
	value := started.Time.UTC()
	return &value
}

func noteLogTruncation(bundle *model.Bundle, container string, previous bool, maxBytes int64) {
	stream := "logs"
	if previous {
		stream = "previous logs"
	}
	bundle.Notes = append(bundle.Notes, fmt.Sprintf("%s for container %s reached the %d-byte cap; output was truncated", stream, container, maxBytes))
}

func noteRotatedLogs(bundle *model.Bundle, container string, previous bool, attemptStart *time.Time, lines []model.LogLine) {
	if attemptStart == nil {
		return
	}
	var first *time.Time
	for i := range lines {
		if !lines[i].Timestamp.IsZero() {
			value := lines[i].Timestamp.UTC()
			first = &value
			break
		}
	}
	if first == nil {
		return
	}
	gap := first.Sub(*attemptStart)
	if gap <= 30*time.Second {
		return
	}

	bundle.Notes = append(bundle.Notes, fmt.Sprintf("startup logs appear rotated away (first line at +%ds); run coldstart soon after startup or use a log backend", int64(gap/time.Second)))
	if bundle.Metadata == nil {
		bundle.Metadata = make(map[string]string)
	}
	prefix := fmt.Sprintf("logs.%s.current", container)
	if previous {
		prefix = fmt.Sprintf("logs.%s.previous", container)
	}
	bundle.Metadata[prefix+".attemptStart"] = attemptStart.Format(time.RFC3339Nano)
	bundle.Metadata[prefix+".firstLine"] = first.Format(time.RFC3339Nano)
	bundle.Metadata[prefix+".startupGapSecs"] = strconv.FormatFloat(gap.Seconds(), 'f', -1, 64)
}

func noteMalformedLogLines(bundle *model.Bundle, container string, previous bool, count int) {
	if count == 0 {
		return
	}
	stream := "logs"
	if previous {
		stream = "previous logs"
	}
	bundle.Notes = append(bundle.Notes, fmt.Sprintf("%d line(s) in %s for container %s had no valid RFC3339Nano timestamp prefix; raw text was preserved", count, stream, container))
}

// parseLogStream parses the RFC3339Nano prefix emitted by kubectl logs
// --timestamps. Exactly one separator is removed from valid lines; any further
// whitespace belongs to the original log text. Malformed lines are retained
// verbatim with a zero timestamp rather than being dropped.
func parseLogStream(raw []byte) ([]model.LogLine, int) {
	if len(raw) == 0 {
		return nil, 0
	}

	text := string(raw)
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	captured := make([]model.LogLine, 0, len(lines))
	malformed := 0
	for _, line := range lines {
		separator := strings.IndexAny(line, " \t")
		if separator > 0 {
			if timestamp, err := time.Parse(time.RFC3339Nano, line[:separator]); err == nil {
				captured = append(captured, model.LogLine{
					Timestamp: timestamp,
					Text:      line[separator+1:],
				})
				continue
			}
		}
		malformed++
		captured = append(captured, model.LogLine{Text: line})
	}
	return captured, malformed
}
