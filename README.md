# coldstart

`coldstart` is a read-only `kubectl` plugin that answers: **where did my LLM pod's startup time go, and what should I fix first?**

It joins Kubernetes pod/node state, events, container logs, and an optional first-token probe into one accounted timeline from pod creation to readiness (or first token with `--probe`). v0.1 targets single-node vLLM and SGLang pods.

> **Project status:** early v0.1 implementation. The example below is illustrative, not a measured benchmark. The bundled rule packs use markers verified against public vLLM/SGLang logs; the repository's bundle fixtures are synthetic until real cluster captures are added.

## Example

```text
$ kubectl coldstart pod/qwen3 -n inference
qwen3 · vLLM 0.30.0 · 1x H100 80GB · total 9m 41s

  scheduling wait     ██████                         1m 02s   11%
  node bring-up       ████                               41s    7%
  image pull          ███████████                    1m 58s   20%  (not cached)
  weights download    ████████████████               2m 51s   29%  ← dominant
  weights load        ███                               29s    5%
  torch.compile       █████                            53s    9%  (cache MISS)
  cuda graph capture  ██                                21s    4%
  kv cache / warmup   █                                 12s    2%
  readiness lag       ██                                24s    4%
  first token         ▏                                 0.4s   0%
  unattributed        █                                 10s    2%

Findings (ranked by estimated recoverable seconds):
  1. weights download 2m51s: pre-stage weights to a PVC or node-local NVMe.
  2. image pull 1m58s: pre-pull with a DaemonSet or a lazy-pull snapshotter.
  3. torch.compile 53s: persist the compile cache on a shared volume.
```

## Install

### Build locally

```sh
make build
sudo install -m 0755 bin/kubectl-coldstart /usr/local/bin/kubectl-coldstart
kubectl coldstart --help
```

Or:

```sh
go install github.com/vatsalyatandon/coldstart/cmd/coldstart@latest
```

The resulting binary must be named `kubectl-coldstart` for kubectl plugin discovery.

### RBAC

Apply the read-only role in `deploy/clusterrole.yaml`. `pods/portforward` is included only because the optional `--probe` uses an in-process port-forward; omit that rule if probing is not allowed.

```sh
kubectl apply -f deploy/clusterrole.yaml
kubectl create clusterrolebinding coldstart \
  --clusterrole=coldstart \
  --serviceaccount=<namespace>:<service-account>
```

For a quick local admin-context trial, no additional binding is needed.

## Usage

```sh
# One pod
kubectl coldstart pod/qwen3-7f9c-xk2p -n inference

# Newest pod from a deployment
kubectl coldstart deploy/qwen3 -n inference

# Before/after comparison
kubectl coldstart pod/qwen3-old --compare pod/qwen3-new -n inference

# Measure time to first streamed token
kubectl coldstart pod/qwen3 -n inference --probe

# Stable JSON for CI/dashboards
kubectl coldstart pod/qwen3 -n inference -o json

# Pasteable GitHub issue report
kubectl coldstart pod/qwen3 -n inference -o markdown

# Self-contained interactive HTML
kubectl coldstart pod/qwen3 -n inference -o html > startup.html

# Capture a redacted, replayable bundle
kubectl coldstart pod/qwen3 -n inference -o bundle > qwen3.tgz

# Offline replay; no cluster access required
kubectl coldstart --from-bundle qwen3.tgz -o text
```

Useful flags:

```text
--container <name>          Override engine-container detection
--rules <dir>               Add custom YAML rule packs
--probe-prompt "<text>"     Prompt for the optional first-token probe
-n, --namespace <name>      Kubernetes namespace
-o, --output <format>       text | json | markdown | html | bundle
--kubeconfig <path>         Explicit kubeconfig (otherwise standard loading)
```

## What is accounted

The timeline is anchored at `metadata.creationTimestamp`. Sibling phases never overlap, and every gap is explicitly `unattributed`. The report covers:

- scheduling, node provisioning, and GPU allocatable delay
- image pull and init containers
- container boot and framework bootstrap
- in-engine weights download/load
- `torch.compile`, CUDA graph capture, KV-cache profiling/warmup
- API server startup and readiness lag
- optional first token
- failed attempts and crash loss

For restarts, the current attempt and the available previous termination/log stream are reconstructed separately. Kubernetes retains only the last termination state, so older attempts are reported as unavailable rather than invented.

## Rule packs

Engine log markers live in versioned YAML files under `rules/`, are embedded in the binary, and can be augmented with `--rules`. Packs include:

- engine/version detection
- phase start/end regexes
- optional engine-reported durations
- cache-hit/miss and other flags
- extracted attributes such as model weight bytes
- version-aware recommendation text

If no pack matches the detected version, coldstart uses the nearest same-engine pack and prints a warning. Unknown markers become `unattributed`.

See [docs/rule-packs.md](docs/rule-packs.md) for authoring and validation guidance.

## Bundles and privacy

A bundle is a gzip-compressed tar containing normalized JSON plus readable pod/node/event YAML and startup logs. Before writing, coldstart:

- removes environment variable values and env-from secret sources
- redacts sensitive metadata
- removes credentials from URLs and common token patterns
- trims logs to the startup window

Bundles can still contain operationally sensitive pod specifications and log content. Review them before sharing publicly.

## Development

```sh
make test       # unit and golden tests
make vet        # go vet
make build      # static kubectl-coldstart binary
make fmt        # gofmt
```

The analysis core is a pure function of a normalized bundle. Collectors never call the analysis package, and offline replay never contacts Kubernetes.

### Validation status

Implemented tests cover cold/warm nodes, cache hit/miss, tensor-parallel duration selection, crash loops, expired events, clock skew, missing markers, SGLang, first-token extension, bundle redaction/round-trip, renderers, and comparisons.

The spec's ten **real captured bundles** and measured GPU before/after run remain release-validation tasks. Synthetic fixtures must not be presented as real benchmark evidence.

## Scope

v0.1 deliberately does not patch engines, require sidecars, restart pods, predict unseen configurations, or analyze multi-node Ray/LWS startups. It diagnoses; the operator applies the fix.

## License

Apache-2.0. See [LICENSE](LICENSE).
