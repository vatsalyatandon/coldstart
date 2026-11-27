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
  - 'vLLM API server version (?P<version>\S+)'
  - 'Initializing a V1 LLM engine'

phases:
  weights_load:
    start: '(?i)Loading weights(?:\.\.\.| \(|$)'
    end: 'Loading weights took (?P<secs>[\d.]+) seconds'
    duration_from: secs
  torch_compile:
    start: '(?i)Compiling'
    end: 'torch\.compile takes [^0-9]*(?P<secs>[\d.]+)\s*s'
    duration_from: secs
  server_ready:
    end: '(?i)Application startup complete'

flags:
  compile_cache_hit: '(?i)load the compiled graph|from the cache'
  compile_cache_miss: '(?i)cache miss'
  weights_remote_download: '(?i)huggingface\.co|hf_hub_download'

attrs:
  weights_bytes:
    pattern: 'Model loading took (?P<value>[\d.]+) GiB'
    multiplier: 1073741824

recommendations:
  compile_cache_miss:
    text: 'Persist the compile cache directory on a shared volume.'
    confidence: high
```

Patterns use Go's RE2 syntax. Look-ahead and backreferences are not supported.
Named capture groups use `(?P<name>...)`.

### Detection

At least one `detect` expression is required. A named `version` group is
preferred. If logs are unavailable, coldstart falls back to container name,
image, and command metadata.

### Phases

Each phase needs a `start`, an `end`, or both.

- If a start marker exists, its timestamp is used.
- If an end rule captures an engine-reported duration and `duration_from`
  names that group, the duration is retained and used to derive the start when
  no explicit start marker exists.
- End-only rules without a duration are point markers, such as
  `server_ready`.
- When several tensor-parallel workers report the same phase, the largest
  reported duration wins.
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
