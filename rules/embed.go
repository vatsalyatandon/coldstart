// Package rulesdata exposes the rule packs shipped in the binary.
package rulesdata

import (
	"embed"
	"io/fs"
)

//go:embed vllm/*.yaml sglang/*.yaml
var embedded embed.FS

// FS returns the embedded rule-pack filesystem rooted at the rule packs.
func FS() fs.FS {
	root, err := fs.Sub(embedded, ".")
	if err != nil {
		panic(err)
	}
	return root
}
