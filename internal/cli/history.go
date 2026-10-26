package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	rspb "helm.sh/helm/v4/pkg/release/v1"
)

type historyRow struct {
	Revision    int       `json:"revision"`
	Updated     time.Time `json:"updated"`
	Status      string    `json:"status"`
	Chart       string    `json:"chart"`
	AppVersion  string    `json:"appVersion,omitempty"`
	Description string    `json:"description,omitempty"`
}

type historyResult struct {
	Release   string       `json:"release"`
	Namespace string       `json:"namespace"`
	Revisions []historyRow `json:"revisions"`
}

func newHistoryCommand(o *Options) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "history RELEASE",
		Short: "Show release revisions with status, chart version and the reason of failures",
		Long: `Show release revisions, oldest first like helm history (the latest is at the bottom), including the description Helm stored
for each revision (it holds the error message for failed operations).

Exits with code 2 when the latest revision is failed or pending.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			rels, err := c.Store.History(args[0], c.Namespace)
			if err != nil {
				return err
			}
			res := historyResult{Release: args[0], Namespace: c.Namespace}
			res.Revisions = historyRows(rels, limit)
			if err := o.print(res, func(w io.Writer) { printHistory(o, w, res) }); err != nil {
				return err
			}
			if latestProblem(rels) {
				return ErrProblems
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "max", 0, "Maximum number of revisions to show (0 = all)")
	return cmd
}

// historyRows takes newest-first rels: --max keeps the latest revisions, which are then returned oldest first like helm history.
func historyRows(rels []*rspb.Release, limit int) []historyRow {
	rows := []historyRow{}
	for i, r := range rels {
		if limit > 0 && i >= limit {
			break
		}
		rows = append([]historyRow{toHistoryRow(r)}, rows...)
	}
	return rows
}

// latestProblem reports whether the newest revision (rels[0]) is failed or pending.
func latestProblem(rels []*rspb.Release) bool {
	st := rels[0].Info.Status
	return st.IsPending() || st == "failed"
}

func toHistoryRow(r *rspb.Release) historyRow {
	h := historyRow{Revision: r.Version}
	if r.Info != nil {
		h.Updated, h.Status, h.Description = r.Info.LastDeployed, string(r.Info.Status), r.Info.Description
	}
	if r.Chart != nil && r.Chart.Metadata != nil {
		h.Chart = r.Chart.Metadata.Name + "-" + r.Chart.Metadata.Version
		h.AppVersion = r.Chart.Metadata.AppVersion
	}
	return h
}

func printHistory(o *Options, w io.Writer, res historyResult) {
	t := newTable(w)
	t.add("REVISION", "UPDATED", "STATUS", "CHART", "APP VERSION", "DESCRIPTION")
	for _, r := range res.Revisions {
		t.add(fmt.Sprint(r.Revision), formatTime(r.Updated), o.paint(r.Status, statusColor(r.Status)), r.Chart, r.AppVersion, r.Description)
	}
	t.flush()
}
