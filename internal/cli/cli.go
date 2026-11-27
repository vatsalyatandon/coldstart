// Package cli implements the kubectl coldstart command.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vatsalyatandon/coldstart/internal/analyze"
	"github.com/vatsalyatandon/coldstart/internal/bundleio"
	"github.com/vatsalyatandon/coldstart/internal/collect"
	"github.com/vatsalyatandon/coldstart/internal/compare"
	"github.com/vatsalyatandon/coldstart/internal/model"
	"github.com/vatsalyatandon/coldstart/internal/probe"
	"github.com/vatsalyatandon/coldstart/internal/render"
)

const version = "0.1.0-dev"

type options struct {
	kubeconfig   string
	namespace    string
	container    string
	rules        string
	output       string
	fromBundle   string
	compareRef   string
	probeEnabled bool
	probePrompt  string
	maxLogBytes  int64
}

// NewRootCommand returns the kubectl plugin command.
func NewRootCommand() *cobra.Command {
	opts := &options{}
	cmd := &cobra.Command{
		Use:           "coldstart [pod/<name> | deploy/<name>]",
		Short:         "Explain where LLM pod startup time went",
		Long:          "Build an accounted startup timeline for one vLLM or SGLang pod and rank concrete fixes by recoverable seconds.",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context(), cmd, args, opts)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig (otherwise standard loading)")
	flags.StringVarP(&opts.namespace, "namespace", "n", "", "Kubernetes namespace")
	flags.StringVar(&opts.container, "container", "", "engine container name (otherwise auto-detected)")
	flags.StringVar(&opts.rules, "rules", "", "directory containing custom YAML rule packs")
	flags.StringVarP(&opts.output, "output", "o", render.FormatText, "output format: text|json|markdown|html|bundle")
	flags.StringVar(&opts.fromBundle, "from-bundle", "", "analyze a captured bundle instead of contacting Kubernetes")
	flags.StringVar(&opts.compareRef, "compare", "", "compare against another pod or deployment reference")
	flags.BoolVar(&opts.probeEnabled, "probe", false, "measure first streamed token via port-forward")
	flags.StringVar(&opts.probePrompt, "probe-prompt", "Hello", "prompt for the optional first-token probe")
	flags.Int64Var(&opts.maxLogBytes, "max-log-bytes", collect.DefaultMaxLogBytes, "maximum bytes read per current or previous container log stream")
	return cmd
}

func run(ctx context.Context, cmd *cobra.Command, args []string, opts *options) error {
	opts.output = strings.ToLower(strings.TrimSpace(opts.output))
	if err := validateOptions(args, opts); err != nil {
		return err
	}

	if opts.fromBundle != "" {
		bundle, err := readBundle(cmd.InOrStdin(), opts.fromBundle)
		if err != nil {
			return err
		}
		if opts.output == "bundle" {
			return bundleio.Write(cmd.OutOrStdout(), bundle)
		}
		report, err := analyze.Analyze(bundle, analyze.Options{RuleDir: opts.rules, Container: opts.container})
		if err != nil {
			return err
		}
		return render.Render(ctx, cmd.OutOrStdout(), report, opts.output)
	}

	if opts.output == "bundle" {
		bundle, err := collectBundle(ctx, args[0], opts)
		if err != nil {
			return err
		}
		return bundleio.Write(cmd.OutOrStdout(), bundle)
	}

	before, err := collectBundle(ctx, args[0], opts)
	if err != nil {
		return err
	}
	beforeReport, err := analyze.Analyze(before, analyze.Options{RuleDir: opts.rules, Container: opts.container})
	if err != nil {
		return err
	}
	if opts.compareRef == "" {
		return render.Render(ctx, cmd.OutOrStdout(), beforeReport, opts.output)
	}

	after, err := collectBundle(ctx, opts.compareRef, opts)
	if err != nil {
		return err
	}
	afterReport, err := analyze.Analyze(after, analyze.Options{RuleDir: opts.rules, Container: opts.container})
	if err != nil {
		return err
	}
	return compare.Render(ctx, cmd.OutOrStdout(), compare.Build(beforeReport, afterReport), opts.output)
}

func validateOptions(args []string, opts *options) error {
	output := strings.ToLower(strings.TrimSpace(opts.output))
	switch output {
	case "text", "json", "markdown", "html", "bundle":
	default:
		return fmt.Errorf("unsupported output format %q (want text, json, markdown, html, or bundle)", opts.output)
	}
	if opts.maxLogBytes < 0 {
		return fmt.Errorf("--max-log-bytes must be non-negative")
	}
	if opts.fromBundle != "" {
		if len(args) != 0 {
			return fmt.Errorf("--from-bundle cannot be combined with a resource argument")
		}
		if opts.compareRef != "" {
			return fmt.Errorf("--compare requires live resource references and cannot be combined with --from-bundle")
		}
		if opts.probeEnabled {
			return fmt.Errorf("--probe cannot be used with --from-bundle; replay is offline")
		}
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("a pod/<name> or deploy/<name> argument is required (or use --from-bundle)")
	}
	if _, err := collect.ParseResource(args[0]); err != nil {
		return err
	}
	if opts.compareRef != "" {
		if _, err := collect.ParseResource(opts.compareRef); err != nil {
			return fmt.Errorf("invalid --compare reference: %w", err)
		}
		if output == "bundle" {
			return fmt.Errorf("--compare cannot be combined with -o bundle")
		}
		if output == "html" {
			return fmt.Errorf("--compare supports text, markdown, or json output")
		}
	}
	return nil
}

func collectBundle(ctx context.Context, resource string, opts *options) (model.Bundle, error) {
	bundle, err := collect.Collect(ctx, collect.Options{
		Kubeconfig:      opts.kubeconfig,
		Namespace:       opts.namespace,
		Resource:        resource,
		Container:       opts.container,
		IncludePrevious: true,
		MaxLogBytes:     opts.maxLogBytes,
	})
	if err != nil {
		return model.Bundle{}, err
	}
	if !opts.probeEnabled {
		return bundle, nil
	}

	result, probeErr := probe.Run(ctx, probe.Options{
		Kubeconfig: opts.kubeconfig,
		Namespace:  bundle.Pod.Namespace,
		Pod:        bundle.Pod.Name,
		Container:  opts.container,
		Prompt:     opts.probePrompt,
	})
	bundle.Probe = &result
	if probeErr != nil {
		bundle.Notes = append(bundle.Notes, fmt.Sprintf("first-token probe failed: %v", probeErr))
	}
	return bundle, nil
}

func readBundle(stdin io.Reader, name string) (model.Bundle, error) {
	var reader io.Reader
	var file *os.File
	if name == "-" {
		reader = stdin
	} else {
		opened, err := os.Open(name) // #nosec G304 -- bundle paths are explicit user input.
		if err != nil {
			return model.Bundle{}, fmt.Errorf("open bundle %s: %w", name, err)
		}
		file = opened
		reader = file
	}
	if file != nil {
		defer func() {
			// ReadBundle has finished before close; cleanup errors do not change
			// the parsed bundle.
			_ = file.Close()
		}()
	}
	bundle, err := bundleio.Read(reader)
	if err != nil {
		return model.Bundle{}, fmt.Errorf("read bundle %s: %w", name, err)
	}
	return bundle, nil
}
