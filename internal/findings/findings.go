// Package findings maps observed timeline evidence to ranked, concrete fixes.
package findings

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/vatsalyatandon/coldstart/internal/model"
	"github.com/vatsalyatandon/coldstart/internal/rules"
)

// Build applies the initial v0.1 findings ruleset. Estimates are deliberately
// transparent: the JSON includes both observed and recoverable seconds.
func Build(report model.Report, selection rules.Selection, flags map[string]bool) []model.Finding {
	phases := aggregate(report.Phases)
	total := report.TotalSecs
	var result []model.Finding

	add := func(f model.Finding) {
		if math.IsNaN(f.EstRecoverableSecs) || math.IsInf(f.EstRecoverableSecs, 0) {
			f.EstRecoverableSecs = 0
		}
		if f.EstRecoverableSecs < 0 {
			f.EstRecoverableSecs = 0
		}
		if f.ObservedSecs > 0 {
			f.Text = fmt.Sprintf("%s %.1fs: %s", phaseLabel(f.Phase), f.ObservedSecs, f.Recommendation)
		}
		result = append(result, f)
	}

	if observed := phases["image_pull"]; observed.secs > 0 && total > 0 && observed.secs/total > 0.20 && phaseAttr(observed.attrs, "cached") == false && hasAttr(observed.attrs, "cached") {
		add(model.Finding{Phase: "image_pull", ObservedSecs: observed.secs, Condition: "image pull exceeds 20% of startup and image was not cached", Recommendation: "Pre-pull with a DaemonSet, use a lazy/parallel-pull snapshotter such as SOCI or stargz, or slim the engine image.", EstRecoverableSecs: observed.secs * 0.75, Confidence: "high"})
	}

	remote := flags["weights_remote_download"]
	if !remote {
		remote = phaseAttr(phases["init_containers"].attrs, "remote_download") == true
	}
	download := phases["weights_download"].secs
	if remote {
		download += phases["init_containers"].secs
	}
	if download > 0 && remote {
		add(model.Finding{Phase: "weights_download", ObservedSecs: download, Condition: "weights downloaded from a remote hub during startup", Recommendation: recommendation(selection, "weights_remote_download", "Pre-stage weights to a PVC or node-local NVMe; consider OCI image volumes or streaming loaders."), EstRecoverableSecs: download, Confidence: confidence(selection, "weights_remote_download", "high")})
	}

	if observed := phases["weights_load"].secs; observed > 0 {
		if bytes, ok := numberAttr(phases["weights_load"].attrs, "bytes"); ok && bytes > 0 {
			reference := 2.0e9
			storage := report.Hardware.Storage
			if storage == "local-nvme" {
				reference = 5.0e9
			}
			expected := bytes / reference
			if observed > expected*1.20 {
				condition := fmt.Sprintf("load throughput %.2f GB/s is below the %.2f GB/s %s reference", bytes/observed/1e9, reference/1e9, storage)
				loadConfidence := confidence(selection, "weights_load_slow", "medium")
				if storage == "" {
					condition = fmt.Sprintf("load throughput %.2f GB/s is below the %.2f GB/s reference assumed for network storage; confirm the storage class", bytes/observed/1e9, reference/1e9)
					loadConfidence = "low"
				}
				add(model.Finding{Phase: "weights_load", ObservedSecs: observed, Condition: condition, Recommendation: recommendation(selection, "weights_load_slow", "Check the storage class (network PV versus local NVMe), prefer safetensors over .bin, and evaluate a streaming loader."), EstRecoverableSecs: observed - expected, Confidence: loadConfidence})
			}
		}
	}

	compileSecs := phases["torch_compile"].secs
	cache := phaseAttr(phases["torch_compile"].attrs, "cache")
	cacheMiss := flags["compile_cache_miss"] || cache == "miss"
	cacheCondition := "compile cache miss"
	cacheConfidence := confidence(selection, "compile_cache_miss", "high")
	if !cacheMiss && cache == "unknown" && compileSecs > 30 {
		cacheMiss = true
		cacheCondition = "compile cache miss inferred: no cache-hit marker observed"
		cacheConfidence = "medium"
	}
	if compileSecs > 0 && cacheMiss {
		recoverable := compileSecs - math.Min(50, compileSecs*0.30)
		add(model.Finding{Phase: "torch_compile", ObservedSecs: compileSecs, Condition: cacheCondition, Recommendation: recommendation(selection, "compile_cache_miss", "Persist the engine compile cache directory on a volume shared across restarts."), EstRecoverableSecs: recoverable, Confidence: cacheConfidence})
	}
	if compileSecs > 30 {
		add(model.Finding{Phase: "torch_compile", ObservedSecs: compileSecs, Condition: "large compile phase and latency-tolerant workload", Recommendation: recommendation(selection, "compile_large_eager", "Consider eager mode if throughput trade-offs are acceptable; this removes compilation but can reduce serving throughput."), EstRecoverableSecs: compileSecs, Confidence: "low"})
	}
	if graph := phases["cuda_graph"].secs; graph > 20 {
		add(model.Finding{Phase: "cuda_graph", ObservedSecs: graph, Condition: "CUDA graph capture exceeds 20 seconds", Recommendation: recommendation(selection, "cuda_graph_large", "Reduce the number of captured batch sizes or use a smaller capture-size set."), EstRecoverableSecs: graph * 0.30, Confidence: "low"})
	}
	if node := phases["node_provisioning"].secs; node > 0 && total > 0 && node/total > 0.20 {
		add(model.Finding{Phase: "node_provisioning", ObservedSecs: node, Condition: "node provisioning exceeds 20% of startup", Recommendation: "Warm the GPU node pool, reserve capacity, or pre-provision driver images. Recoverable time is platform-dependent.", EstRecoverableSecs: 0, Confidence: "low"})
	}
	if lag := phases["readiness_lag"].secs; lag > 0 {
		period := 30.0
		if configured, ok := numberAttr(phases["readiness_lag"].attrs, "probe_period_secs"); ok && configured > 0 {
			period = configured
		}
		if lag > period {
			add(model.Finding{Phase: "readiness_lag", ObservedSecs: lag, Condition: fmt.Sprintf("readiness lag exceeds one %.0f-second probe period", period), Recommendation: "Use a startupProbe and shorten readiness periodSeconds once the server has started.", EstRecoverableSecs: lag - period, Confidence: "high"})
		}
	}

	var lost float64
	var reasons []string
	for _, attempt := range report.Attempts {
		if attempt.Outcome == "failed" {
			lost += attempt.LostSecs
			if attempt.CrashReason != "" {
				reasons = append(reasons, attempt.CrashReason)
			}
		}
	}
	if lost > 0 {
		condition := "one or more startup attempts failed"
		if len(reasons) > 0 {
			condition += ": " + strings.Join(unique(reasons), ", ")
		}
		text := "Fix the crash cause before optimizing startup; the failed attempts consumed the reported time."
		if flags["oom"] || strings.Contains(strings.ToLower(strings.Join(reasons, " ")), "oom") {
			text = recommendation(selection, "oom", text)
		}
		add(model.Finding{Phase: "restart_attempt", ObservedSecs: lost, Condition: condition, Recommendation: text, EstRecoverableSecs: lost, Confidence: "high"})
	}
	if report.UnattributedSecs > 0 && total > 0 && report.UnattributedSecs/total > 0.10 {
		add(model.Finding{Phase: "unattributed", ObservedSecs: report.UnattributedSecs, Condition: "more than 10% of startup has no matching marker", Recommendation: "Gap not explained. Capture a reproducible bundle with `-o bundle` and open an issue.", EstRecoverableSecs: 0, Confidence: "low"})
	}

	sort.SliceStable(result, func(i, j int) bool {
		if result[i].EstRecoverableSecs != result[j].EstRecoverableSecs {
			return result[i].EstRecoverableSecs > result[j].EstRecoverableSecs
		}
		if result[i].ObservedSecs != result[j].ObservedSecs {
			return result[i].ObservedSecs > result[j].ObservedSecs
		}
		return result[i].Phase < result[j].Phase
	})
	return result
}

type aggregatePhase struct {
	secs  float64
	attrs map[string]any
}

func aggregate(phases []model.Phase) map[string]aggregatePhase {
	out := make(map[string]aggregatePhase)
	for _, phase := range phases {
		value := out[phase.ID]
		value.secs += phase.Secs
		if value.attrs == nil {
			value.attrs = make(map[string]any)
		}
		for key, attr := range phase.Attrs {
			if number, ok := attr.(float64); ok {
				if old, ok := value.attrs[key].(float64); !ok || number > old {
					value.attrs[key] = number
				}
			} else if _, exists := value.attrs[key]; !exists || attr == true {
				value.attrs[key] = attr
			}
		}
		out[phase.ID] = value
	}
	return out
}

func phaseAttr(attrs map[string]any, key string) any {
	if attrs == nil {
		return nil
	}
	return attrs[key]
}

func hasAttr(attrs map[string]any, key string) bool {
	_, ok := attrs[key]
	return ok
}

func numberAttr(attrs map[string]any, key string) (float64, bool) {
	if attrs == nil {
		return 0, false
	}
	value, ok := attrs[key].(float64)
	return value, ok
}

func recommendation(selection rules.Selection, key, fallback string) string {
	if selection.Pack.Recommendations != nil {
		if value, ok := selection.Pack.Recommendations[key]; ok && value.Text != "" {
			return value.Text
		}
	}
	return fallback
}

func confidence(selection rules.Selection, key, fallback string) string {
	if selection.Pack.Recommendations != nil {
		if value, ok := selection.Pack.Recommendations[key]; ok && value.Confidence != "" {
			return value.Confidence
		}
	}
	return fallback
}

func phaseLabel(id string) string {
	labels := map[string]string{
		"image_pull": "image pull", "weights_download": "weights download", "weights_load": "weights load",
		"torch_compile": "torch.compile", "cuda_graph": "CUDA graph capture", "node_provisioning": "node provisioning",
		"readiness_lag": "readiness lag", "restart_attempt": "failed startup attempts", "unattributed": "unattributed",
	}
	if label := labels[id]; label != "" {
		return label
	}
	return strings.ReplaceAll(id, "_", " ")
}

func unique(values []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
