package probe

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestExtractModel(t *testing.T) {
	tests := []struct {
		name      string
		container corev1.Container
		want      string
	}{
		{
			name: "model flag wins over model path",
			container: corev1.Container{
				Args: []string{"--model-path=/models/qwen", "--model", "qwen3-35b"},
			},
			want: "qwen3-35b",
		},
		{
			name: "equals flags in command",
			container: corev1.Container{
				Command: []string{"vllm", "serve", "--model=org/model"},
				Args:    []string{"--port=8000"},
			},
			want: "org/model",
		},
		{
			name: "model path fallback",
			container: corev1.Container{
				Args: []string{"--model-path", "/models/sglang"},
			},
			want: "/models/sglang",
		},
		{
			name: "environment",
			container: corev1.Container{
				Env: []corev1.EnvVar{{Name: "MODEL_PATH", Value: "/models/env-model"}},
			},
			want: "/models/env-model",
		},
		{
			name: "engine prefixed environment",
			container: corev1.Container{
				Env: []corev1.EnvVar{{Name: "VLLM_MODEL", Value: "prefixed-model"}},
			},
			want: "prefixed-model",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractModel(tt.container); got != tt.want {
				t.Fatalf("extractModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectPort(t *testing.T) {
	tests := []struct {
		name  string
		ports []corev1.ContainerPort
		want  int
	}{
		{name: "named http", ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}, want: 8080},
		{name: "numeric fallback", ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9090}, {ContainerPort: 8000}}, want: 8000},
		{name: "http preferred over 8000", ports: []corev1.ContainerPort{{ContainerPort: 8000}, {Name: "http", ContainerPort: 8081}}, want: 8081},
		{name: "engine default", want: 8000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			container := corev1.Container{Ports: tt.ports}
			if got := detectPort(container); got != tt.want {
				t.Fatalf("detectPort() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSelectContainer(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "sidecar", Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 9090}}},
			{Name: "vllm", Args: []string{"--model=qwen"}, Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8000}}},
		}},
	}
	container, err := selectContainer(pod, "")
	if err != nil {
		t.Fatalf("selectContainer() error = %v", err)
	}
	if container.Name != "vllm" {
		t.Fatalf("selectContainer() = %q, want vllm", container.Name)
	}
	container, err = selectContainer(pod, "sidecar")
	if err != nil || container.Name != "sidecar" {
		t.Fatalf("selectContainer(explicit) = %#v, %v", container, err)
	}
}

func TestFirstTokenFromSSE(t *testing.T) {
	tests := []struct {
		name    string
		stream  string
		want    string
		wantErr bool
	}{
		{
			name: "completion text after role chunk",
			stream: ": keepalive\n\n" +
				"data: {\"choices\":[{\"text\":\"\"}]}\n\n" +
				"data:{\"choices\":[{\"text\":\"Hello\"}]}\n\n" +
				"data: [DONE]\n\n",
			want: "Hello",
		},
		{
			name:   "delta content at EOF",
			stream: "event: message\ndata: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}",
			want:   "world",
		},
		{
			name:   "multiline data",
			stream: "data: {\"choices\":[\ndata: {\"text\":\"multi\"}]}\n\n",
			want:   "multi",
		},
		{
			name:    "done without token",
			stream:  "data: [DONE]\n\n",
			wantErr: true,
		},
		{
			name:    "malformed json",
			stream:  "data: not-json\n\n",
			wantErr: true,
		},
		{
			name:    "stream error",
			stream:  "data: {\"error\":{\"message\":\"model not found\"}}\n\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := firstTokenFromSSE(strings.NewReader(tt.stream))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("firstTokenFromSSE() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("firstTokenFromSSE() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("firstTokenFromSSE() = %q, want %q", got, tt.want)
			}
		})
	}
}
