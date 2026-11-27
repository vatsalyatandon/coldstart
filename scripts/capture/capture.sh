#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage: capture.sh <deployment> <namespace> <label>

Scales a deployment to zero, waits for deletion, scales it to one, waits for
Ready, and captures a redacted coldstart bundle plus raw Kubernetes evidence.

This mutates cluster state and may consume GPU/cloud capacity. Human execution
only. Set COLDSTART_CAPTURE_CONFIRM=yes to acknowledge that.

Environment:
  COLDSTART_CAPTURE_CONFIRM=yes       Required safety acknowledgement
  COLDSTART_CAPTURE_TIMEOUT=30m      Ready wait timeout
  COLDSTART_CAPTURE_OUT=<path>       Output root (default: scripts/capture/out)
  COLDSTART_CAPTURE_OVERWRITE=yes    Allow replacing an existing label
  COLDSTART_CAPTURE_ALLOW_NOT_READY=yes  Capture crash-loop/pending pods after timeout
EOF
}

if [[ $# -ne 3 ]]; then
  usage
  exit 2
fi

deployment=$1
namespace=$2
label=$3

if [[ ${COLDSTART_CAPTURE_CONFIRM:-} != "yes" ]]; then
  echo "error: COLDSTART_CAPTURE_CONFIRM=yes is required; this script scales a deployment and consumes cluster capacity" >&2
  exit 2
fi
if [[ ! $label =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
  echo "error: label must contain only letters, digits, dot, underscore, or hyphen and cannot start with punctuation" >&2
  exit 2
fi
for command in kubectl jq tar; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "error: required command not found: $command" >&2
    exit 2
  fi
done
if ! kubectl coldstart --help >/dev/null 2>&1; then
  echo "error: kubectl coldstart plugin is not installed or not on PATH" >&2
  exit 2
fi

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
out_root=${COLDSTART_CAPTURE_OUT:-"$script_dir/out"}
out_dir="$out_root/$label"
timeout=${COLDSTART_CAPTURE_TIMEOUT:-30m}

if [[ -e "$out_dir" && ${COLDSTART_CAPTURE_OVERWRITE:-} != "yes" ]]; then
  echo "error: output already exists: $out_dir (set COLDSTART_CAPTURE_OVERWRITE=yes to replace it)" >&2
  exit 2
fi
mkdir -p "$out_dir/raw/logs"
errors_log="$out_dir/capture-errors.log"
: >"$errors_log"

record_error() {
  printf '%s\n' "$*" | tee -a "$errors_log" >&2
}

deployment_json=$(kubectl -n "$namespace" get deployment "$deployment" -o json)
selector=$(jq -r '.spec.selector.matchLabels | to_entries | map("\(.key)=\(.value)") | join(",")' <<<"$deployment_json")
if [[ -z "$selector" ]]; then
  echo "error: deployment has no matchLabels selector; this script requires a simple label selector" >&2
  exit 2
fi
if [[ $(jq '.spec.selector.matchExpressions | length' <<<"$deployment_json") != "0" ]]; then
  echo "error: deployment uses matchExpressions; adapt the capture script to that selector before running" >&2
  exit 2
fi

printf 'Scaling %s/%s to zero\n' "$namespace" "$deployment"
kubectl -n "$namespace" scale deployment "$deployment" --replicas=0
kubectl -n "$namespace" wait --for=jsonpath='{.status.replicas}'=0 deployment/"$deployment" --timeout=5m

# Deployment status can reach zero before every terminating pod disappears.
deadline=$((SECONDS + 300))
while true; do
  remaining=$(kubectl -n "$namespace" get pods -l "$selector" -o name 2>/dev/null || true)
  [[ -z "$remaining" ]] && break
  if (( SECONDS >= deadline )); then
    echo "error: pods for $namespace/$deployment did not disappear within 5m" >&2
    exit 1
  fi
  sleep 2
done

printf 'Scaling %s/%s to one and waiting up to %s\n' "$namespace" "$deployment" "$timeout"
kubectl -n "$namespace" scale deployment "$deployment" --replicas=1
allow_not_ready=${COLDSTART_CAPTURE_ALLOW_NOT_READY:-no}
if ! kubectl -n "$namespace" wait --for=condition=Available deployment/"$deployment" --timeout="$timeout"; then
  if [[ "$allow_not_ready" != "yes" ]]; then
    echo "error: deployment did not become Available; set COLDSTART_CAPTURE_ALLOW_NOT_READY=yes to capture a crash-loop" >&2
    exit 1
  fi
  record_error "deployment did not become Available; continuing because COLDSTART_CAPTURE_ALLOW_NOT_READY=yes"
fi

pod_deadline=$((SECONDS + 300))
while true; do
  pod=$(kubectl -n "$namespace" get pods -l "$selector" --sort-by=.metadata.creationTimestamp -o json | jq -r '.items[-1].metadata.name // empty')
  [[ -n "$pod" ]] && break
  if (( SECONDS >= pod_deadline )); then
    echo "error: no pod appeared for $namespace/$deployment within 5m" >&2
    exit 1
  fi
  sleep 2
done
pod_json=$(kubectl -n "$namespace" get pod "$pod" -o json)
if ! kubectl -n "$namespace" wait --for=condition=Ready pod/"$pod" --timeout="$timeout"; then
  if [[ "$allow_not_ready" != "yes" ]]; then
    echo "error: pod did not become Ready; set COLDSTART_CAPTURE_ALLOW_NOT_READY=yes to capture a crash-loop" >&2
    exit 1
  fi
  record_error "pod did not become Ready; continuing because COLDSTART_CAPTURE_ALLOW_NOT_READY=yes"
fi

printf 'Capturing bundle for %s/%s\n' "$namespace" "$pod"
kubectl coldstart deploy/"$deployment" -n "$namespace" -o bundle >"$out_dir/$label.bundle.tgz"
tar -xOzf "$out_dir/$label.bundle.tgz" bundle.json >"$out_dir/$label.bundle.json"

kubectl -n "$namespace" get pod "$pod" -o json >"$out_dir/raw/$label.pod.json"
kubectl -n "$namespace" get pod "$pod" -o yaml >"$out_dir/raw/$label.pod.yaml"
if ! kubectl -n "$namespace" get events --field-selector="involvedObject.name=$pod" -o yaml >"$out_dir/raw/$label.events.yaml"; then
  record_error "events unavailable for $pod; saved an empty list"
  printf '[]\n' >"$out_dir/raw/$label.events.yaml"
fi

node=$(jq -r '.spec.nodeName // empty' <<<"$pod_json")
if [[ -n "$node" ]]; then
  if ! kubectl get node "$node" -o json >"$out_dir/raw/$label.node.json"; then
    record_error "node $node unavailable"
  fi
  if ! kubectl get node "$node" -o yaml >"$out_dir/raw/$label.node.yaml"; then
    record_error "node YAML unavailable for $node"
  fi
fi

while IFS=$'\t' read -r kind container; do
  [[ -n "$container" ]] || continue
  safe_container=$(printf '%s' "$container" | tr -c 'A-Za-z0-9._-' '-')
  log_file="$out_dir/raw/logs/$label.$kind.$safe_container.log"
  if ! kubectl -n "$namespace" logs --timestamps -c "$container" "$pod" >"$log_file"; then
    record_error "current logs unavailable for $kind container $container"
  fi

  restarts=$(jq -r --arg name "$container" --arg kind "$kind" '
    ((if $kind == "init" then .status.initContainerStatuses else .status.containerStatuses end) // [])
    | map(select(.name == $name)) | .[0].restartCount // 0
  ' <<<"$pod_json")
  if [[ "$restarts" != "0" ]]; then
    previous_file="$out_dir/raw/logs/$label.$kind.$safe_container.previous.log"
    if ! kubectl -n "$namespace" logs --timestamps --previous -c "$container" "$pod" >"$previous_file"; then
      record_error "previous logs unavailable for $kind container $container (restartCount=$restarts)"
    fi
  fi
done < <(
  jq -r '.spec.initContainers[]? | "init\t\(.name)"' <<<"$pod_json"
  jq -r '.spec.containers[]? | "main\t\(.name)"' <<<"$pod_json"
)

engine_image=$(jq -r '[.spec.containers[].image | select(test("vllm|sglang"; "i"))][0] // ""' <<<"$pod_json")
engine_version=""
if [[ -n "$engine_image" ]]; then
  engine_version=${engine_image##*:}
  if [[ "$engine_version" == *"@"* ]]; then
    engine_version=${engine_version#*@}
  fi
fi
image_digest=$(jq -r '[.status.containerStatuses[]? | select((.image // "" | test("vllm|sglang"; "i"))) | .imageID][0] // ""' <<<"$pod_json")
gpu_type=""
node_fresh=null
if [[ -n "$node" && -s "$out_dir/raw/$label.node.json" ]]; then
  gpu_type=$(jq -r '.metadata.labels["nvidia.com/gpu.product"] // .metadata.labels["amd.com/gpu.device-name"] // ""' "$out_dir/raw/$label.node.json")
  node_fresh=$(jq -n --argjson node "$(cat "$out_dir/raw/$label.node.json")" --argjson pod "$pod_json" '
    ($node.metadata.creationTimestamp | fromdateiso8601) > ($pod.metadata.creationTimestamp | fromdateiso8601)
  ')
fi

commands=$(
  printf '%s\n' \
    "kubectl -n $namespace scale deployment $deployment --replicas=0" \
    "kubectl -n $namespace scale deployment $deployment --replicas=1" \
    "kubectl coldstart deploy/$deployment -n $namespace -o bundle" \
    "kubectl -n $namespace logs --timestamps -c <container> $pod" \
    "kubectl -n $namespace logs --timestamps --previous -c <container> $pod" \
    "kubectl -n $namespace get pod $pod -o yaml" \
    "kubectl -n $namespace get events --field-selector=involvedObject.name=$pod -o yaml" \
    "kubectl get node $node -o yaml" |
    jq -R . | jq -s .
)

jq -n \
  --arg label "$label" \
  --arg namespace "$namespace" \
  --arg deployment "$deployment" \
  --arg pod "$pod" \
  --arg node "$node" \
  --arg engine_version "$engine_version" \
  --arg engine_image "$engine_image" \
  --arg image_digest "$image_digest" \
  --arg gpu_type "$gpu_type" \
  --argjson node_fresh "$node_fresh" \
  --arg captured_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson commands "$commands" \
  '{
    label: $label,
    namespace: $namespace,
    deployment: $deployment,
    pod: $pod,
    node: $node,
    engine_version: $engine_version,
    engine_image: $engine_image,
    image_digest: $image_digest,
    gpu_type: $gpu_type,
    node_fresh: $node_fresh,
    captured_at: $captured_at,
    commands: $commands
  }' >"$out_dir/$label.meta.json"

printf 'Capture complete: %s\n' "$out_dir"
printf 'Review raw evidence for secrets before sharing. The bundle is redacted, but raw pod YAML and logs are not.\n'
