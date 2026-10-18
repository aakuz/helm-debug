package cli

import (
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spf13/cobra"
	"helm.sh/helm/v4/pkg/release/common"

	"github.com/aakuz/helm-debug/internal/helm"
)

type stuckRow struct {
	Release   string    `json:"release"`
	Namespace string    `json:"namespace"`
	Revision  int       `json:"revision"`
	Status    string    `json:"status"`
	Updated   time.Time `json:"updated"`
	Fix       string    `json:"fix"`
}

type stuckResult struct {
	Scanned      int        `json:"scanned"`
	DecodeErrors int        `json:"decodeErrors,omitempty"`
	Releases     []stuckRow `json:"releases"`
}

func newStuckCommand(o *Options) *cobra.Command {
	var allNamespaces bool
	var olderThan time.Duration
	cmd := &cobra.Command{
		Use:   "stuck",
		Short: "Find releases stuck in pending-install, pending-upgrade or pending-rollback",
		Long: `Find releases whose latest revision is pending-install, pending-upgrade or
pending-rollback ("another operation (install/upgrade/rollback) is in progress")
and print the command that unlocks each of them. Nothing is changed.

Exits with code 2 when stuck releases are found.`,
		Example: `  helm-debug stuck -A
  helm-debug stuck -n prod --older-than 1h`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			ns := c.Namespace
			if allNamespaces {
				ns = ""
			}
			refs, err := c.Store.ListRefs(cmd.Context(), ns)
			if err != nil {
				return err
			}
			refs = helm.LatestOnly(refs)
			rows := make([]*stuckRow, len(refs))
			failed := make([]bool, len(refs))
			parallel(len(refs), o.Concurrency, func(i int) {
				rel, err := c.Store.Load(refs[i])
				if err != nil {
					slog.Debug("cannot decode release record", "namespace", refs[i].Namespace, "object", refs[i].Object, "error", err)
					failed[i] = true
					return
				}
				if rel.Info == nil || !rel.Info.Status.IsPending() {
					return
				}
				if !rel.Info.LastDeployed.IsZero() && o.Now().Sub(rel.Info.LastDeployed) < olderThan {
					return
				}
				rows[i] = &stuckRow{
					Release: rel.Name, Namespace: rel.Namespace, Revision: rel.Version,
					Status: string(rel.Info.Status), Updated: rel.Info.LastDeployed,
					Fix: unlockCommand(c.Store, rel.Name, rel.Namespace, rel.Version),
				}
			})
			res := stuckResult{Scanned: len(refs), Releases: []stuckRow{}}
			for i := range refs {
				if rows[i] != nil {
					res.Releases = append(res.Releases, *rows[i])
				}
				if failed[i] {
					res.DecodeErrors++
				}
			}
			if err := o.print(res, func(w io.Writer) { printStuck(o, w, res) }); err != nil {
				return err
			}
			if len(res.Releases) > 0 {
				return ErrProblems
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "Look for releases in all namespaces")
	cmd.Flags().DurationVar(&olderThan, "older-than", 5*time.Minute, "Ignore releases that became pending more recently (they may still be running)")
	return cmd
}

// unlockCommand suggests a rollback to the last good revision, or an uninstall when there is none.
func unlockCommand(store *helm.Store, name, namespace string, current int) string {
	if rels, err := store.History(name, namespace); err == nil {
		for _, r := range rels {
			if r.Version < current && r.Info != nil && (r.Info.Status == common.StatusDeployed || r.Info.Status == common.StatusSuperseded) {
				return fmt.Sprintf("helm rollback %s %d -n %s", name, r.Version, namespace)
			}
		}
	}
	return fmt.Sprintf("helm uninstall %s -n %s", name, namespace)
}

func printStuck(o *Options, w io.Writer, res stuckResult) {
	if len(res.Releases) == 0 {
		fmt.Fprintf(w, "No stuck releases among %d scanned\n", res.Scanned)
	} else {
		t := newTable(w)
		t.add("NAMESPACE", "RELEASE", "REVISION", "STATUS", "AGE", "FIX")
		for _, r := range res.Releases {
			age := ""
			if !r.Updated.IsZero() {
				age = formatAge(o.Now().Sub(r.Updated))
			}
			t.add(r.Namespace, r.Release, fmt.Sprint(r.Revision), o.paint(r.Status, statusColor(r.Status)), age, r.Fix)
		}
		t.flush()
	}
	if res.DecodeErrors > 0 {
		fmt.Fprintf(w, "\n%s %d release record(s) could not be decoded\n", o.paint("warning:", yellow), res.DecodeErrors)
	}
}
