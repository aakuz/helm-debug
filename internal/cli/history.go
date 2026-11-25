package cli

import (
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"helm.sh/helm/v4/pkg/release/common"
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
	var allNamespaces bool
	var since time.Duration
	var statuses []string
	cmd := &cobra.Command{
		Use:   "history [RELEASE]",
		Short: "Show release revisions with status, chart version and the reason of failures",
		Long: `With RELEASE, show its revisions oldest first like helm history (the latest is at the bottom), including the
description Helm stored for each revision (it holds the error message for failed operations).

Without RELEASE, show a timeline of the revisions of all releases in the namespace (or in all namespaces with -A), newest first.
Narrow it with --since (revisions updated within the duration), --status (repeatable, any of the given statuses) and --max.
Records are decoded only when they can match, so --since, --status and --max keep large clusters fast.

Exits with code 2 when the latest revision of any release in the output is failed or pending.`,
		Example: `  helm-debug history web
  helm-debug history -A --since 24h
  helm-debug history -A --status failed --status pending-upgrade`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if since < 0 {
					return fmt.Errorf("--since must not be negative")
				}
				for _, s := range statuses {
					if !slices.Contains(helmStatuses, common.Status(s)) {
						return fmt.Errorf("invalid --status %q", s)
					}
				}
			} else if cmd.Flags().Changed("all-namespaces") || cmd.Flags().Changed("since") || cmd.Flags().Changed("status") {
				return fmt.Errorf("--since, --status and -A apply only without RELEASE")
			}
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				ns := c.Namespace
				if allNamespaces {
					ns = ""
				}
				return runTimeline(cmd.Context(), o, c, ns, since, statuses, limit)
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
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "Show the timeline of all namespaces (without RELEASE)")
	cmd.Flags().DurationVar(&since, "since", 0, "Only revisions updated within this duration, e.g. 24h (without RELEASE)")
	cmd.Flags().StringArrayVar(&statuses, "status", nil, "Only revisions with this status; repeat for several (without RELEASE)")
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
