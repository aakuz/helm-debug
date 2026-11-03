package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aakuz/helm-debug/internal/render"
)

type lintResult struct {
	Chart    string           `json:"chart"`
	Findings []render.Finding `json:"findings"`
}

func newLintCommand(o *Options) *cobra.Command {
	var valueFiles, sets []string
	var releaseName string
	cmd := &cobra.Command{
		Use:   "lint CHART",
		Short: "Render a chart locally and show the rendered YAML around every template error",
		Long: `Render CHART (a chart directory or .tgz archive) client-side with the Helm SDK, like
helm template, and report every problem in one run:

  - templates that fail to render (nil pointer, required, fail), as Helm reports them;
  - rendered documents that are not valid YAML, with the rendered lines around the line
    the parser names (helm template --debug shows the output but no line numbers);
  - documents with an empty apiVersion, kind or metadata.name, usually a misspelled value.

The cluster is never contacted: lookup returns an empty result and .Capabilities holds
Helm's defaults, so templates that depend on live objects or the server version may render
differently at install time. Secret data/stringData values in the context lines are masked
unless --show-secrets is set. The release namespace is taken from --namespace (default
"default").

Exits with code 2 when problems are found, 1 when the chart or values cannot be loaded.`,
		Example: `  helm-debug lint ./mychart
  helm-debug lint mychart-1.2.0.tgz -f prod.yaml --set image.tag=1.2 -n prod -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ch, err := render.Load(args[0])
			if err != nil {
				return err
			}
			vals, err := render.Values(valueFiles, sets)
			if err != nil {
				return err
			}
			ns := *o.Flags.Namespace
			if ns == "" {
				ns = "default"
			}
			findings, err := render.Lint(ch, render.Options{ReleaseName: releaseName, Namespace: ns, Values: vals, ShowSecrets: o.ShowSecrets})
			if err != nil {
				return err
			}
			res := lintResult{Chart: ch.Name(), Findings: findings}
			if err := o.print(res, func(w io.Writer) { printLint(o, w, res) }); err != nil {
				return err
			}
			if len(findings) > 0 {
				return ErrProblems
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&valueFiles, "values", "f", nil, "Local values file to render with (can be repeated)")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "Set a value, as in helm template (can be repeated)")
	cmd.Flags().StringVar(&releaseName, "release-name", "release-name", "Release name to render with")
	return cmd
}

func printLint(o *Options, w io.Writer, res lintResult) {
	if len(res.Findings) == 0 {
		fmt.Fprintf(w, "No problems found in chart %s\n", res.Chart)
		return
	}
	for _, f := range res.Findings {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(w, "%s: %s\n", o.paint(loc, name), o.paint(strings.ReplaceAll(f.Error, "\n", "\n  "), red))
		width := 0
		if n := len(f.Context); n > 0 {
			width = len(fmt.Sprint(f.Context[n-1].Number))
		}
		for _, l := range f.Context {
			mark, text := " ", l.Text
			if l.Failing {
				mark, text = ">", o.paint(text, red)
			}
			fmt.Fprintf(w, "%s %*d | %s\n", mark, width, l.Number, text)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "%d problem(s) found in chart %s\n", len(res.Findings), res.Chart)
}
