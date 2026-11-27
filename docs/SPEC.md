# coldstart — Specification v0.1

**Status:** Draft · **Author:** Vatsalya · **Date:** 27 Nov 2026 · **License (proposed):** Apache-2.0

---

## 0. Summary

`coldstart` is a read-only `kubectl` plugin. It answers one question: *"Where did my LLM pod's startup time go, and what should I fix first?"*

You point it at a vLLM or SGLang pod. It joins three sources that today live in separate places:

1. Kubernetes pod and node state, and events
2. Container log timestamps
3. An optional first-token probe

From these it builds one timeline, from **pod created** to **first token served**. Every second is assigned to a named phase. The phase that dominates is mapped to a concrete, known fix.

```
$ kubectl coldstart pod/qwen3-35b-7f9c-xk2p -n inference

qwen3-35b-7f9c-xk2p  ·  vLLM 0.30.0  ·  1x H100 80GB  ·  total 9m 41s  (illustrative)

  scheduling wait     ██████                                    1m 02s   11%
  node bring-up       ████                                        41s    7%
  image pull          ███████████                               1m 58s   20%  (not cached)
  weights download    ████████████████                          2m 51s   29%  ← dominant
  weights load        ███                                         29s    5%
  torch.compile       █████                                       53s    9%  (cache MISS)
  cuda graph capture  ██                                          21s    4%
  kv cache / warmup   █                                           12s    2%
  readiness lag       ██                                          24s    4%  (probe period 30s)
  first token         ▏                                          0.4s    0%
  unattributed        █                                           10s    2%

Findings (ranked by seconds recoverable):
  1. weights download 2m51s: weights pulled from HF Hub at startup. Pre-stage to a PVC or node-local NVMe.
  2. image pull 1m58s: 11.2 GB image not cached on a fresh node. Pre-pull DaemonSet or lazy-pull snapshotter.
  3. torch.compile 53s: compile cache miss. Persist the vLLM compile cache dir on a volume.
```

**Why this is new.** Existing work covers one layer at a time:

- [upb-cn/vllm-startup-profiler](https://github.com/upb-cn/vllm-startup-profiler) (MLSys 2026, 12★) only covers the engine. It works by patching a pinned vLLM version (0.10.1.1) to add profiling logs, runs offline benchmark sweeps and trains a latency predictor. It does not look at Kubernetes at all.
- Vendor write-ups (Microsoft, AWS/New Stack) break the problem down well, but each uses a one-off harness.

Nobody gives an operator a breakdown of *their own pod, after the fact, with no changes to it*.

**Build estimate:** 10 working days for v0.1 (Section 10).

---

## 1. Problem

Self-hosted LLM pods take minutes to become ready. Published breakdowns show the bottleneck **moves**, depending on the engine, model size and how warm the node is:

- **vLLM:** startup (compile plus CUDA graphs) dominates. A smaller image barely helps: dropping gzip gave only a 1.02x speedup, against 1.35x for llama.cpp. (Microsoft)
- **64 GB model:** torch.compile is about 65% of model startup. **203 GB model:** weights loading is about 92%. (AWS / The New Stack)
- **Fresh nodes:** driver load, image pull and weight download add minutes before the engine even starts.

Operators today guess. They tune the wrong layer, or they write a one-off script. The vLLM issue tracker has upvoted startup-time issues, and none of them come with a standard way to report *where* the time went.

## 2. Users and use cases

| User | Situation | What they need from coldstart |
|---|---|---|
| Platform / MLOps engineer | Autoscaling from zero or bursty scale-up is too slow | Which layer to fix first, and how much time each fix recovers |
| Inference engineer | Upgraded vLLM, startup got slower | A before/after comparison of two pods by phase |
| OSS maintainer / issue reporter | Filing a "startup is slow" issue | A pasteable, standard breakdown (`-o markdown`) |
| Small team on a single node | Pod takes 12 min on an H100 | A plain-English diagnosis with no extra infrastructure |

## 3. Goals and non-goals

**Goals (v0.1)**
- G1. A per-phase breakdown for **one pod**, after the fact. It needs no sidecar, no engine patch and no restart.
- G2. Supports **vLLM** and **SGLang** (single-node, tensor parallelism on one pod).
- G3. Every finding is mapped to a ranked, concrete fix.
- G4. Time that cannot be attributed is reported openly, never hidden.
- G5. Read-only RBAC. Ships as a single static binary installable via krew.

**Non-goals (v0.1)**
- Making startup faster. coldstart diagnoses and the user fixes.
- Predicting startup time for configs that haven't run (the upb predictor does this).
- Multi-node / pipeline-parallel startup (Ray, LWS), deferred to v0.3.
- Serverless platforms that hide the pod (Modal, RunPod serverless).
- llama.cpp, TGI and TensorRT-LLM. The rule-pack format allows them later.

## 4. Phase model

The timeline is anchored on **T0 = pod `metadata.creationTimestamp`**. Phases may be nested (shown indented). Sibling phases never overlap. Gaps become `unattributed`.

| # | Phase | Start signal | End signal | Source |
|---|---|---|---|---|
| P1 | **Scheduling wait** | T0 | `PodScheduled` condition → True | Pod status |
| P1a | ↳ node provisioning | Autoscaler event (`TriggeredScaleUp` / Karpenter `Nominated`) or new node creation | Node `Ready` condition → True | Events, Node |
| P1b | ↳ GPU becomes allocatable | Node Ready | Device-plugin / GPU-operator pod Ready on that node | Pods in operator namespace |
| P2 | **Image pull** | `Pulling` event | `Pulled` event (the kubelet message includes the duration and whether the image was already present) | Events |
| P3 | **Init containers** (usually the weights download) | `initContainerStatuses[].state.terminated.startedAt` | `.finishedAt` | Pod status |
| P4 | **Container start → engine boot** | `containerStatuses[].state.running.startedAt` | First engine log line | Status + logs |
| P5 | **Framework bootstrap** (imports, config, tokenizer) | First engine log line | Weight-load start marker | Logs |
| P6 | **Weights download (in-engine)** | Download/fetch marker, if present | Load start | Logs |
| P7 | **Weights load** | Load start marker | Load end marker | Logs |
| P8 | **torch.compile** (with cache hit/miss) | Compile start marker | Compile end marker | Logs |
| P9 | **CUDA graph capture** | Capture start | Capture end | Logs |
| P10 | **KV cache profiling / warmup** | Profile start | Engine-init end | Logs |
| P11 | **API server up** | Engine-init end | "server listening / ready" marker | Logs |
| P12 | **Readiness lag** | Server-ready marker | `Ready` condition → True | Logs + status |
| P13 | **First token** *(optional)* | Probe request sent | First streamed token | Active probe |

**Restarts.** If `restartCount > 0`, each attempt gets its own timeline. Earlier attempts use `kubectl logs --previous`. The report shows how much time was **lost to crashes**, for example an OOM during KV-cache profiling.

### 4.1 Log markers (rule packs)

Engine phases come from **versioned rule packs**: YAML files of regexes keyed by engine and version range. They are not hard-coded.

```yaml
# rules/vllm/>=0.11,<0.40.yaml   (patterns are placeholders, verify against real logs)
engine: vllm
detect: ['vLLM API server version (?P<version>\S+)', 'Initializing a V1 LLM engine']
phases:
  weights_load:
    end:   'Loading weights took (?P<secs>[\d.]+) seconds'
    duration_from: secs          # engine-reported duration preferred over timestamp diff
  torch_compile:
    end:   'torch.compile takes (?P<secs>[\d.]+) s in total'
    cache_hit: 'Directly load the compiled graph'
  cuda_graph:
    end:   'Graph capturing finished in (?P<secs>[\d.]+) secs'
  engine_init:
    end:   'init engine .* took (?P<secs>[\d.]+) seconds'
  server_ready:
    end:   'Application startup complete'
```

**Rules for rule packs**

- If the engine prints its own duration, use it. Otherwise subtract log timestamps (`kubectl logs --timestamps`).
- Every pack ships with **golden fixtures**: real captured log and event dumps, plus the expected output. CI fails if a pack stops matching its fixtures.
- The SGLang pack covers the same markers (weight load, CUDA graph capture, "server is ready"). The exact strings must be verified against real logs.
- If no pack matches the version, fall back to the nearest one and print `⚠ rule pack for vllm 0.31 not found; used 0.30 rules`.

> **Task D1 (day 1):** capture real logs from 3 vLLM versions and 2 SGLang versions before writing any regex. The marker strings above are placeholders until confirmed against those logs.

## 5. Findings engine

Each finding is: `{phase, observed_secs, condition, recommendation, est_recoverable_secs, confidence}`.

Findings are ranked by **estimated recoverable seconds**, not by phase size. A 3-minute phase with no available fix should rank below a 1-minute phase with an obvious one.

| Trigger | Recommendation (initial ruleset) | Recoverable estimate |
|---|---|---|
| P2 image pull > 20% of total and the image was not present | Pre-pull DaemonSet; lazy/parallel-pull snapshotter (SOCI, stargz); slimmer engine image | ~70–80% of P2 |
| P3/P6 weights downloaded from a remote hub at startup | Pre-stage weights to a PVC or node-local NVMe; OCI image volumes; streaming loaders | ~all of the download |
| P7 load throughput < expected for the storage type | Check the storage class (network PV vs local NVMe); safetensors vs .bin; streaming loader | P7 − (size ÷ reference throughput) |
| P8 compile cache miss | Persist the vLLM compile cache directory on a volume shared across restarts | ~most of P8 (reported cache-hit restarts take about 45–60s in total) |
| P8 large and latency-tolerant | Consider eager mode (trades throughput for startup). Flag as a trade-off. | All of P8, low confidence |
| P9 CUDA graph capture large | Reduce the number of capture sizes | Partial, low confidence |
| P1a node provisioning dominant | Warm GPU node pool / capacity reservation / pre-provisioned driver images | Platform-dependent |
| P12 readiness lag > probe period | Use a `startupProbe`; shorten `periodSeconds` once started | P12 − 1 probe period |
| Restart attempts > 0 | Show the crash reason (OOMKilled, exit code) and the startup time lost to it | Time spent in failed attempts |
| `unattributed` > 10% | "Gap not explained. Please open an issue with `-o bundle`." | — |

Recommendations stay engine-version-aware. Flag names live in the rule pack, not in code, so they can be fixed without a release.

## 6. Interfaces

### 6.1 CLI

```
kubectl coldstart pod/<name> [-n ns]          # analyze one pod (default: text waterfall)
kubectl coldstart deploy/<name>               # newest pod of the deployment
kubectl coldstart pod/<a> --compare pod/<b>   # phase-by-phase diff (e.g. before/after upgrade)
kubectl coldstart pod/<name> --probe          # also measure first token via port-forward
kubectl coldstart --from-bundle bundle.tgz    # offline analysis of a captured bundle

Flags:
  -o text|json|markdown|html|bundle
  --container <name>        # if the engine container isn't auto-detected
  --rules <dir>             # custom rule packs
  --probe-prompt "<text>"   # default: a 1-token completion
```

### 6.2 Outputs
- **text:** the waterfall plus findings (above).
- **markdown:** the same content, built to be pasted into a GitHub issue. This is the main growth channel.
- **json:** stable schema (Section 6.3) for CI and dashboards.
- **html:** a single self-contained file with an interactive timeline.
- **bundle:** a tarball of the raw inputs (events, pod and node YAML, logs). Secrets and env values are redacted. It can be replayed with `--from-bundle`, which makes the tool's own bug reports easy to reproduce.

### 6.3 JSON schema (abridged)
```json
{
  "schema": "coldstart/v1",
  "pod": "ns/name", "engine": {"name": "vllm", "version": "0.30.0"},
  "hardware": {"gpu": "H100-80GB", "count": 1, "node": "ip-10-0-3-7", "node_fresh": true},
  "t0": "2026-11-27T02:41:09Z", "total_secs": 581.2,
  "attempts": [{
    "index": 0, "outcome": "ready",
    "phases": [{"id": "image_pull", "start": "...", "end": "...", "secs": 118.0,
                "source": "event", "confidence": "high", "attrs": {"cached": false, "image_bytes": 11200000000}}]
  }],
  "unattributed_secs": 10.1,
  "findings": [{"phase": "weights_download", "recoverable_secs": 160, "confidence": "high", "text": "..."}],
  "warnings": ["events older than 1h may have expired"]
}
```

## 7. Architecture

```
            ┌────────────── collectors (read-only) ──────────────┐
 pod ref ─▶ │ PodCollector   NodeCollector   EventCollector      │
            │ LogCollector (--timestamps, --previous)  Prober    │
            └───────────────┬────────────────────────────────────┘
                            ▼
                 Raw bundle (normalized, redacted)
                            ▼
            Engine detector ─▶ Rule-pack matcher ─▶ Phase builder
                            ▼                          │ (clock-skew correction,
                    Timeline (attempts × phases)  ◀────┘  gap = unattributed)
                            ▼
                    Findings engine (ruleset)
                            ▼
                Renderers: text · md · json · html · bundle
```

- **Language:** Go. It uses client-go, fits the kubectl/krew ecosystem and ships as one static binary. Rule packs and the findings ruleset are embedded YAML that can be overridden with `--rules`.
- **Pure core:** everything after the bundle step is a pure function of the bundle. This makes the golden-fixture tests deterministic and lets `--from-bundle` work offline.
- **RBAC:** `get/list` on pods, pods/log, events, nodes. `create` on pods/portforward only with `--probe`. Ships a ready-made `ClusterRole` YAML.

## 8. Known hard problems and how v0.1 handles them

| Problem | Handling |
|---|---|
| **Event TTL.** Kubernetes events expire, by default after about 1 hour. | Warn when T0 is more than 50 min ago. Phases are rebuilt from durable pod/node status where possible (container `startedAt`, condition transition times). v0.2 adds `coldstart watch`, a lightweight recorder that stores events for a namespace. |
| **Clock skew.** Events are stamped by the kubelet/API server, logs by the container runtime on the node. | Use the container `startedAt` (node clock) as the bridge between the two. Detect negative or overlapping durations and clamp them, with a warning. Never report a negative phase. |
| **Log format drift** across engine versions | Versioned rule packs plus golden fixtures in CI. Unknown versions fall back to the nearest pack, with a warning. Gaps go to `unattributed`. |
| **Interleaved worker logs** (tensor parallelism > 1 within one pod) | Key markers on rank 0 / the driver process. Report the slowest worker for load and compile when ranks log separately. |
| **Wrapped deployments** (KServe, llm-d, Dynamo, custom operators) | v0.1 just needs a pod. The engine container is auto-detected by its log signature, and `--container` overrides it. |
| **Weights downloaded by a sidecar or init script without markers** | Fall back to the init-container duration, or attribute the gap to `pre-engine (unknown)`, and say so. |
| **Secrets in logs and env** | Bundles redact env values, tokens (HF_TOKEN patterns) and URLs with credentials. Logs are trimmed to the startup window. |

## 9. Testing and validation

1. **Golden fixtures (unit).** At least 10 real captured bundles, covering a warm node, a cold node, a cache hit vs miss, a crash-loop, SGLang, and 2 vLLM versions. Each bundle has an expected JSON output. CI runs all of them.
2. **Synthetic edge cases.** Hand-built bundles for skew, expired events, restarts and missing markers.
3. **One real end-to-end run.** A rented single-GPU node for a few hours: a managed k8s cluster, a deploy that scales from zero, `coldstart` before and after applying its own top recommendation. **The before/after pair is the README's headline.**
4. **Accuracy check.** The sum of attributed phases plus unattributed time must equal T0→Ready within ±1s on every fixture.

## 10. Build plan (10 working days)

| Day | Deliverable |
|---|---|
| 1 | Capture real logs and events: vLLM ×3 versions, SGLang ×2, cold and warm node. Confirm the log markers. |
| 2 | Collectors + bundle format + redaction; `--from-bundle` |
| 3 | Phase builder for k8s phases (P1–P4, P12), skew handling, restarts |
| 4 | Rule-pack loader + vLLM pack + fixtures |
| 5 | SGLang pack + engine auto-detect + fallback logic |
| 6 | Findings engine + initial ruleset |
| 7 | Renderers: text waterfall, markdown, json |
| 8 | `--compare`, `--probe`, html renderer |
| 9 | Real GPU end-to-end run, before/after, fix what breaks |
| 10 | README with the real waterfall, krew manifest, ClusterRole, release v0.1.0 |

The v0.1 scope can be cut to 1 week by dropping html, `--probe` and `--compare`. That leaves analyze + text/markdown/json, vLLM only.

## 11. Roadmap

- **v0.2:** `coldstart watch` (an event recorder Deployment), Prometheus metrics per phase (`coldstart_phase_seconds{phase,engine}`), and a summary across many pods of a Deployment (p50/p95 per phase).
- **v0.3:** Multi-node startup (LWS / Ray), llama.cpp and TGI rule packs, and a GitHub Action that fails CI if startup regresses past a threshold.
- **Upstream (stretch, high leverage):** propose to vLLM and SGLang a **single structured startup-timing log line** (JSON, one event per phase). This would remove the need for regexes and make coldstart the reference consumer. It also makes the project credible to maintainers.

## 12. Success criteria (first 60 days)

- 5 external users post coldstart output in vLLM, SGLang or the project's own issues (the real signal).
- At least one finding acted on and reported back, with a measured before/after.
- Rule packs cover the latest 3 minor versions of vLLM and SGLang.
- Stars are a secondary signal and are not a target.

## 13. Launch plan

1. README headline: the real before/after waterfall from Section 9.3.
2. Comment with real output on the open vLLM startup-time issues (useful, not promotional).
3. Post to r/LocalLLaMA, the vLLM Slack and Hacker News (a "Show HN" with the before/after).
4. Optionally, a short write-up: "Where an H100 pod's 10 minutes actually go".

## 14. Open questions

1. Name: check that `coldstart` is free on krew, GitHub and Homebrew. The fallback is `kubectl-llmstart`.
2. Should `--probe` support OpenAI-compatible auth headers in v0.1?
3. Should the upb-cn authors be contacted? Their predictor could be an optional "expected vs observed" column, which would differentiate coldstart and credit their work.
4. Should Kubernetes support be split from a plain `coldstart logs <file>` mode for Docker/bare-metal users? It is cheap to add and widens the audience.

## References

- Kabakibo, Trivedi, Wang. *Breaking the Ice: Analyzing Cold Start Latency in vLLM*, MLSys 2026. https://arxiv.org/abs/2606.07362 · code: https://github.com/upb-cn/vllm-startup-profiler
- Microsoft Tech Community. *Dissecting LLM container cold start: where the time actually goes.* https://techcommunity.microsoft.com/blog/linuxandopensourceblog/dissecting-llm-container-cold-start-where-the-time-actually-goes/4508831
- The New Stack. *Cut GPU inference cold start from 8 minutes to less than a minute* (Sep 2026). https://thenewstack.io/cut-gpu-cold-starts/
