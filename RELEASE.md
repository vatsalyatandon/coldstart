# Release checklist

1. Run `go test ./...`, `go vet ./...`, and `make build`.
2. Replace synthetic fixtures with at least ten real captured bundles, including
   cold/warm nodes, cache hit/miss, a crash loop, SGLang, and multiple vLLM
   versions.
3. Perform the managed-cluster, single-GPU before/after run and use the measured
   output as the README headline.
4. Tag `v0.1.0` and publish binaries with GoReleaser.
5. Replace every `REPLACE_WITH_RELEASE_SHA256` in `deploy/krew.yaml` with the
   corresponding archive checksum.
6. Validate the krew manifest with `kubectl krew index add` / the krew manifest
   validator before submitting the plugin.
