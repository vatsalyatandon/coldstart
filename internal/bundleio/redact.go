package bundleio

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/vatsalyatandon/coldstart/internal/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	redactedValue   = "[REDACTED]"
	timestampLayout = time.RFC3339Nano
	logTrimMargin   = 2 * time.Minute
)

var (
	privateKeyPattern           = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	urlPattern                  = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s<>"']+`)
	bearerPattern               = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[a-z0-9._~+/=-]+`)
	jwtPattern                  = regexp.MustCompile(`(?i)\beyJ[a-z0-9_-]{5,}\.[a-z0-9_-]{5,}\.[a-z0-9_-]{5,}\b`)
	hfTokenPattern              = regexp.MustCompile(`(?i)\bhf_[a-z0-9_-]{6,}\b`)
	providerTokenPattern        = regexp.MustCompile(`(?i)\b(?:github_pat_[a-z0-9_]{20,}|gh[pousr]_[a-z0-9]{20,}|glpat-[a-z0-9_-]{16,}|xox[baprs]-[a-z0-9-]{10,}|sk-(?:ant-|proj-)?[a-z0-9_-]{6,}|akia[0-9a-z]{16}|asia[0-9a-z]{16}|aiza[0-9a-z_-]{30,})\b`)
	credentialAssignmentPattern = regexp.MustCompile(`(?i)(\b(?:[a-z0-9]+[_-])*(?:aws[_-]?secret[_-]?access[_-]?key|secret[_-]?access[_-]?key|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret|secret[_-]?key|access[_-]?token|refresh[_-]?token|auth[_-]?token|hf[_-]?token|token|password|passwd|secret|credentials?)\b\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;&]+)`)
	credentialFlagPattern       = regexp.MustCompile(`(?i)(--(?:api[_-]?key|access[_-]?token|auth[_-]?token|token|password|passwd|client[_-]?secret|secret)\s+)([^\s]+)`)
)

// normalizeAndRedact makes a JSON round-trip copy so no caller-owned maps,
// slices, pointers, or Kubernetes objects are mutated.
func normalizeAndRedact(b model.Bundle) (model.Bundle, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return model.Bundle{}, err
	}
	var redacted model.Bundle
	if err := json.Unmarshal(raw, &redacted); err != nil {
		return model.Bundle{}, err
	}

	redacted.Schema = model.BundleSchema
	normalizeTypeMeta(&redacted)
	scrubPod(&redacted.Pod)
	if redacted.Node != nil {
		redactObjectMetadata(&redacted.Node.ObjectMeta)
	}
	for i := range redacted.Events {
		normalizeEventTypeMeta(&redacted.Events[i])
		redactObjectMetadata(&redacted.Events[i].ObjectMeta)
	}
	for i := range redacted.OperatorPods {
		normalizePodTypeMeta(&redacted.OperatorPods[i])
		scrubPod(&redacted.OperatorPods[i])
	}
	redactSensitiveMap(redacted.Metadata)
	trimLogsToStartup(&redacted)

	// Redact every remaining string, including event messages, probe errors,
	// annotations, notes, node data, and any future string-bearing model fields.
	raw, err = json.Marshal(redacted)
	if err != nil {
		return model.Bundle{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return model.Bundle{}, err
	}
	tree = redactJSONStrings(tree)
	raw, err = json.Marshal(tree)
	if err != nil {
		return model.Bundle{}, err
	}
	if err := json.Unmarshal(raw, &redacted); err != nil {
		return model.Bundle{}, err
	}
	return redacted, nil
}

func normalizeTypeMeta(b *model.Bundle) {
	normalizePodTypeMeta(&b.Pod)
	if b.Node != nil {
		if b.Node.APIVersion == "" {
			b.Node.APIVersion = "v1"
		}
		if b.Node.Kind == "" {
			b.Node.Kind = "Node"
		}
	}
	for i := range b.Events {
		normalizeEventTypeMeta(&b.Events[i])
	}
	for i := range b.OperatorPods {
		normalizePodTypeMeta(&b.OperatorPods[i])
	}
}

func normalizePodTypeMeta(pod *corev1.Pod) {
	if pod.APIVersion == "" {
		pod.APIVersion = "v1"
	}
	if pod.Kind == "" {
		pod.Kind = "Pod"
	}
}

func normalizeEventTypeMeta(event *corev1.Event) {
	if event.APIVersion == "" {
		event.APIVersion = "v1"
	}
	if event.Kind == "" {
		event.Kind = "Event"
	}
}

func scrubPod(pod *corev1.Pod) {
	redactObjectMetadata(&pod.ObjectMeta)
	for i := range pod.Spec.InitContainers {
		scrubContainer(&pod.Spec.InitContainers[i])
	}
	for i := range pod.Spec.Containers {
		scrubContainer(&pod.Spec.Containers[i])
	}
	for i := range pod.Spec.EphemeralContainers {
		scrubEphemeralContainer(&pod.Spec.EphemeralContainers[i])
	}
}

func scrubContainer(container *corev1.Container) {
	scrubEnv(container.Env)
	container.EnvFrom = nil
}

func scrubEphemeralContainer(container *corev1.EphemeralContainer) {
	scrubEnv(container.Env)
	container.EnvFrom = nil
}

func scrubEnv(env []corev1.EnvVar) {
	for i := range env {
		env[i].Value = ""
		env[i].ValueFrom = nil
	}
}

func redactObjectMetadata(meta *metav1.ObjectMeta) {
	redactSensitiveMap(meta.Annotations)
	redactSensitiveMap(meta.Labels)
}

func redactSensitiveMap(values map[string]string) {
	for key := range values {
		if sensitiveKey(key) {
			values[key] = redactedValue
		}
	}
}

func sensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	if strings.Contains(lower, "last-applied-configuration") {
		return true
	}
	compact := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, lower)
	for _, marker := range []string{
		"auth", "token", "secret", "password", "passwd", "credential", "apikey",
		"accesskey", "privatekey", "secretkey", "authorization", "authtoken",
		"authurl", "bearerauth", "clientsecret", "signingkey", "encryptionkey", "sshkey",
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return compact == "key"
}

func trimLogsToStartup(b *model.Bundle) {
	start := b.Pod.CreationTimestamp.Time
	end := firstReadyTime(b.Pod)
	if end.IsZero() {
		end = b.CapturedAt
	}
	if end.IsZero() {
		for _, capture := range b.Logs {
			for _, line := range capture.Lines {
				if line.Timestamp.After(end) {
					end = line.Timestamp
				}
			}
		}
	}
	if end.IsZero() {
		end = start
	}
	if !start.IsZero() {
		start = start.Add(-logTrimMargin)
	}
	end = end.Add(logTrimMargin)

	for i := range b.Logs {
		lines := make([]model.LogLine, 0, len(b.Logs[i].Lines))
		for _, line := range b.Logs[i].Lines {
			if !start.IsZero() && line.Timestamp.Before(start) {
				continue
			}
			if line.Timestamp.After(end) {
				continue
			}
			lines = append(lines, line)
		}
		b.Logs[i].Lines = lines
	}
}

func firstReadyTime(pod corev1.Pod) time.Time {
	var ready time.Time
	for _, condition := range pod.Status.Conditions {
		if condition.Type != corev1.PodReady || condition.Status != corev1.ConditionTrue || condition.LastTransitionTime.IsZero() {
			continue
		}
		if ready.IsZero() || condition.LastTransitionTime.Time.Before(ready) {
			ready = condition.LastTransitionTime.Time
		}
	}
	return ready
}

func redactJSONStrings(value any) any {
	switch value := value.(type) {
	case string:
		return redactText(value)
	case []any:
		for i := range value {
			value[i] = redactJSONStrings(value[i])
		}
		return value
	case map[string]any:
		redacted := make(map[string]any, len(value))
		for key, item := range value {
			redacted[redactText(key)] = redactJSONStrings(item)
		}
		return redacted
	default:
		return value
	}
}

func redactText(text string) string {
	text = privateKeyPattern.ReplaceAllString(text, redactedValue)
	text = urlPattern.ReplaceAllStringFunc(text, redactURL)
	text = bearerPattern.ReplaceAllString(text, redactedValue)
	text = jwtPattern.ReplaceAllString(text, redactedValue)
	text = providerTokenPattern.ReplaceAllString(text, redactedValue)
	text = hfTokenPattern.ReplaceAllString(text, redactedValue)
	text = credentialAssignmentPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := credentialAssignmentPattern.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		return parts[1] + redactedValue
	})
	text = credentialFlagPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := credentialFlagPattern.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		return parts[1] + redactedValue
	})
	return text
}

func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		// A malformed URL can still contain credentials. Dropping it is safer
		// than attempting a best-effort rewrite that might leave a secret behind.
		return redactedValue
	}
	parsed.User = nil
	if parsed.RawQuery != "" {
		parts := strings.Split(parsed.RawQuery, "&")
		for i, part := range parts {
			rawKey, _, _ := strings.Cut(part, "=")
			key, err := url.QueryUnescape(rawKey)
			if err != nil {
				parts[i] = redactedValue
				continue
			}
			if sensitiveQueryKey(key) {
				parts[i] = rawKey + "=" + redactedValue
			}
		}
		parsed.RawQuery = strings.Join(parts, "&")
	}
	return parsed.String()
}

func sensitiveQueryKey(key string) bool {
	compact := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, strings.ToLower(key))
	if compact == "key" || compact == "sig" || compact == "auth" {
		return true
	}
	for _, marker := range []string{
		"token", "secret", "password", "passwd", "credential", "apikey",
		"accesskey", "privatekey", "signature", "authorization",
	} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}
