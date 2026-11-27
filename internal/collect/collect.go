// Package collect builds a read-only coldstart bundle from Kubernetes.
package collect

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

// DefaultMaxLogBytes is the per-stream log cap used when Options.MaxLogBytes is
// zero or negative.
const DefaultMaxLogBytes int64 = 32 << 20

// Options controls which pod and logs are collected. Resource must be either
// pod/<name> or deploy/<name>. An empty Namespace uses the kubeconfig context's
// namespace, falling back to default.
type Options struct {
	Kubeconfig      string
	Namespace       string
	Resource        string
	Container       string
	IncludePrevious bool
	// MaxLogBytes limits each current or previous log stream. Values <= 0 use
	// DefaultMaxLogBytes.
	MaxLogBytes int64
}

// ResourceRef is a parsed pod or deployment reference.
type ResourceRef struct {
	Kind string
	Name string
}

// ParseResource parses pod/<name> and deploy/<name> references. The common
// plural aliases pods, deployments, and deploy are also accepted.
func ParseResource(resource string) (ResourceRef, error) {
	parts := strings.Split(strings.TrimSpace(resource), "/")
	if len(parts) != 2 {
		return ResourceRef{}, fmt.Errorf("invalid resource %q: expected pod/<name> or deploy/<name>", resource)
	}

	var kind string
	switch strings.ToLower(strings.TrimSpace(parts[0])) {
	case "pod", "pods":
		kind = "pod"
	case "deploy", "deployment", "deployments":
		kind = "deployment"
	default:
		return ResourceRef{}, fmt.Errorf("invalid resource %q: expected pod/<name> or deploy/<name>", resource)
	}

	name := strings.TrimSpace(parts[1])
	if name == "" || strings.ContainsAny(name, " \t\r\n") {
		return ResourceRef{}, fmt.Errorf("invalid resource %q: resource name is empty or malformed", resource)
	}
	return ResourceRef{Kind: kind, Name: name}, nil
}

// Collect reads a pod and its best-effort supporting data. Pod resolution is
// required; failures to read node state, events, operator pods, or logs are
// recorded in Bundle.Notes and do not discard the rest of the bundle.
func Collect(ctx context.Context, opts Options) (model.Bundle, error) {
	ref, err := ParseResource(opts.Resource)
	if err != nil {
		return model.Bundle{}, err
	}

	client, namespace, err := loadClient(opts)
	if err != nil {
		return model.Bundle{}, fmt.Errorf("load Kubernetes client: %w", err)
	}

	pod, err := ResolvePod(ctx, client, namespace, ref)
	if err != nil {
		return model.Bundle{}, err
	}

	bundle := model.Bundle{
		Schema:     model.BundleSchema,
		Source:     strings.TrimSpace(opts.Resource),
		CapturedAt: time.Now().UTC(),
		Pod:        *pod,
		Metadata: map[string]string{
			"namespace": namespace,
			"resource":  ref.Kind + "/" + pod.Name,
		},
	}

	collectNodeAndOperators(ctx, client, pod, &bundle)
	collectEvents(ctx, client, pod, &bundle)
	collectLogs(ctx, client, pod, opts, &bundle)
	return bundle, nil
}

// ResolvePod returns a pod directly, or the newest pod matching a deployment's
// selector. It only performs get/list operations.
func ResolvePod(ctx context.Context, client kubernetes.Interface, namespace string, ref ResourceRef) (*corev1.Pod, error) {
	switch ref.Kind {
	case "pod":
		pod, err := client.CoreV1().Pods(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get pod %s/%s: %w", namespace, ref.Name, err)
		}
		return pod, nil
	case "deployment":
		deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get deployment %s/%s: %w", namespace, ref.Name, err)
		}
		if deployment.Spec.Selector == nil || len(deployment.Spec.Selector.MatchLabels)+len(deployment.Spec.Selector.MatchExpressions) == 0 {
			return nil, fmt.Errorf("deployment %s/%s has an empty selector", namespace, ref.Name)
		}

		pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: metav1.FormatLabelSelector(deployment.Spec.Selector),
		})
		if err != nil {
			return nil, fmt.Errorf("list pods for deployment %s/%s: %w", namespace, ref.Name, err)
		}
		if len(pods.Items) == 0 {
			return nil, fmt.Errorf("deployment %s/%s has no matching pods", namespace, ref.Name)
		}
		return newestPod(pods.Items), nil
	default:
		return nil, fmt.Errorf("unsupported resource kind %q", ref.Kind)
	}
}

func newestPod(pods []corev1.Pod) *corev1.Pod {
	var newest *corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if newest == nil ||
			pod.CreationTimestamp.After(newest.CreationTimestamp.Time) ||
			(pod.CreationTimestamp.Equal(&newest.CreationTimestamp) && pod.Name > newest.Name) {
			newest = pod
		}
	}
	return newest
}

func loadClient(opts Options) (kubernetes.Interface, string, error) {
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
		return nil, "", err
	}
	namespace, _, err := loader.Namespace()
	if err != nil {
		return nil, "", err
	}
	if opts.Namespace != "" {
		namespace = opts.Namespace
	}
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, "", err
	}
	return client, namespace, nil
}

func collectNodeAndOperators(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod, bundle *model.Bundle) {
	if pod.Spec.NodeName == "" {
		bundle.Notes = append(bundle.Notes, "pod is not assigned to a node; node and operator pod collection skipped")
		return
	}

	node, err := client.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		bundle.Notes = append(bundle.Notes, fmt.Sprintf("node %s unavailable: %v", pod.Spec.NodeName, err))
		return
	}
	bundle.Node = node

	operatorPods, err := listOperatorPods(ctx, client, pod.Spec.NodeName)
	if err != nil {
		bundle.Notes = append(bundle.Notes, fmt.Sprintf("operator pods on node %s unavailable: %v", pod.Spec.NodeName, err))
		return
	}
	bundle.OperatorPods = operatorPods
}

func collectEvents(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod, bundle *model.Bundle) {
	selector := fields.Set{
		"involvedObject.name":      pod.Name,
		"involvedObject.namespace": pod.Namespace,
	}.AsSelector().String()
	events, err := client.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		bundle.Notes = append(bundle.Notes, fmt.Sprintf("pod events unavailable: %v", err))
		return
	}

	for i := range events.Items {
		event := &events.Items[i]
		object := event.InvolvedObject
		if object.Name != pod.Name || (object.Kind != "" && object.Kind != "Pod") {
			continue
		}
		if object.Namespace != "" && object.Namespace != pod.Namespace {
			continue
		}
		if object.UID != "" && pod.UID != "" && object.UID != pod.UID {
			continue
		}
		bundle.Events = append(bundle.Events, *event)
	}

	if len(bundle.Events) == 0 {
		bundle.Notes = append(bundle.Notes, "no pod events found; Kubernetes event retention may have expired")
		return
	}
	sort.SliceStable(bundle.Events, func(i, j int) bool {
		left, right := bundle.Events[i], bundle.Events[j]
		lt, rt := eventTime(left), eventTime(right)
		if !lt.Equal(rt) {
			return lt.Before(rt)
		}
		if left.Reason != right.Reason {
			return left.Reason < right.Reason
		}
		return left.Name < right.Name
	})
}

func eventTime(event corev1.Event) time.Time {
	if !event.FirstTimestamp.IsZero() {
		return event.FirstTimestamp.Time
	}
	if event.EventTime.IsZero() {
		return event.CreationTimestamp.Time
	}
	return event.EventTime.Time
}

func listOperatorPods(ctx context.Context, client kubernetes.Interface, nodeName string) ([]corev1.Pod, error) {
	selector := fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
	pods, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		return nil, err
	}

	operators := make([]corev1.Pod, 0)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil && isOperatorPod(pod) {
			operators = append(operators, *pod)
		}
	}
	sort.Slice(operators, func(i, j int) bool {
		if operators[i].Namespace != operators[j].Namespace {
			return operators[i].Namespace < operators[j].Namespace
		}
		return operators[i].Name < operators[j].Name
	})
	return operators, nil
}

func isOperatorPod(pod *corev1.Pod) bool {
	var values []string
	values = append(values, pod.Namespace, pod.Name)
	for key, value := range pod.Labels {
		values = append(values, key, value)
	}
	for key, value := range pod.Annotations {
		values = append(values, key, value)
	}
	identity := strings.ToLower(strings.Join(values, " "))

	for _, marker := range []string{
		"device-plugin",
		"device_plugin",
		"gpu-operator",
		"gpu-feature-discovery",
		"dcgm",
		"gpu-manager",
		"nvidia-driver-daemonset",
		"nvidia-container-toolkit-daemonset",
		"nvidia-cuda-validator",
		"nvidia-mig-manager",
		"node-feature-discovery",
		"nfd-master",
		"nfd-worker",
	} {
		if strings.Contains(identity, marker) {
			return true
		}
	}
	return false
}
