# Real capture sprint (human-run)

This directory contains tooling for Workstream C. **Do not run it from an agent
session.** It scales a Deployment to zero and back to one, waits for a real GPU
pod, and writes raw cluster evidence. A human must approve the cluster, model,
node pool, and cost before running it.

No real captures are checked in. Real bundles belong only in
`testdata/fixtures/real/` after a human captures and reviews them.

## Prerequisites

- `kubectl`, `jq`, and `tar`
- `kubectl coldstart` installed and on `PATH`
- A namespace where scaling the target Deployment is approved
- GPU node pools labeled for the cold/warm variants (see
  [`manifests/README.md`](manifests/README.md))
- Explicit replacement of all `REPLACE_ME` image/model/version placeholders
- Enough quota for the selected model and tensor-parallel size

## Safety and output

The script requires an explicit acknowledgement:

```sh
COLDSTART_CAPTURE_CONFIRM=yes scripts/capture/capture.sh <deployment> <namespace> <label>
```

It refuses to overwrite an existing label unless
`COLDSTART_CAPTURE_OVERWRITE=yes` is also set. The Ready timeout defaults to 30
minutes and can be changed with `COLDSTART_CAPTURE_TIMEOUT`. For the deliberate
OOM/crash-loop run, set `COLDSTART_CAPTURE_ALLOW_NOT_READY=yes` so the script
continues to capture the failed pod after the Ready timeout. On success the
Deployment is left at one replica. If the script is interrupted between scale
steps, restore the desired replica count manually.

Outputs are written under `scripts/capture/out/<label>/`:

```text
<label>.bundle.tgz                 redacted coldstart archive
<label>.bundle.json                canonical bundle.json extracted from the archive
<label>.meta.json                  versions, digests, GPU/node metadata, commands
<label>.pod.yaml                   raw pod spec/status
<label>.events.yaml                raw pod events
<label>.node.yaml                  raw node spec/status
raw/logs/                         timestamped current and previous container logs
capture-errors.log                 non-fatal raw-evidence collection failures
```

The raw pod YAML and logs are **not redacted**. They can contain environment
values, tokens, credentials in URLs, prompts, or other sensitive data. The
coldstart bundle is redacted, but it should still be reviewed before sharing.

## Run matrix

The version slots are intentionally placeholders. Choose three real vLLM
versions and two real SGLang versions during the sprint; do not infer them from
this document.

| # | Engine | Variant | Purpose |
|---:|---|---|---|
| 1 | vLLM old | cold-node | Real markers, image pull, download |
| 2 | vLLM mid | cold-node | Real markers, image pull, download |
| 3 | vLLM new | cold-node | Real markers, image pull, download |
| 4 | vLLM old | warm-node + compile-cache PVC, first run | Compile-cache miss |
| 5 | vLLM mid | warm-node + compile-cache PVC, second run | Compile-cache hit |
| 6 | vLLM new | warm-node + compile-cache PVC, second run | Compile-cache hit |
| 7 | vLLM new | init-download | P3 attribution |
| 8 | vLLM new | oom | Restart/crash finding |
| 9 | SGLang first | cold-node | SGLang markers |
| 10 | SGLang second | warm-node | SGLang markers/cache behavior |
| 11 | vLLM new | warm-node, TP=2 | Interleaved worker logs |
| 12 | vLLM new | istio-sidecar | Engine-container selection |
| 13 | vLLM new, large model | before | Headline baseline |
| 14 | vLLM new, large model | after top recommendation | Headline improvement |

The kustomize run overlays under `manifests/runs/` mirror these slots. Build
one before applying, for example:

```sh
kubectl kustomize scripts/capture/manifests/runs/vllm-new-cold > /tmp/coldstart-run.yaml
# Review image, model, GPU count, node selector, and storage before applying.
kubectl apply -n <namespace> -f /tmp/coldstart-run.yaml
COLDSTART_CAPTURE_CONFIRM=yes scripts/capture/capture.sh <deployment> <namespace> vllm-new-cold
```

For compile-cache miss/hit, apply the warm/cache overlay twice with different
labels while keeping the same PVC. For the deliberate OOM run, add
`COLDSTART_CAPTURE_ALLOW_NOT_READY=yes` so the failed pod is captured after the
Ready timeout. For the headline pair, apply the top finding between run 13 and
run 14 and record the exact manifest/command change in the run notes.

### Cold versus warm nodes

A label selector cannot create a fresh node. The human/platform owner must
provision or label an appropriate node pool before the run:

- `coldstart-node-pool=cold` for a genuinely fresh node and uncached image
- `coldstart-node-pool=warm` for a pre-warmed node/image

Record node creation time, image digest, and whether the image/weights were
already present. If a supposedly cold run lands on an existing node, label it
accordingly rather than presenting it as cold evidence.

## Cost planning (estimate, not a measurement)

Small-model runs commonly need roughly 0.25–0.75 GPU-hours each including image,
weights, startup, and capture. Twelve small runs therefore plan for about 3–9
GPU-hours. The two large-model headline runs can add roughly 2–6 GPU-hours each,
for a planning range of approximately **7–21 GPU-hours**, plus node/idle time.
Actual cost depends on model size, image cache, storage, and provider billing.
Get human/cloud approval before provisioning anything.

## Redaction and promotion

1. Keep `scripts/capture/out/` local and untracked; it is gitignored.
2. Review raw pod YAML and every log for secrets, credentials, customer data,
   prompts, and internal hostnames.
3. Share only the redacted bundle after review. Never commit raw evidence that
   has not been sanitized.
4. A human copies approved bundles to `testdata/fixtures/real/` with provenance
   and expected reports. Agents must not create or fabricate files there.
5. Generate goldens only after reviewing each phase against the raw logs. Every
   real fixture must pass the ±1s accounting gate and have under 10% unattributed
   time; failures are analyzer bugs, not acceptable goldens.

## Fleet alternative

If a team already operates self-hosted vLLM/SGLang, they can run:

```sh
kubectl coldstart pod/<pod> -n <namespace> -o bundle > <label>.bundle.tgz
```

The fleet owner must perform the same redaction review and provide provenance.
No production pod restart is required for this alternative.
