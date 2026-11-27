# Golden fixtures

This directory contains ten deterministic bundle/report pairs used by
`internal/analyze/golden_test.go`.

**These are synthetic fixtures, not real cluster captures.** Kubernetes object
timestamps and events are constructed to exercise the analyzer; several log
markers are based on publicly visible vLLM/SGLang output. They must not be used
as benchmark evidence or represented as the spec's real captured corpus.

Cases:

- `cold-vllm-cache-miss`: cold node, image pull, init download, compile miss
- `warm-vllm-cache-hit`: warm node and compile-cache hit
- `crash-loop`: previous OOMKilled attempt and current retry
- `expired-events`: durable status with events expired
- `clock-skew`: log/container timestamps before pod creation
- `sglang`: SGLang load, graph capture, and server-ready markers
- `vllm-legacy`: vLLM 0.10 rule selection
- `tensor-parallel`: slowest worker duration wins
- `missing-markers`: no logs, explicit unattributed time
- `first-token-probe`: optional probe extends the timeline

Regenerate only after reviewing an intentional analyzer/rule change:

```sh
UPDATE_GOLDEN=1 go test ./internal/analyze
go test ./...
```

Before tagging v0.1, replace or supplement these with at least ten real,
redacted captured bundles and a measured single-GPU before/after pair.
