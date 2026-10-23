package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	rspb "helm.sh/helm/v4/pkg/release/v1"

	"github.com/aakuz/helm-debug/internal/helm"
	"github.com/aakuz/helm-debug/internal/kube"
)

type hookRow struct {
	Name           string    `json:"name"`
	Kind           string    `json:"kind"`
	Namespace      string    `json:"namespace,omitempty"`
	Event          string    `json:"event"`
	Weight         int       `json:"weight"`
	DeletePolicies []string  `json:"deletePolicies"`
	Phase          string    `json:"phase,omitempty"`
	LastRun        time.Time `json:"lastRun,omitzero"`
	Status         string    `json:"status,omitempty"`
	Orphan         bool      `json:"orphan"`
	Error          string    `json:"error,omitempty"`
}

type hooksResult struct {
	Release   string    `json:"release"`
	Namespace string    `json:"namespace"`
	Revision  int       `json:"revision"`
	Hooks     []hookRow `json:"hooks"`
}

func newHooksCommand(o *Options) *cobra.Command {
	var revision int
	cmd := &cobra.Command{
		Use:   "hooks RELEASE",
		Short: "List the hooks of a release revision and find leftover hook Jobs and Pods",
		Long: `List the hooks of a release revision with their events, weight, delete policy and
the phase and time of their last run, and check whether hook Jobs and Pods are
still in the cluster. Nothing is changed.

A hook Job or Pod is an orphan when it still exists although its last run
finished and its delete policy (hook-succeeded, hook-failed) asks for removal
after that outcome. Objects kept by policy, or by Helm's default
before-hook-creation, are not orphans.

Exits with code 2 when orphans are found or a hook object cannot be looked up.`,
		Example: `  helm-debug hooks backend
  helm-debug hooks backend --revision 3 -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			rel, err := c.Store.Revision(args[0], c.Namespace, revision)
			if err != nil {
				return err
			}
			hooks := helm.Hooks(rel)
			rows := make([]hookRow, len(hooks))
			parallel(len(hooks), o.Concurrency, func(i int) {
				rows[i] = inspectHook(cmd, c, rel, hooks[i])
			})
			res := hooksResult{Release: rel.Name, Namespace: rel.Namespace, Revision: rel.Version, Hooks: rows}
			if err := o.print(res, func(w io.Writer) { printHooks(o, w, res) }); err != nil {
				return err
			}
			if slices.ContainsFunc(rows, func(r hookRow) bool { return r.Orphan || r.Error != "" }) {
				return ErrProblems
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&revision, "revision", "r", 0, "Release revision to inspect (default: latest)")
	return cmd
}

func inspectHook(cmd *cobra.Command, c *Clients, rel *rspb.Release, h helm.Hook) hookRow {
	row := hookRow{
		Name: h.Name, Kind: h.Kind, Namespace: h.Namespace, Event: h.HookEvents(),
		Weight: h.Weight, DeletePolicies: h.DeletePolicies, Phase: h.Phase, LastRun: h.LastRun,
	}
	// Only Jobs and Pods are left behind by a finished hook; other kinds are not looked up.
	if h.Kind != "Job" && h.Kind != "Pod" {
		return row
	}
	res, err := c.Cluster.Resolve(h.APIVersion, h.Kind)
	if err != nil {
		row.Status, row.Error = kube.StatusUnknownKind, err.Error()
		return row
	}
	if row.Namespace == "" {
		row.Namespace = rel.Namespace
	}
	live := c.Cluster.Get(cmd.Context(), res, row.Namespace, h.Name)
	row.Status, row.Error = live.Status, live.Error
	row.Orphan = live.Status == kube.StatusPresent && shouldBeDeleted(h)
	return row
}

// shouldBeDeleted reports whether the delete policy asks for removal after the hook's last outcome.
func shouldBeDeleted(h helm.Hook) bool {
	switch rspb.HookPhase(h.Phase) {
	case rspb.HookPhaseSucceeded:
		return slices.Contains(h.DeletePolicies, string(rspb.HookSucceeded))
	case rspb.HookPhaseFailed:
		return slices.Contains(h.DeletePolicies, string(rspb.HookFailed))
	}
	return false
}

func printHooks(o *Options, w io.Writer, res hooksResult) {
	fmt.Fprintf(w, "Release %s/%s revision %d\n\n", res.Namespace, res.Release, res.Revision)
	if len(res.Hooks) == 0 {
		fmt.Fprintln(w, "This revision has no hooks")
		return
	}
	t := newTable(w)
	t.add("NAME", "KIND", "EVENT", "WEIGHT", "DELETE-POLICY", "PHASE", "LAST RUN", "STATUS")
	orphans := 0
	for _, r := range res.Hooks {
		last := ""
		if !r.LastRun.IsZero() {
			last = formatAge(o.Now().Sub(r.LastRun)) + " ago"
		}
		status := r.Status
		if r.Orphan {
			orphans++
			status = o.paint("orphan", red)
		}
		t.add(r.Name, r.Kind, r.Event, fmt.Sprint(r.Weight), strings.Join(r.DeletePolicies, ","), r.Phase, last, status)
	}
	t.flush()
	for _, r := range res.Hooks {
		if r.Error != "" {
			fmt.Fprintf(w, "\n%s %s/%s: %s\n", o.paint("error:", red), r.Kind, r.Name, r.Error)
		}
	}
	if orphans > 0 {
		fmt.Fprintf(w, "\n%s %d hook object(s) outlived their delete policy\n", o.paint("warning:", yellow), orphans)
	}
}
