package cli

import (
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"

	"github.com/aakuz/helm-debug/internal/helm"
)

type listRow struct {
	Name       string    `json:"name"`
	Namespace  string    `json:"namespace"`
	Revision   int       `json:"revision"`
	Updated    time.Time `json:"updated"`
	Status     string    `json:"status"`
	Chart      string    `json:"chart"`
	AppVersion string    `json:"appVersion,omitempty"`
}

type listResult struct {
	DecodeErrors int       `json:"decodeErrors,omitempty"`
	Releases     []listRow `json:"releases"`
}

func newListCommand(o *Options) *cobra.Command {
	var allNamespaces, all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List releases like helm list, reading the records straight from the cluster",
		Long: `List releases with the latest revision, status and chart, sorted by name.

Like helm list, only deployed and failed releases are shown by default; use --all to also
see superseded, uninstalled and pending ones (a release stuck in a pending state is only
visible with --all).`,
		Example: `  helm-debug list -A
  helm-debug list -n prod --all`,
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
			rels := make([]*rspb.Release, len(refs))
			parallel(len(refs), o.Concurrency, func(i int) {
				rel, err := c.Store.Load(refs[i])
				if err != nil {
					slog.Debug("cannot decode release record", "namespace", refs[i].Namespace, "object", refs[i].Object, "error", err)
					return
				}
				rels[i] = rel
			})
			res := listResult{Releases: []listRow{}}
			for _, rel := range rels {
				switch {
				case rel == nil:
					res.DecodeErrors++
				case all || showByDefault(rel):
					res.Releases = append(res.Releases, toListRow(rel))
				}
			}
			sort.SliceStable(res.Releases, func(i, j int) bool {
				a, b := res.Releases[i], res.Releases[j]
				if a.Name != b.Name {
					return a.Name < b.Name
				}
				return a.Namespace < b.Namespace
			})
			return o.print(res, func(w io.Writer) { printList(o, w, res) })
		},
	}
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "List releases in all namespaces")
	cmd.Flags().BoolVar(&all, "all", false, "Show releases of every status, not only deployed and failed")
	return cmd
}

// showByDefault mirrors helm list, which shows only deployed and failed releases unless --all is set.
func showByDefault(rel *rspb.Release) bool {
	return rel.Info != nil && (rel.Info.Status == common.StatusDeployed || rel.Info.Status == common.StatusFailed)
}

func toListRow(rel *rspb.Release) listRow {
	row := listRow{Name: rel.Name, Namespace: rel.Namespace, Revision: rel.Version}
	if rel.Info != nil {
		row.Updated, row.Status = rel.Info.LastDeployed, string(rel.Info.Status)
	}
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		row.Chart = rel.Chart.Metadata.Name + "-" + rel.Chart.Metadata.Version
		row.AppVersion = rel.Chart.Metadata.AppVersion
	}
	return row
}

func printList(o *Options, w io.Writer, res listResult) {
	t := newTable(w)
	t.add("NAME", "NAMESPACE", "REVISION", "UPDATED", "STATUS", "CHART", "APP VERSION")
	for _, r := range res.Releases {
		t.add(r.Name, r.Namespace, fmt.Sprint(r.Revision), formatTime(r.Updated), o.paint(r.Status, statusColor(r.Status)), r.Chart, r.AppVersion)
	}
	t.flush()
}
