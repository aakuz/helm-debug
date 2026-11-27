package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

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
	Active         int       `json:"active,omitempty"`
	Failed         int       `json:"failed,omitempty"`
	AbsentReason   string    `json:"absentReason,omitempty"`
	Orphan         bool      `json:"orphan"`
	Error          string    `json:"error,omitempty"`
	Logs           []podLog  `json:"logs,omitempty"`
}

// statusRunning marks a hook Job or Pod that has not finished: the one Helm is still waiting on.
const statusRunning = "running"

type hooksResult struct {
	Release   string    `json:"release"`
	Namespace string    `json:"namespace"`
	Revision  int       `json:"revision"`
	Hooks     []hookRow `json:"hooks"`

	Events      []healthEvent `json:"events,omitempty"`
	EventsError string        `json:"eventsError,omitempty"`
}

func newHooksCommand(o *Options) *cobra.Command {
	var revision, logLines int
	cmd := &cobra.Command{
		Use:   "hooks RELEASE",
		Short: "List the hooks of a release revision and find leftover hook Jobs and Pods",
		Long: `List the hooks of a release revision with their events, weight, delete policy and
the phase and time of their last run, and check whether hook Jobs and Pods are
still in the cluster. A hook Job or Pod that has not finished is shown as running, a Job with
its active and failed pod counts. The warning events of the last hour about the hook objects and
their pods are listed, and with --logs N the last N lines of every started container of their pods.
A hook object that is gone gets the reason its delete policy does or does not give for that.
Nothing is changed.

A hook Job or Pod is an orphan when it still exists although its last run
finished and its delete policy (hook-succeeded, hook-failed) asks for removal
after that outcome. Objects kept by policy, or by Helm's default
before-hook-creation, are not orphans.

Exits with code 2 when orphans are found or a hook object cannot be looked up. Running or
failed hooks, warning events and absent objects do not change the exit code.`,
		Example: `  helm-debug hooks backend
  helm-debug hooks backend --logs 20
  helm-debug hooks backend --revision 3 -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if logLines < 0 || logLines > maxLogs {
				return fmt.Errorf("--logs must be between 0 and %d", maxLogs)
			}
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
			pods := make([][]corev1.Pod, len(hooks))
			parallel(len(hooks), o.Concurrency, func(i int) {
				rows[i], pods[i] = inspectHook(cmd, c, rel, hooks[i], logLines)
			})
			res := hooksResult{Release: rel.Name, Namespace: rel.Namespace, Revision: rel.Version, Hooks: rows}
			var live []workloadHealth
			var livePods [][]corev1.Pod
			for i, r := range rows {
				// Events outlive a deleted hook object and explain why it failed, so absent rows are included.
				if r.Status == kube.StatusPresent || r.Status == statusRunning || r.Status == kube.StatusAbsent {
					live = append(live, workloadHealth{Kind: r.Kind, Namespace: r.Namespace, Name: r.Name})
					livePods = append(livePods, pods[i])
				}
			}
			if len(live) > 0 {
				res.Events, res.EventsError = warningEvents(cmd, c, live, livePods, o.Now())
			}
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
	cmd.Flags().IntVar(&logLines, "logs", 0, "Show the last N log lines of every started container of the hook pods (0 = off)")
	return cmd
}

// inspectHook looks up the live hook object and returns its row and its pods, which are the source of
// the warning events and logs.
func inspectHook(cmd *cobra.Command, c *Clients, rel *rspb.Release, h helm.Hook, logLines int) (hookRow, []corev1.Pod) {
	row := hookRow{
		Name: h.Name, Kind: h.Kind, Namespace: h.Namespace, Event: h.HookEvents(),
		Weight: h.Weight, DeletePolicies: h.DeletePolicies, Phase: h.Phase, LastRun: h.LastRun,
	}
	// Only Jobs and Pods are left behind by a finished hook; other kinds are not looked up.
	if h.Kind != "Job" && h.Kind != "Pod" {
		return row, nil
	}
	res, err := c.Cluster.Resolve(h.APIVersion, h.Kind)
	if err != nil {
		row.Status, row.Error = kube.StatusUnknownKind, err.Error()
		return row, nil
	}
	if row.Namespace == "" {
		row.Namespace = rel.Namespace
	}
	live := c.Cluster.Get(cmd.Context(), res, row.Namespace, h.Name)
	row.Status, row.Error = live.Status, live.Error
	row.Orphan = live.Status == kube.StatusPresent && shouldBeDeleted(h)
	if live.Status == kube.StatusAbsent {
		row.AbsentReason = absentReason(h)
	}
	if live.Status != kube.StatusPresent {
		return row, nil
	}

	var pods []corev1.Pod
	running := false
	switch h.Kind {
	case "Job":
		var j batchv1.Job
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(live.Object.Object, &j); err != nil {
			row.Status, row.Error = kube.StatusError, err.Error()
			return row, nil
		}
		row.Active, row.Failed = int(j.Status.Active), int(j.Status.Failed)
		running = !slices.ContainsFunc(j.Status.Conditions, func(cond batchv1.JobCondition) bool {
			return (cond.Type == batchv1.JobComplete || cond.Type == batchv1.JobFailed) && cond.Status == corev1.ConditionTrue
		})
		if j.Spec.Selector != nil {
			var err error
			if pods, err = selectorPods(cmd, c, row.Namespace, j.Spec.Selector); err != nil {
				row.Error = err.Error()
			}
		}
	case "Pod":
		var p corev1.Pod
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(live.Object.Object, &p); err != nil {
			row.Status, row.Error = kube.StatusError, err.Error()
			return row, nil
		}
		running = p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed
		pods = []corev1.Pod{p}
	}
	if running {
		row.Status = statusRunning
	}
	if logLines > 0 && c.Logs != nil {
		var targets []logTarget
		for _, p := range pods {
			targets = append(targets, logTargetsOf(p, nil, false, healthOptions{allLogs: logLines})...)
		}
		row.Logs = readLogs(cmd, c, row.Namespace, targets)
	}
	return row, pods
}

// absentReason says whether the delete policy accounts for a hook object that is gone.
func absentReason(h helm.Hook) string {
	switch {
	case shouldBeDeleted(h):
		if rspb.HookPhase(h.Phase) == rspb.HookPhaseSucceeded {
			return "deleted by its hook-succeeded policy after the hook succeeded"
		}
		return "deleted by its hook-failed policy after the hook failed"
	case h.Phase == "":
		return "the hook has not run"
	case len(h.DeletePolicies) == 0 || slices.Contains(h.DeletePolicies, string(rspb.HookBeforeHookCreation)):
		return "not explained by its delete policy: before-hook-creation deletes it only when the hook runs again; a later run or someone else removed it"
	}
	return "not explained by its delete policy"
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
	t.add("NAME", "KIND", "EVENT", "WEIGHT", "DELETE-POLICY", "PHASE", "LAST RUN", "STATUS", "DETAILS")
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
		details := r.AbsentReason
		if r.Kind == "Job" && (r.Status == kube.StatusPresent || r.Status == statusRunning) {
			details = fmt.Sprintf("active %d, failed %d", r.Active, r.Failed)
		}
		t.add(r.Name, r.Kind, r.Event, fmt.Sprint(r.Weight), strings.Join(r.DeletePolicies, ","), r.Phase, last, status, details)
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
	printWarningEvents(o, w, res.Events, res.EventsError)
	for _, r := range res.Hooks {
		printPodLogs(o, w, r.Logs)
	}
}
