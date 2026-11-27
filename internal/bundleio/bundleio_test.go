package bundleio

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vatsalyatandon/coldstart/internal/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWriteReadRoundTrip(t *testing.T) {
	bundle := sampleBundle()
	var archive bytes.Buffer
	if err := Write(&archive, bundle); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	members := readArchiveMembers(t, archive.Bytes())
	wantNames := []string{
		"bundle.json",
		"pod.yaml",
		"node.yaml",
		"events.yaml",
		"logs/engine.log",
		"logs/engine-previous.log",
	}
	if len(members) != len(wantNames) {
		t.Fatalf("archive has %d members, want %d: %v", len(members), len(wantNames), memberNames(members))
	}
	for _, name := range wantNames {
		if _, ok := members[name]; !ok {
			t.Errorf("archive is missing %q; got %v", name, memberNames(members))
		}
	}
	if !bytes.Contains(members["pod.yaml"], []byte("apiVersion: v1")) || !bytes.Contains(members["pod.yaml"], []byte("kind: Pod")) {
		t.Errorf("pod.yaml is not a normalized Kubernetes manifest:\n%s", members["pod.yaml"])
	}
	if !bytes.Contains(members["node.yaml"], []byte("kind: Node")) {
		t.Errorf("node.yaml is not a normalized Kubernetes manifest:\n%s", members["node.yaml"])
	}
	wantCurrentLog := bundle.Logs[0].Lines[0].Timestamp.Format(timestampLayout) + "\tengine boot\n"
	if got := string(members["logs/engine.log"]); got != wantCurrentLog {
		t.Errorf("current log = %q, want %q", got, wantCurrentLog)
	}
	wantPreviousLog := bundle.Logs[1].Lines[0].Timestamp.Format(timestampLayout) + "\tprevious attempt\n"
	if got := string(members["logs/engine-previous.log"]); got != wantPreviousLog {
		t.Errorf("previous log = %q, want %q", got, wantPreviousLog)
	}

	got, err := Read(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Schema != model.BundleSchema {
		t.Errorf("schema = %q, want %q", got.Schema, model.BundleSchema)
	}
	if got.Pod.Name != bundle.Pod.Name || got.Pod.Namespace != bundle.Pod.Namespace {
		t.Errorf("pod identity = %s/%s, want %s/%s", got.Pod.Namespace, got.Pod.Name, bundle.Pod.Namespace, bundle.Pod.Name)
	}
	if got.Pod.APIVersion != "v1" || got.Pod.Kind != "Pod" {
		t.Errorf("pod type metadata = %s %s, want v1 Pod", got.Pod.APIVersion, got.Pod.Kind)
	}
	if got.Node == nil || got.Node.Name != bundle.Node.Name {
		t.Errorf("node = %#v, want node %q", got.Node, bundle.Node.Name)
	}
	if len(got.Events) != 1 || got.Events[0].Reason != "Started" {
		t.Errorf("events = %#v, want one Started event", got.Events)
	}
	if len(got.Logs) != 2 || len(got.Logs[0].Lines) != 1 || got.Logs[0].Lines[0].Text != "engine boot" {
		t.Errorf("logs did not round trip: %#v", got.Logs)
	}
	if got.Metadata["cluster"] != "test-cluster" {
		t.Errorf("metadata = %#v, want cluster metadata", got.Metadata)
	}
}

func TestWriteMinimalBundle(t *testing.T) {
	var archive bytes.Buffer
	if err := Write(&archive, model.Bundle{}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := Read(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Schema != model.BundleSchema {
		t.Errorf("schema = %q, want normalized %q", got.Schema, model.BundleSchema)
	}
	members := readArchiveMembers(t, archive.Bytes())
	if _, ok := members["node.yaml"]; ok {
		t.Error("archive contains node.yaml for an absent node")
	}
	if strings.TrimSpace(string(members["events.yaml"])) != "[]" {
		t.Errorf("events.yaml = %q, want an empty YAML list", members["events.yaml"])
	}
}

func TestWriteRedactsWithoutMutatingCaller(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	secretRef := corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "env-ref-secret"},
		Key:                  "token",
	}
	bundle := model.Bundle{
		Schema:     model.BundleSchema,
		CapturedAt: t0.Add(30 * time.Minute),
		Pod: corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "secretive",
				Namespace:         "inference",
				CreationTimestamp: metav1.NewTime(t0),
				Annotations: map[string]string{
					"example.com/api-key":                              "annotation-secret",
					"kubectl.kubernetes.io/last-applied-configuration": `{"env":[{"value":"last-applied-secret"}]}`,
					"example.com/safe":                                 "keep-annotation",
				},
				Labels: map[string]string{"example.com/auth-token": "label-secret"},
			},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{
					Name: "init",
					Env:  []corev1.EnvVar{{Name: "INIT_TOKEN", Value: "init-env-secret"}},
				}},
				Containers: []corev1.Container{{
					Name: "engine",
					Env: []corev1.EnvVar{
						{Name: "PLAIN", Value: "env-plain-secret"},
						{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &secretRef}},
						{Name: "SAFE", Value: "all-env-values-must-be-cleared"},
					},
					EnvFrom: []corev1.EnvFromSource{
						{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "envfrom-secret"}}},
						{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "envfrom-config"}}},
					},
				}},
				EphemeralContainers: []corev1.EphemeralContainer{{
					EphemeralContainerCommon: corev1.EphemeralContainerCommon{
						Name: "debug",
						Env:  []corev1.EnvVar{{Name: "DEBUG_PASSWORD", Value: "ephemeral-env-secret"}},
					},
				}},
			},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type:               corev1.PodReady,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(t0.Add(5 * time.Minute)),
			}}},
		},
		Node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:        "node-1",
			Annotations: map[string]string{"example.com/credentials": "node-secret"},
		}},
		Events: []corev1.Event{{
			Reason: "Pulling",
			Message: "Authorization: Bearer bearer-secret; HF_TOKEN=hf_abcdefghijklmnopqrstuvwxyz123456; " +
				"https://alice:url-password@example.com/model?download=true&api_key=query-secret&access_token=query-token",
		}},
		Logs: []model.LogCapture{{
			Container: "engine",
			Lines: []model.LogLine{{
				Timestamp: t0.Add(time.Minute),
				Text: "token=log-secret ghp_abcdefghijklmnopqrstuvwxyz123456 sk-abcdefghijklmnopqrstuvwxyz " +
					"https://user:log-pass@example.com/path?client_secret=log-query-secret",
			}},
		}},
		OperatorPods: []corev1.Pod{{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "operator",
				Env:  []corev1.EnvVar{{Name: "OPERATOR_TOKEN", Value: "operator-env-secret"}},
			}}},
		}},
		Probe: &model.ProbeResult{
			Endpoint:      "https://probe-user:probe-pass@example.com/v1?token=probe-query-secret",
			RequestSentAt: t0.Add(5 * time.Minute),
			FirstTokenAt:  t0.Add(5*time.Minute + time.Second),
			Error: "password=probe-error-secret glpat-abcdefghijklmnopqrstuvwxyz123456 " +
				"-----BEGIN PRIVATE KEY-----\nprivate-key-secret\n-----END PRIVATE KEY-----",
		},
		Notes: []string{
			"ordinary diagnostic",
			"github_pat_abcdefghijklmnopqrstuvwxyz1234567890",
			"AKIAABCDEFGHIJKLMNOP",
			"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJzZWNyZXQifQ.signaturevalue",
			"--password flag-secret",
			"AWS_SESSION_TOKEN=session-token-secret OPENAI_API_KEY=openai-api-secret",
		},
		Metadata: map[string]string{
			"credential": "metadata-secret",
			"safe":       "keep-metadata",
		},
	}
	before, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := Write(&archive, bundle); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	after, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Write() mutated the caller's bundle")
	}

	got, err := Read(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	containers := [][]corev1.Container{got.Pod.Spec.Containers, got.Pod.Spec.InitContainers, got.OperatorPods[0].Spec.Containers}
	for _, group := range containers {
		for _, container := range group {
			for _, env := range container.Env {
				if env.Value != "" || env.ValueFrom != nil {
					t.Errorf("container %q env %q was not cleared: %#v", container.Name, env.Name, env)
				}
			}
			if len(container.EnvFrom) != 0 {
				t.Errorf("container %q EnvFrom was not cleared: %#v", container.Name, container.EnvFrom)
			}
		}
	}
	if env := got.Pod.Spec.EphemeralContainers[0].Env; len(env) != 1 || env[0].Value != "" || env[0].ValueFrom != nil {
		t.Errorf("ephemeral container env was not cleared: %#v", env)
	}
	if got.Pod.Annotations["example.com/api-key"] != redactedValue {
		t.Errorf("sensitive annotation = %q, want %q", got.Pod.Annotations["example.com/api-key"], redactedValue)
	}
	if got.Pod.Annotations["kubectl.kubernetes.io/last-applied-configuration"] != redactedValue {
		t.Errorf("last-applied annotation was not redacted")
	}
	if got.Metadata["credential"] != redactedValue || got.Metadata["safe"] != "keep-metadata" {
		t.Errorf("metadata = %#v", got.Metadata)
	}
	if got.Pod.Annotations["example.com/safe"] != "keep-annotation" {
		t.Errorf("safe annotation was changed: %q", got.Pod.Annotations["example.com/safe"])
	}
	if !strings.Contains(got.Events[0].Message, "https://example.com/model?download=true") {
		t.Errorf("safe URL parts were not retained: %q", got.Events[0].Message)
	}
	if !strings.Contains(got.Logs[0].Lines[0].Text, redactedValue) {
		t.Errorf("log secret was not redacted: %q", got.Logs[0].Lines[0].Text)
	}

	secrets := []string{
		"env-plain-secret", "env-ref-secret", "envfrom-secret", "init-env-secret",
		"ephemeral-env-secret", "operator-env-secret", "annotation-secret",
		"last-applied-secret", "label-secret", "node-secret", "bearer-secret",
		"hf_abcdefghijklmnopqrstuvwxyz123456", "alice:url-password", "query-secret",
		"query-token", "log-secret", "ghp_abcdefghijklmnopqrstuvwxyz123456",
		"sk-abcdefghijklmnopqrstuvwxyz", "user:log-pass", "log-query-secret",
		"probe-user:probe-pass", "probe-query-secret", "probe-error-secret",
		"glpat-abcdefghijklmnopqrstuvwxyz123456", "private-key-secret",
		"github_pat_abcdefghijklmnopqrstuvwxyz1234567890", "AKIAABCDEFGHIJKLMNOP",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJzZWNyZXQifQ.signaturevalue", "flag-secret",
		"session-token-secret", "openai-api-secret",
		"metadata-secret",
	}
	for _, member := range readArchiveMembers(t, archive.Bytes()) {
		for _, secret := range secrets {
			if bytes.Contains(member, []byte(secret)) {
				t.Errorf("archive member contains unredacted secret %q", secret)
			}
		}
	}
}

func TestWriteTrimsLogsToStartupWindow(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	ready := t0.Add(10 * time.Minute)
	bundle := model.Bundle{
		CapturedAt: t0.Add(time.Hour),
		Pod: corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "trim", CreationTimestamp: metav1.NewTime(t0)},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type:               corev1.PodReady,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(ready),
			}}},
		},
		Logs: []model.LogCapture{
			{
				Container: "../../engine",
				Lines: []model.LogLine{
					{Timestamp: t0.Add(-3 * time.Minute), Text: "too old"},
					{Timestamp: t0.Add(-logTrimMargin), Text: "before margin"},
					{Timestamp: t0.Add(5 * time.Minute), Text: "token=inside-secret"},
					{Timestamp: ready.Add(logTrimMargin), Text: "after margin"},
					{Timestamp: ready.Add(logTrimMargin + time.Nanosecond), Text: "too late"},
				},
			},
			{
				Container: "../../engine",
				Previous:  true,
				Lines:     []model.LogLine{{Timestamp: t0.Add(time.Minute), Text: "previous attempt"}},
			},
		},
	}
	originalLineCount := len(bundle.Logs[0].Lines)

	var archive bytes.Buffer
	if err := Write(&archive, bundle); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if len(bundle.Logs[0].Lines) != originalLineCount {
		t.Fatal("Write() mutated the caller's log lines")
	}
	members := readArchiveMembers(t, archive.Bytes())
	if _, ok := members["logs/engine.log"]; !ok {
		t.Fatalf("sanitized current log path is missing: %v", memberNames(members))
	}
	if _, ok := members["logs/engine-previous.log"]; !ok {
		t.Fatalf("sanitized previous log path is missing: %v", memberNames(members))
	}
	current := string(members["logs/engine.log"])
	if strings.Contains(current, "too old") || strings.Contains(current, "too late") || strings.Contains(current, "inside-secret") {
		t.Errorf("trimmed/redacted log contains unwanted text: %q", current)
	}
	if !strings.Contains(current, "before margin") || !strings.Contains(current, redactedValue) || !strings.Contains(current, "after margin") {
		t.Errorf("startup-window log lines are missing: %q", current)
	}

	got, err := Read(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(got.Logs[0].Lines) != 3 {
		t.Errorf("current capture has %d lines, want 3", len(got.Logs[0].Lines))
	}
	if len(got.Logs[1].Lines) != 1 {
		t.Errorf("previous capture has %d lines, want 1", len(got.Logs[1].Lines))
	}
}

func TestWriteUsesCapturedAtWhenPodIsNotReady(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	bundle := model.Bundle{
		CapturedAt: t0.Add(4 * time.Minute),
		Pod:        corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(t0)}},
		Logs: []model.LogCapture{{Container: "engine", Lines: []model.LogLine{
			{Timestamp: t0.Add(3 * time.Minute), Text: "before capture"},
			{Timestamp: t0.Add(7 * time.Minute), Text: "after capture window"},
		}}},
	}
	var archive bytes.Buffer
	if err := Write(&archive, bundle); err != nil {
		t.Fatal(err)
	}
	got, err := Read(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Logs[0].Lines) != 1 || got.Logs[0].Lines[0].Text != "before capture" {
		t.Errorf("captured-at window produced logs %#v", got.Logs[0].Lines)
	}
}

func TestReadRejectsMaliciousAndInvalidArchives(t *testing.T) {
	validJSON := validBundleJSON(t)
	tests := []struct {
		name    string
		archive func(*testing.T) []byte
		wantErr string
	}{
		{
			name: "parent traversal",
			archive: func(t *testing.T) []byte {
				return makeArchive(t, fixtureMember{name: "../bundle.json", data: validJSON})
			},
			wantErr: "unsafe archive path",
		},
		{
			name: "absolute path",
			archive: func(t *testing.T) []byte {
				return makeArchive(t, fixtureMember{name: "/bundle.json", data: validJSON})
			},
			wantErr: "unsafe archive path",
		},
		{
			name: "symlink",
			archive: func(t *testing.T) []byte {
				return makeArchive(t,
					fixtureMember{name: bundleJSONName, data: validJSON},
					fixtureMember{name: "escape", typeflag: tar.TypeSymlink, linkname: "../../etc/passwd"},
				)
			},
			wantErr: "unsupported archive member type",
		},
		{
			name: "duplicate bundle.json",
			archive: func(t *testing.T) []byte {
				return makeArchive(t,
					fixtureMember{name: bundleJSONName, data: validJSON},
					fixtureMember{name: bundleJSONName, data: validJSON},
				)
			},
			wantErr: "duplicate archive member",
		},
		{
			name: "wrong schema",
			archive: func(t *testing.T) []byte {
				return makeArchive(t, fixtureMember{name: bundleJSONName, data: []byte(`{"schema":"coldstart/v1"}`)})
			},
			wantErr: "unsupported bundle schema",
		},
		{
			name: "missing bundle.json",
			archive: func(t *testing.T) []byte {
				return makeArchive(t, fixtureMember{name: "pod.yaml", data: []byte("kind: Pod\n")})
			},
			wantErr: "missing bundle.json",
		},
		{
			name: "oversized member",
			archive: func(t *testing.T) []byte {
				return makeArchive(t, fixtureMember{name: bundleJSONName, declaredSize: maxArchiveMemberSize + 1})
			},
			wantErr: "exceeds",
		},
		{
			name: "corrupt tar",
			archive: func(t *testing.T) []byte {
				return gzipData(t, []byte("not a tar archive"))
			},
			wantErr: "read tar header",
		},
		{
			name: "corrupt gzip checksum",
			archive: func(t *testing.T) []byte {
				data := makeArchive(t, fixtureMember{name: bundleJSONName, data: validJSON})
				data[len(data)-1] ^= 0xff
				return data
			},
			wantErr: "decompress archive",
		},
		{
			name: "decompression bomb",
			archive: func(t *testing.T) []byte {
				var compressed bytes.Buffer
				gz := gzip.NewWriter(&compressed)
				chunk := bytes.Repeat([]byte{'x'}, 64<<10)
				remaining := maxUncompressedArchiveSize + 1
				for remaining > 0 {
					n := min(remaining, len(chunk))
					if _, err := gz.Write(chunk[:n]); err != nil {
						t.Fatal(err)
					}
					remaining -= n
				}
				if err := gz.Close(); err != nil {
					t.Fatal(err)
				}
				return compressed.Bytes()
			},
			wantErr: "uncompressed archive exceeds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Read(bytes.NewReader(tt.archive(t)))
			if err == nil {
				t.Fatal("Read() succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Read() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestReadAcceptsSafeDirectoryEntries(t *testing.T) {
	archive := makeArchive(t,
		fixtureMember{name: "logs/", typeflag: tar.TypeDir},
		fixtureMember{name: bundleJSONName, data: validBundleJSON(t)},
	)
	got, err := Read(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("Read() rejected safe directory entries: %v", err)
	}
	if got.Schema != model.BundleSchema {
		t.Errorf("schema = %q, want %q", got.Schema, model.BundleSchema)
	}
}

func TestReadRejectsDataAfterTarEnd(t *testing.T) {
	archive := makeArchive(t, fixtureMember{name: bundleJSONName, data: validBundleJSON(t)})
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("smuggled data")...)
	if _, err := Read(bytes.NewReader(gzipData(t, raw))); err == nil || !strings.Contains(err.Error(), "after tar end marker") {
		t.Fatalf("Read() error = %v, want trailing-data rejection", err)
	}
}

func sampleBundle() model.Bundle {
	t0 := time.Date(2026, 11, 27, 2, 41, 9, 0, time.UTC)
	ready := t0.Add(5 * time.Minute)
	return model.Bundle{
		Schema:     model.BundleSchema,
		Source:     "unit-test",
		CapturedAt: ready.Add(time.Minute),
		Pod: corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "qwen-test",
				Namespace:         "inference",
				CreationTimestamp: metav1.NewTime(t0),
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "vllm:test"}}},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type:               corev1.PodReady,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(ready),
			}}},
		},
		Node:   &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		Events: []corev1.Event{{Reason: "Started", Message: "container started"}},
		Logs: []model.LogCapture{
			{Container: "engine", Lines: []model.LogLine{{Timestamp: t0.Add(time.Minute), Text: "engine boot"}}},
			{Container: "engine", Previous: true, Lines: []model.LogLine{{Timestamp: t0.Add(2 * time.Minute), Text: "previous attempt"}}},
		},
		Metadata: map[string]string{"cluster": "test-cluster"},
	}
}

type fixtureMember struct {
	name         string
	data         []byte
	typeflag     byte
	linkname     string
	declaredSize int64
}

func makeArchive(t *testing.T, members ...fixtureMember) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tw := tar.NewWriter(gz)
	allowShortMember := false
	for _, member := range members {
		typeflag := member.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		size := int64(len(member.data))
		if member.declaredSize != 0 {
			size = member.declaredSize
			allowShortMember = true
		}
		hdr := &tar.Header{
			Name:     member.name,
			Mode:     0o600,
			Size:     size,
			Typeflag: typeflag,
			Linkname: member.linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write fixture tar header: %v", err)
		}
		if member.declaredSize == 0 && typeflag == tar.TypeReg {
			if _, err := tw.Write(member.data); err != nil {
				t.Fatalf("write fixture tar member: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil && !allowShortMember {
		t.Fatalf("close fixture tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close fixture gzip: %v", err)
	}
	return compressed.Bytes()
}

func gzipData(t *testing.T, data []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func validBundleJSON(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(model.Bundle{Schema: model.BundleSchema})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readArchiveMembers(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("open archive gzip: %v", err)
	}
	defer func() {
		// The archive is fully consumed above; gzip close errors cannot affect
		// the extracted test members.
		_ = gz.Close()
	}()
	tr := tar.NewReader(gz)
	members := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive tar: %v", err)
		}
		if !validArchivePath(hdr.Name) {
			t.Fatalf("Write() produced unsafe path %q", hdr.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read archive member %q: %v", hdr.Name, err)
		}
		members[hdr.Name] = data
	}
	return members
}

func memberNames(members map[string][]byte) []string {
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	return names
}
