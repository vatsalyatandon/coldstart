package collect

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

func collectLogs(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod, opts Options, bundle *model.Bundle) {
	statuses := make(map[string]corev1.ContainerStatus, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		statuses[status.Name] = status
	}
	for _, status := range pod.Status.InitContainerStatuses {
		statuses[status.Name] = status
	}

	containers := append([]corev1.Container{}, pod.Spec.Containers...)
	containers = append(containers, pod.Spec.InitContainers...)
	foundContainer := false
	for _, container := range containers {
		if opts.Container != "" && container.Name != opts.Container {
			continue
		}
		foundContainer = true

		raw, err := client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container:  container.Name,
			Timestamps: true,
		}).Do(ctx).Raw()
		if err != nil {
			bundle.Notes = append(bundle.Notes, fmt.Sprintf("logs for container %s unavailable: %v", container.Name, err))
		} else {
			lines, malformed := parseLogStream(raw)
			bundle.Logs = append(bundle.Logs, model.LogCapture{Container: container.Name, Lines: lines})
			noteMalformedLogLines(bundle, container.Name, false, malformed)
		}

		status, hasStatus := statuses[container.Name]
		if !opts.IncludePrevious || !hasStatus || status.RestartCount == 0 {
			continue
		}
		previous, err := client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container:  container.Name,
			Timestamps: true,
			Previous:   true,
		}).Do(ctx).Raw()
		if err != nil {
			bundle.Notes = append(bundle.Notes, fmt.Sprintf("previous logs for container %s unavailable: %v", container.Name, err))
			continue
		}
		lines, malformed := parseLogStream(previous)
		bundle.Logs = append(bundle.Logs, model.LogCapture{Container: container.Name, Previous: true, Lines: lines})
		noteMalformedLogLines(bundle, container.Name, true, malformed)
	}

	if opts.Container != "" && !foundContainer {
		bundle.Notes = append(bundle.Notes, fmt.Sprintf("container %s not found in pod %s", opts.Container, pod.Name))
	}
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
