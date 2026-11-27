package main

import (
	"fmt"
	"os"

	"github.com/vatsalyatandon/coldstart/internal/cli"
)

func main() {
	if err := cli.NewRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
