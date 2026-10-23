package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"helm.sh/helm/v4/pkg/chart/common/util"
)

func newValuesCommand(o *Options) *cobra.Command {
	var revision int
	var all bool
	var outputFile string
	cmd := &cobra.Command{
		Use:   "values RELEASE",
		Short: "Show user-supplied or computed values of a release revision",
		Long: `Show the values stored in a release revision, newest revision by default.

By default only the user-supplied values (release.config) are printed as plain YAML, ready for
helm upgrade -f or helm template -f. With --all the chart defaults merged with the user values
are printed, like helm get values --all. Use -o json for JSON.

Values may contain secrets: a warning is printed to stderr and values are never logged.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if revision < 0 {
				return fmt.Errorf("invalid revision %d: must not be negative", revision)
			}
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			rel, err := c.Store.Revision(args[0], c.Namespace, revision)
			if err != nil {
				return err
			}
			vals := rel.Config
			if all && rel.Chart != nil {
				if vals, err = util.CoalesceValues(rel.Chart, rel.Config); err != nil {
					return err
				}
			}
			if vals == nil {
				vals = map[string]any{}
			}
			b, err := encodeValues(o.Output, vals)
			if err != nil {
				return err
			}
			fmt.Fprintln(o.ErrOut, "Warning: values may contain secrets; handle the output with care")
			if outputFile != "" {
				return writePrivateFile(outputFile, b)
			}
			_, err = o.Out.Write(b)
			return err
		},
	}
	cmd.Flags().IntVarP(&revision, "revision", "r", 0, "Release revision to inspect (default: latest)")
	cmd.Flags().BoolVar(&all, "all", false, "Show computed values: chart defaults merged with user-supplied values")
	cmd.Flags().StringVar(&outputFile, "output-file", "", "Write values to this file (mode 0600) instead of stdout")
	return cmd
}

// encodeValues renders JSON for -o json and plain YAML otherwise, so the default output is a usable values file.
func encodeValues(format string, vals map[string]any) ([]byte, error) {
	if format == "json" {
		b, err := json.MarshalIndent(vals, "", "  ")
		return append(b, '\n'), err
	}
	return yaml.Marshal(vals)
}

// writePrivateFile enforces 0600 even when the file already exists, since os.WriteFile only applies the mode on creation.
func writePrivateFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
