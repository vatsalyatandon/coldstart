# Rule packs

Rule packs are versioned YAML files that teach coldstart how to recognize an
engine and split its startup logs into phases. They are embedded in the binary
and can be augmented or overridden with `--rules <dir>`.

## File layout

```text
rules/
  vllm/
    ge0.8-lt0.11.yaml
    ge0.11-lt0.40.yaml
  sglang/
    ge0.4.yaml
```

A pack's `versions` value is a
[Masterminds/semver](https://github.com/Masterminds/semver) constraint. The
filename is descriptive; selection uses the constraint, not the filename.

## Schema

```yaml
engine: vllm
versions: ">=0.11,<0.40"
detect:
  - 'vLLM API server version (?P<version>\S+)' # UNVERIFIED: confirm during capture C.
  - 'Initializing a V1 LLM engine' # src: vllm v0.11.0 vllm/v1/engine/core.py

phases:
  weights_load:
    # UNVERIFIED: explicit start marker.
    start: '(?i)Loading weights(?:\.\.\.| \(|$)'
    # src: vllm v0.11.0 vllm/model_executor/model_loader/default_loader.py
    end: 'Loading weights took (?P<secs>[\d.]+) seconds'
    duration_from: secs
  torch_compile:
    # UNVERIFIED: generic Compiling is intentionally not used.
    start: '(?i)torch\.compile(?:\.\.\.| \(|$)'
    # src: vllm v0.11.0 vllm/compilation/monitor.py
    end: 'torch\.compile takes [^0-9]*(?P<secs>[\d.]+)\s*s'
    duration_from: secs
  server_ready:
    # UNVERIFIED: Starting vLLM is detection-only, not readiness.
    end: '(?i)(Application startup complete|Uvicorn running)'

flags:
  # UNVERIFIED: pin to real hit/miss pairs during capture C.
  compile_cache_hit: '(?i)(load the compiled graph|from the cache)'
  compile_cache_miss: '(?i)cache miss'
  # UNVERIFIED: weight-file evidence is required; bare huggingface.co is not enough.
  weights_remote_download: '(?i)(?:download(?:ing|ed)?|fetch(?:ing|ed)?)[^\n]{0,160}(?:\.safetensors|\.bin)\b'

attrs:
  weights_bytes:
    # src: vllm v0.11.0 vllm/v1/worker/gpu_model_runner.py
    pattern: 'Model loading took (?P<value>[\d.]+) GiB'
    multiplier: 1073741824

recommendations:
  compile_cache_miss:
    text: 'Persist the compile cache directory on a shared volume.'
    confidence: high
```

Patterns use Go's RE2 syntax. Look-ahead and backreferences are not supported.
Named capture groups use `(?P<name>...)`.

### Evidence comments

Every shipped regex must have an adjacent comment containing either:

- `# src: <engine> <tag> <source-file>` for a marker verified in tagged source; or
- `# UNVERIFIED: ...` with the assumption that capture C must confirm.

A pattern without evidence is not release-ready. Public issue snippets are
useful leads, but real captured log file and line references are preferred once
available.

### Detection

At least one `detect` expression is required. A named `version` group is
preferred. If logs are unavailable, coldstart falls back to container name,
image, and command metadata. Automatic engine selection only considers main
containers; init-container logs can supply flags such as remote weight download
evidence, but never P4–P11 boundaries.

### Phases

Each phase needs a `start`, an `end`, or both.

- Timestamped lines are sorted before matching; malformed lines with no
  timestamp cannot define a phase boundary.
- The **first** matching start marker wins.
- The **first** matching end marker at or after that start wins. End markers
  before the selected start are ignored.
- If an end rule captures an engine-reported duration and `duration_from`
  names that group, the duration is retained and used to derive the start when
  no explicit start marker exists.
- End-only rules without a duration are point markers, such as
  `server_ready`.
- Repeated tensor-parallel markers follow the same first-start/first-end rule;
  they do not select the largest reported worker duration.
- `duration_from` also accepts a `name2` named group as a convenience for
  alternative end expressions.

### Flags and attributes

Flags are boolean regex matches across the selected container's startup logs.
Attributes extract a named numeric `value` and multiply it, allowing byte
counts to be represented consistently.

### Recommendations

Recommendation text and confidence live in the pack so wording can be corrected
without a code release. The findings engine decides when a flag or observed
threshold warrants showing the recommendation.

## Version selection

1. Use a pack whose constraint matches the detected version.
2. Otherwise use the nearest same-engine pack, preferring the newest pack whose
   lower bound is not newer than the observed version.
3. Print a warning that identifies the missing version and selected pack.
4. Leave unmatched time as `unattributed`.

Custom packs are loaded before embedded packs, so an exact custom constraint
wins. Invalid YAML, unknown fields, invalid constraints, and invalid regexes are
fatal configuration errors.

## Adding a pack

1. Capture real timestamped logs from the target engine version. Do not invent
   marker strings.
2. Record the engine version and provenance in the fixture README.
3. Add start/end markers conservatively. Prefer end markers with explicit
   durations when the engine provides them.
4. Add a golden fixture containing the raw logs/events and expected JSON.
5. Test cold/warm behavior and tensor-parallel interleaving where applicable.
6. Run `go test ./...`; marker regressions must fail CI.

The current repository includes public-log marker tests and synthetic bundle
fixtures. A release candidate still needs the spec's ten real captured bundles
and a measured before/after GPU run.
