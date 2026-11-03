package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

type notesOutput struct {
	Release   string `json:"release"`
	Namespace string `json:"namespace"`
	Revision  int    `json:"revision"`
	Notes     string `json:"notes"`
}

func newNotesCommand(o *Options) *cobra.Command {
	var revision int
	cmd := &cobra.Command{
		Use:   "notes RELEASE",
		Short: "Show the stored NOTES.txt of a release revision",
		Long: `Show the rendered NOTES.txt stored in a release revision, newest revision by default,
like helm get notes. Nothing is changed. Use -o json or -o yaml to print
{release, namespace, revision, notes}.

Notes are chart-controlled text, so terminal escape sequences, control
characters (except newline and tab) and Unicode format characters are stripped in every output format.`,
		Example: `  helm-debug notes backend
  helm-debug notes backend --revision 3 -o json`,
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
			var notes string
			if rel.Info != nil {
				notes = stripUnsafe(rel.Info.Notes)
			}
			out := notesOutput{Release: rel.Name, Namespace: rel.Namespace, Revision: rel.Version, Notes: notes}
			return o.print(out, func(w io.Writer) { fmt.Fprint(w, withNewline(notes)) })
		},
	}
	cmd.Flags().IntVarP(&revision, "revision", "r", 0, "Release revision to inspect (default: latest)")
	return cmd
}
