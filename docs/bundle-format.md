# Bundle format v1

A coldstart bundle is a gzip-compressed tar archive. `bundle.json` is the
canonical replay input; the other members are included for human inspection.

```text
bundle.json
pod.yaml
node.yaml              # when a node was collected
events.yaml
logs/
  engine.log
  engine-previous.log # when previous-attempt logs were collected
```

`bundle.json` conforms to `model.Bundle` and has `"schema":
"coldstart/bundle/v1"`. It contains a normalized Kubernetes pod, optional node,
events, timestamped container log lines, optional GPU-operator pods, and an
optional first-token probe result.

## Replay

```sh
kubectl coldstart --from-bundle bundle.tgz -o json
```

Replay loads the same rule packs and performs no network calls. A bundle created
with one coldstart release may produce different output with a newer release if
rule packs or findings rules improve; the report schema remains
`coldstart/v1`.

## Safety and limits

Bundle writing deep-copies and redacts the input. Environment values and
env-from secret sources are removed, sensitive metadata is redacted, URL
credentials and common token patterns are scrubbed, and logs are trimmed to the
startup window.

Bundle reading rejects:

- gzip/tar corruption
- absolute or traversing paths
- links and special files
- duplicate members
- archives over 32 MiB compressed or 64 MiB uncompressed
- more than 256 archive entries

Bundles may still contain sensitive operational data. Review before sharing.
