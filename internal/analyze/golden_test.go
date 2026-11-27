package analyze

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vatsalyatandon/coldstart/internal/model"
)

// TestGoldenFixtures is both the deterministic fixture suite and the explicit
// update mechanism. Run UPDATE_GOLDEN=1 go test ./internal/analyze to regenerate
// expected output after an intentional rule/report change.
func TestGoldenFixtures(t *testing.T) {
	cases := map[string]func() model.Bundle{
		"cold-vllm-cache-miss": coldVLLMBundle,
		"warm-vllm-cache-hit":  warmBundle,
		"crash-loop":           crashBundle,
		"expired-events": func() model.Bundle {
			bundle := warmBundle()
			bundle.Events = nil
			bundle.CapturedAt = fixtureT0.Add(2 * time.Hour)
			return bundle
		},
		"clock-skew": func() model.Bundle {
			bundle := warmBundle()
			bundle.Pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(fixtureT0.Add(-time.Minute))
			bundle.Logs[0].Lines[0].Timestamp = fixtureT0.Add(-30 * time.Second)
			return bundle
		},
		"sglang": sglangBundle,
		"vllm-legacy": func() model.Bundle {
			bundle := warmBundle()
			bundle.Pod.Spec.Containers[0].Image = "vllm/vllm-openai:v0.10.1.1"
			for i := range bundle.Logs[0].Lines {
				bundle.Logs[0].Lines[i].Text = replaceAll(bundle.Logs[0].Lines[i].Text, "0.30.0", "0.10.1.1")
			}
			return bundle
		},
		"tensor-parallel": func() model.Bundle {
			bundle := warmBundle()
			bundle.Logs[0].Lines = append(bundle.Logs[0].Lines,
				model.LogLine{Timestamp: fixtureT0.Add(61 * time.Second), Text: "(Worker_TP1 pid=2) Loading weights took 45.10 seconds"},
			)
			return bundle
		},
		"missing-markers": func() model.Bundle {
			bundle := baseBundle(200)
			bundle.Node = nodeFixture(-3600, -3500)
			bundle.Logs = nil
			return bundle
		},
		"first-token-probe": func() model.Bundle {
			bundle := warmBundle()
			request := fixtureT0.Add(200 * time.Second)
			bundle.Probe = &model.ProbeResult{Endpoint: "/v1/completions", Model: "test-model", RequestSentAt: request, FirstTokenAt: request.Add(400 * time.Millisecond), FirstToken: "Hello", StatusCode: 200}
			return bundle
		},
	}

	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) < 10 {
		t.Fatalf("golden corpus has %d cases, want at least 10", len(names))
	}

	update := os.Getenv("UPDATE_GOLDEN") == "1"
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			bundle := cases[name]()
			bundle.Metadata = map[string]string{"fixture": "synthetic", "case": name}
			report, err := Analyze(bundle, Options{})
			if err != nil {
				t.Fatal(err)
			}
			assertAccounting(t, report)

			base := filepath.Join("..", "..", "testdata", "fixtures")
			bundlePath := filepath.Join(base, name+".bundle.json")
			reportPath := filepath.Join(base, name+".report.json")
			if update {
				if err := os.MkdirAll(base, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := writeIndentedJSON(bundlePath, bundle); err != nil {
					t.Fatal(err)
				}
				if err := writeIndentedJSON(reportPath, report); err != nil {
					t.Fatal(err)
				}
				return
			}

			var want model.Report
			if err := readJSON(reportPath, &want); err != nil {
				t.Fatal(err)
			}
			gotJSON, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			wantJSON, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			var gotValue, wantValue any
			if err := json.Unmarshal(gotJSON, &gotValue); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(wantJSON, &wantValue); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotValue, wantValue) {
				t.Fatalf("report changed; run UPDATE_GOLDEN=1 go test ./internal/analyze after reviewing the difference\n got: %s\nwant: %s", gotJSON, wantJSON)
			}

			// Ensure the checked-in input itself remains replayable.
			var stored model.Bundle
			if err := readJSON(bundlePath, &stored); err != nil {
				t.Fatal(err)
			}
			replayed, err := Analyze(stored, Options{})
			if err != nil {
				t.Fatal(err)
			}
			replayedJSON, _ := json.Marshal(replayed)
			if !reflect.DeepEqual(gotValue, decodeJSON(t, replayedJSON)) {
				t.Fatalf("report generated from live fixture differs from bundle replay\n got: %s\nreplay: %s", gotJSON, replayedJSON)
			}
		})
	}
}

func writeIndentedJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func replaceAll(value, old, new string) string {
	return strings.ReplaceAll(value, old, new)
}
