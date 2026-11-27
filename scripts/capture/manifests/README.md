# Capture manifests

Kustomize bases, components, and run overlays for Workstream C1 capture scenarios.
Both engine bases default to the same small Qwen 7B-class model:
`Qwen/Qwen2.5-7B-Instruct`.

**These manifests were only authored and rendered locally. No manifests were
deployed.**

## Layout

- `bases/vllm/` and `bases/sglang/`: single-replica, single-GPU Deployment bases
  using `Recreate` so cache reruns do not briefly require two GPU pods.
- `components/versions/`: three vLLM image slots and two SGLang image slots.
- `components/node/`: engine-independent cold/warm node selection and pull policy.
- `components/vllm/`: compile cache, init download, OOM, Istio injection, and TP2.
- `components/model/`: the large-model placeholder.
- `overlays/runs/`: the 15-entry scenario matrix, including large before/after.
- `overlays/large/base/`: shared large-model configuration, not a capture run.

The bases are not meant to be applied directly; every run overlay selects a
version component. Components require a kustomize release with Component
support (kustomize v5 or a recent `kubectl kustomize`).

## Placeholders to resolve

All unresolved tokens contain `REPLACE_ME`. Do not apply an overlay until its
required values have been replaced.

| Placeholder | Where / meaning |
| --- | --- |
| `REPLACE_ME_VLLM_MID_TAG` | Selected vLLM middle-version image tag |
| `REPLACE_ME_VLLM_NEW_TAG` | Selected vLLM new-version image tag |
| `REPLACE_ME_SGLANG_SLOT1_TAG` | Selected first SGLang image tag |
| `REPLACE_ME_SGLANG_SLOT2_TAG` | Selected second SGLang image tag |
| `REPLACE_ME_MODEL_DOWNLOADER_IMAGE`, `REPLACE_ME_MODEL_DOWNLOADER_TAG` | Init-container image that provides `/bin/sh` and `huggingface-cli` |
| `REPLACE_ME_OVERSIZED_MAX_MODEL_LEN` | Deliberately oversized integer context length for the OOM scenario |
| `REPLACE_ME_HIGH_GPU_MEMORY_UTILIZATION` | Deliberately high vLLM GPU-memory-utilization value for the OOM scenario |
| `REPLACE_ME_LARGE_MODEL_ID` | Concrete large-model Hugging Face repository ID |

The vLLM old slot uses `vllm/vllm-openai:v0.10.1.1`. The mid and new versions
were intentionally left unknown. SGLang uses the official `lmsysorg/sglang`
repository with unknown slot tags. If a private registry or different image
repository is required, replace the complete `image:` value in the corresponding
version component.

`REPLACE_ME_VLLM_VERSION` and `REPLACE_ME_SGLANG_VERSION` in the raw bases are
also placeholders; version components override them in every run overlay.

The OOM and large-model overlays intentionally do not render deployable values.
A human must select GPU-appropriate OOM tuning values or a concrete large model.
The large after overlay is only a post-fix placeholder: it reuses the large
base, and the chosen top fix must be added there before capture.

## Prerequisites

1. Label at least one fresh node pool and one pre-warmed node pool:

   ```sh
   kubectl label nodes <cold-node-names> coldstart.sh/pool=cold --overwrite
   kubectl label nodes <warm-node-names> coldstart.sh/pool=warm --overwrite
   ```

   `cold-node` adds `imagePullPolicy: Always`; `warm-node` uses `IfNotPresent`.
   Pre-pull the selected engine images on the warm pool before measuring it.

2. Nodes must expose GPU capacity through the NVIDIA device plugin resource
   `nvidia.com/gpu`. The bases request one GPU; `vllm-new-tp2-warm` requests two
   GPUs and sets `--tensor-parallel-size 2`.

3. The warm-cache overlays require a default StorageClass, or the PVC in
   `components/vllm/compile-cache-pvc/pvc.yaml` must be adjusted for the target
   cluster. The claim requests 20Gi with `ReadWriteOnce`.

4. The init-download overlay requires a downloader image with
   `huggingface-cli`, network access to Hugging Face, and enough `emptyDir` space
   for the model.

5. The Istio overlay assumes a cluster mesh and injection webhook are already
   configured. It adds only `sidecar.istio.io/inject: "true"` to the pod
   template; it does not set or assume a mesh sidecar image.

No tolerations or namespace are hard-coded. Supply a namespace at apply time,
and add tolerations separately if the selected pools are tainted.

## Run matrix

Paths below are relative to `scripts/capture/manifests/`.

| Overlay | Version slot | Variant / intent |
| --- | --- | --- |
| `overlays/runs/vllm-old-cold` | vLLM `v0.10.1.1` | Cold node |
| `overlays/runs/vllm-mid-cold` | vLLM mid placeholder | Cold node |
| `overlays/runs/vllm-new-cold` | vLLM new placeholder | Cold node |
| `overlays/runs/vllm-new-warm-cache-first` | vLLM new placeholder | Warm node plus compile-cache PVC; expected miss |
| `overlays/runs/vllm-new-warm-cache-second` | vLLM new placeholder | Same warm/cache setup; expected hit |
| `overlays/runs/vllm-new-init-download` | vLLM new placeholder | Cold node plus init download to `emptyDir`; main uses `/models/qwen-7b` |
| `overlays/runs/vllm-new-oom` | vLLM new placeholder | Cold node plus oversized context / high GPU-memory placeholders |
| `overlays/runs/sglang-slot1-cold` | SGLang slot 1 placeholder | Cold node |
| `overlays/runs/sglang-slot2-cold` | SGLang slot 2 placeholder | Cold node |
| `overlays/runs/sglang-slot1-warm` | SGLang slot 1 placeholder | Warm node |
| `overlays/runs/sglang-slot2-warm` | SGLang slot 2 placeholder | Warm node |
| `overlays/runs/vllm-new-tp2-warm` | vLLM new placeholder | Warm node, two GPUs, tensor parallel size 2 |
| `overlays/runs/vllm-new-istio-sidecar` | vLLM new placeholder | Cold node plus injection annotation |
| `overlays/runs/vllm-large-before` | vLLM new placeholder | Cold node plus large-model placeholder |
| `overlays/runs/vllm-large-after` | vLLM new placeholder | Large-model placeholder; human must add the top fix |

`overlays/large/base` holds the shared large-model configuration and is not a
separate capture run.

### Warm compile-cache miss then hit

Apply the first overlay, let the workload become ready and populate
`VLLM_CACHE_ROOT=/root/.cache/vllm`, then apply the second overlay in the **same
namespace**. Do not delete the `compile-cache` PVC between runs. The differing
capture annotations trigger a Deployment rollout while retaining the claim.

```sh
kubectl apply -n <capture-namespace> \
  -k scripts/capture/manifests/overlays/runs/vllm-new-warm-cache-first

# After readiness and cache population:
kubectl apply -n <capture-namespace> \
  -k scripts/capture/manifests/overlays/runs/vllm-new-warm-cache-second
```

The `miss` and `hit` annotations record the expected scenario; they do not
themselves prove whether a cache hit occurred.

## Build and apply

Render locally before applying:

```sh
kustomize build scripts/capture/manifests/overlays/runs/vllm-new-cold
```

`kubectl kustomize <overlay-path>` is an equivalent local renderer when the
standalone kustomize binary is unavailable. It does not contact the cluster.
After resolving placeholders and reviewing the output, apply with:

```sh
kubectl apply -n <capture-namespace> -k <overlay-path>
```

To locate unresolved placeholders before an apply:

```sh
grep -R -n REPLACE_ME scripts/capture/manifests
```
