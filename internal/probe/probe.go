// Package probe performs an explicitly requested, read-only first-token probe
// through a Kubernetes SPDY port-forward.
package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/vatsalyatandon/coldstart/internal/collect"
	"github.com/vatsalyatandon/coldstart/internal/model"
)

const defaultTimeout = 60 * time.Second

// Options controls the optional probe. Pod is a bare pod name (not pod/<name>).
// The CLI should call Run only when --probe is set, then assign the returned
// result to bundle.Probe.
type Options struct {
	Kubeconfig string
	Namespace  string
	Pod        string
	Container  string
	Prompt     string
}

// Run sends one streaming OpenAI-compatible completion request through an SPDY
// port-forward. It never patches, recreates, or otherwise mutates the pod. A
// partial model.ProbeResult is returned on error so callers can attach it to a
// bundle even when the endpoint cannot complete the request.
func Run(ctx context.Context, opts Options) (model.ProbeResult, error) {
	result := model.ProbeResult{}
	fail := func(err error) (model.ProbeResult, error) {
		result.Error = err.Error()
		return result, err
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	podName := strings.TrimSpace(opts.Pod)
	if podName == "" || strings.ContainsAny(podName, "/ \t\r\n") {
		return fail(fmt.Errorf("invalid pod name %q", opts.Pod))
	}
	client, config, namespace, err := loadClient(opts)
	if err != nil {
		return fail(fmt.Errorf("load Kubernetes client: %w", err))
	}
	pod, err := collect.ResolvePod(ctx, client, namespace, collect.ResourceRef{Kind: "pod", Name: podName})
	if err != nil {
		return fail(err)
	}
	container, err := selectContainer(pod, opts.Container)
	if err != nil {
		return fail(err)
	}

	result.Model = extractModel(*container)
	if result.Model == "" {
		return fail(fmt.Errorf("could not determine model for container %s from --model, --model-path, or environment", container.Name))
	}

	remotePort := detectPort(*container)
	localPort, stopForward, err := startPortForward(ctx, config, pod.Namespace, pod.Name, remotePort, defaultTimeout)
	if err != nil {
		return fail(err)
	}
	defer stopForward()

	result.Endpoint = fmt.Sprintf("http://127.0.0.1:%d/v1/completions", localPort)
	prompt := opts.Prompt
	if prompt == "" {
		prompt = "Hello"
	}
	requestBody := struct {
		Model     string `json:"model"`
		Prompt    string `json:"prompt"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
	}{
		Model:     result.Model,
		Prompt:    prompt,
		MaxTokens: 1,
		Stream:    true,
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return fail(fmt.Errorf("encode probe request: %w", err))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, result.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fail(fmt.Errorf("create probe request: %w", err))
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")

	httpClient := &http.Client{
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		Timeout:   defaultTimeout,
	}
	result.RequestSentAt = time.Now().UTC()
	response, err := httpClient.Do(request)
	if err != nil {
		return fail(fmt.Errorf("send probe request: %w", err))
	}
	defer response.Body.Close()
	result.StatusCode = response.StatusCode

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		message := strings.TrimSpace(string(detail))
		if message == "" {
			return fail(fmt.Errorf("probe endpoint returned HTTP %d", response.StatusCode))
		}
		return fail(fmt.Errorf("probe endpoint returned HTTP %d: %s", response.StatusCode, message))
	}

	token, err := firstTokenFromSSE(response.Body)
	if err != nil {
		return fail(err)
	}
	result.FirstTokenAt = time.Now().UTC()
	result.FirstToken = token
	return result, nil
}

func loadClient(opts Options) (kubernetes.Interface, *rest.Config, string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.Kubeconfig != "" {
		loadingRules.ExplicitPath = opts.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if opts.Namespace != "" {
		overrides.Context = clientcmdapi.Context{Namespace: opts.Namespace}
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	config, err := loader.ClientConfig()
	if err != nil {
		return nil, nil, "", err
	}
	namespace, _, err := loader.Namespace()
	if err != nil {
		return nil, nil, "", err
	}
	if opts.Namespace != "" {
		namespace = opts.Namespace
	}
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, "", err
	}
	return client, config, namespace, nil
}

func selectContainer(pod *corev1.Pod, requested string) (*corev1.Container, error) {
	if requested != "" {
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == requested {
				return &pod.Spec.Containers[i], nil
			}
		}
		return nil, fmt.Errorf("container %s not found in pod %s", requested, pod.Name)
	}
	if len(pod.Spec.Containers) == 0 {
		return nil, fmt.Errorf("pod %s has no containers", pod.Name)
	}

	best := -1
	bestScore := 0
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		score := 0
		if hasHTTPOr8000Port(*container) {
			score += 2
		}
		if isEngineContainer(*container) {
			score += 4
		}
		if extractModel(*container) != "" {
			score++
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best >= 0 && bestScore > 0 {
		return &pod.Spec.Containers[best], nil
	}
	if len(pod.Spec.Containers) == 1 {
		return &pod.Spec.Containers[0], nil
	}
	return nil, fmt.Errorf("cannot select a probe container in pod %s; specify --container", pod.Name)
}

func isEngineContainer(container corev1.Container) bool {
	parts := append([]string{container.Name}, container.Command...)
	parts = append(parts, container.Args...)
	identity := strings.ToLower(strings.Join(parts, " "))
	return strings.Contains(identity, "vllm") || strings.Contains(identity, "sglang")
}

func hasHTTPOr8000Port(container corev1.Container) bool {
	for _, port := range container.Ports {
		if strings.EqualFold(port.Name, "http") || port.ContainerPort == 8000 {
			return true
		}
	}
	return false
}

// detectPort prefers a named http port, then an explicitly declared port 8000.
// vLLM and SGLang both default to 8000, so an undeclared port also falls back to
// 8000 rather than making a valid server unprobeable.
func detectPort(container corev1.Container) int {
	for _, port := range container.Ports {
		if strings.EqualFold(port.Name, "http") && port.ContainerPort > 0 {
			return int(port.ContainerPort)
		}
	}
	for _, port := range container.Ports {
		if port.ContainerPort == 8000 {
			return 8000
		}
	}
	return 8000
}

func extractModel(container corev1.Container) string {
	arguments := append(append([]string{}, container.Command...), container.Args...)
	var modelFlag, modelPathFlag string
	for i := 0; i < len(arguments); i++ {
		argument := arguments[i]
		switch {
		case argument == "--model" || argument == "--model-path":
			if i+1 < len(arguments) && !strings.HasPrefix(arguments[i+1], "-") {
				value := arguments[i+1]
				if argument == "--model" {
					modelFlag = value
				} else {
					modelPathFlag = value
				}
				i++
			}
		case strings.HasPrefix(argument, "--model="):
			modelFlag = strings.TrimPrefix(argument, "--model=")
		case strings.HasPrefix(argument, "--model-path="):
			modelPathFlag = strings.TrimPrefix(argument, "--model-path=")
		}
	}
	if modelFlag != "" {
		return strings.TrimSpace(modelFlag)
	}
	if modelPathFlag != "" {
		return strings.TrimSpace(modelPathFlag)
	}

	preferred := []string{"MODEL_NAME", "MODEL", "MODEL_ID", "MODEL_NAME_OR_PATH", "MODEL_PATH", "HUGGINGFACE_MODEL", "HF_MODEL"}
	for _, candidate := range preferred {
		for _, env := range container.Env {
			if strings.EqualFold(env.Name, candidate) && strings.TrimSpace(env.Value) != "" {
				return strings.TrimSpace(env.Value)
			}
		}
	}
	for _, env := range container.Env {
		name := strings.ToUpper(env.Name)
		if strings.TrimSpace(env.Value) != "" && (strings.HasSuffix(name, "_MODEL") || strings.HasSuffix(name, "_MODEL_PATH")) {
			return strings.TrimSpace(env.Value)
		}
	}
	return ""
}

func startPortForward(ctx context.Context, config *rest.Config, namespace, podName string, remotePort int, timeout time.Duration) (int, func(), error) {
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return 0, nil, fmt.Errorf("create port-forward client: %w", err)
	}
	request := client.CoreV1().RESTClient().Post().
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("portforward")

	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return 0, nil, fmt.Errorf("configure SPDY port-forward: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport, Timeout: timeout}, http.MethodPost, request.URL())

	stop := make(chan struct{})
	ready := make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(
		dialer,
		[]string{"127.0.0.1"},
		[]string{fmt.Sprintf("0:%d", remotePort)},
		stop,
		ready,
		io.Discard,
		io.Discard,
	)
	if err != nil {
		return 0, nil, fmt.Errorf("create port-forward: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- forwarder.ForwardPorts()
	}()
	var stopOnce sync.Once
	stopForward := func() {
		stopOnce.Do(func() { close(stop) })
	}

	select {
	case <-ready:
	case err := <-done:
		stopForward()
		if err == nil {
			err = fmt.Errorf("port-forward stopped before becoming ready")
		}
		return 0, nil, fmt.Errorf("start port-forward to %s/%s:%d: %w", namespace, podName, remotePort, err)
	case <-ctx.Done():
		stopForward()
		return 0, nil, fmt.Errorf("start port-forward to %s/%s:%d: %w", namespace, podName, remotePort, ctx.Err())
	}

	ports, err := forwarder.GetPorts()
	if err != nil {
		stopForward()
		return 0, nil, fmt.Errorf("read forwarded port: %w", err)
	}
	if len(ports) != 1 || ports[0].Remote != uint16(remotePort) {
		stopForward()
		return 0, nil, fmt.Errorf("port-forward returned unexpected ports: %v", ports)
	}
	return int(ports[0].Local), stopForward, nil
}

type completionChunk struct {
	Choices []struct {
		Text  string `json:"text"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// firstTokenFromSSE returns the first non-empty text/delta.content value from an
// OpenAI-compatible event stream. It stops without consuming the rest of the
// response, which keeps RequestSentAt-to-FirstTokenAt focused on server latency.
func firstTokenFromSSE(reader io.Reader) (string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var dataLines []string

	dispatch := func() (string, bool, error) {
		if len(dataLines) == 0 {
			return "", false, nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		trimmed := strings.TrimSpace(data)
		if trimmed == "" {
			return "", false, nil
		}
		if trimmed == "[DONE]" {
			return "", false, fmt.Errorf("stream ended before returning a first token")
		}

		var chunk completionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return "", false, fmt.Errorf("parse streamed completion: %w", err)
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return "", false, fmt.Errorf("streamed completion error: %s", chunk.Error.Message)
		}
		for _, choice := range chunk.Choices {
			if choice.Text != "" {
				return choice.Text, true, nil
			}
			if choice.Delta.Content != "" {
				return choice.Delta.Content, true, nil
			}
		}
		return "", false, nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			token, found, err := dispatch()
			if found || err != nil {
				return token, err
			}
			continue
		}
		field, value, hasColon := strings.Cut(line, ":")
		if !hasColon {
			value = ""
		}
		if field == "data" {
			value = strings.TrimPrefix(value, " ")
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read streamed completion: %w", err)
	}
	token, found, err := dispatch()
	if found || err != nil {
		return token, err
	}
	return "", fmt.Errorf("stream ended before returning a first token")
}
