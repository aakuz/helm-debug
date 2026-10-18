package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"helm.sh/helm/v4/pkg/release/common"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/aakuz/helm-debug/internal/helm"
	"github.com/aakuz/helm-debug/internal/kube"
)

// eventWindow is how far back warning events are reported.
const eventWindow = time.Hour

// Workload health statuses (absent and error come from kube).
const (
	healthy   = "healthy"
	unhealthy = "unhealthy"
)

// maxLogs is the largest accepted --logs value.
const maxLogs = 1000

// noLogs are waiting reasons of containers that never started, so they have no log to read.
var noLogs = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
}

// badWaiting are container waiting reasons that mean the pod cannot start or keeps crashing.
var badWaiting = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
}

type podHealth struct {
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Reason    string `json:"reason"`
	Container string `json:"container,omitempty"`
	Restarts  int32  `json:"restarts"`
}

// podLog is the log tail of one container of a pod that needs attention.
type podLog struct {
	Pod       string   `json:"pod"`
	Container string   `json:"container"`
	Previous  bool     `json:"previous,omitempty"`
	Lines     []string `json:"lines,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type workloadHealth struct {
	Kind      string      `json:"kind"`
	Namespace string      `json:"namespace"`
	Name      string      `json:"name"`
	Status    string      `json:"status"`
	Desired   int32       `json:"desired"`
	Ready     int32       `json:"ready"`
	Message   string      `json:"message,omitempty"`
	Pods      []podHealth `json:"pods,omitempty"`
	Logs      []podLog    `json:"logs,omitempty"`
}

type healthEvent struct {
	Object   string    `json:"object"`
	Reason   string    `json:"reason"`
	Message  string    `json:"message"`
	Count    int32     `json:"count"`
	LastSeen time.Time `json:"lastSeen"`
}

type healthResult struct {
	Release     string           `json:"release"`
	Namespace   string           `json:"namespace"`
	Revision    int              `json:"revision"`
	Status      string           `json:"status"`
	Workloads   []workloadHealth `json:"workloads"`
	Events      []healthEvent    `json:"events,omitempty"`
	EventsError string           `json:"eventsError,omitempty"`
}

// releaseProblem reports whether a release status means the release is not (yet) in a good state:
// an operation is in progress, so the workloads may not reflect the new revision, or it failed.
func releaseProblem(status string) bool {
	switch common.Status(status) {
	case common.StatusPendingInstall, common.StatusPendingUpgrade, common.StatusPendingRollback, common.StatusUninstalling, common.StatusFailed:
		return true
	}
	return false
}

func (r *healthResult) problems() bool {
	if releaseProblem(r.Status) {
		return true
	}
	for _, w := range r.Workloads {
		if w.Status != healthy || len(w.Pods) > 0 {
			return true
		}
	}
	return false
}

var workloadGVR = map[string]schema.GroupVersionResource{
	"Deployment":  {Group: "apps", Version: "v1", Resource: "deployments"},
	"StatefulSet": {Group: "apps", Version: "v1", Resource: "statefulsets"},
}

// healthOptions are the flags that shape one run of the health command.
type healthOptions struct {
	revision int
	logLines int  // --logs: lines of log of the pods that need attention; 0 = none
	allLogs  int  // --all-logs: lines of log of every started container of every pod; 0 = none
	tail     bool // with --wait: print only log lines that were not shown before

	timeout, interval time.Duration
}

func newHealthCommand(o *Options) *cobra.Command {
	var revision, logLines, allLogs int
	var wait, tail bool
	var timeout, interval time.Duration
	cmd := &cobra.Command{
		Use:   "health RELEASE",
		Short: "Check readiness of the Deployments and StatefulSets of a release",
		Long: `Check the rollout status of the Deployments and StatefulSets declared by a release
revision, list their pods that crash or cannot pull an image, and show the warning events of
the last hour for these workloads and pods. With --logs N it also shows the last N log lines of
the pods that crash, fail to start or are not ready; --all-logs M shows the last M lines of every
container of every pod. The two are independent and can be combined: pods that need attention get N
lines, every other container M. Nothing is changed.

With --wait (-w) it polls until the release is healthy, printing the state again whenever it changes
(including the log lines when --logs or --all-logs is set), and exits with code 2 if --timeout passes
first. Add --tail to print only the log lines that were not shown before, like tail -f.
With -o json or -o yaml only the final state is printed.

Exits with code 2 when a workload is not fully rolled out or absent, a pod is crashing, or the
release itself is pending, uninstalling or failed.`,
		Example: `  helm-debug health backend -n prod
  helm-debug health backend -w --logs 20
  helm-debug health backend -w --logs 50 --all-logs 5 --tail
  helm-debug health backend -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for name, n := range map[string]int{"logs": logLines, "all-logs": allLogs} {
				if n < 0 || n > maxLogs {
					return fmt.Errorf("--%s must be between 0 and %d", name, maxLogs)
				}
			}
			if tail && !wait {
				return fmt.Errorf("--tail requires --wait")
			}
			if tail && logLines == 0 && allLogs == 0 {
				return fmt.Errorf("--tail requires --logs or --all-logs")
			}
			for _, f := range []string{"timeout", "interval"} {
				if !wait && cmd.Flags().Changed(f) {
					return fmt.Errorf("--%s requires --wait", f)
				}
			}
			if wait && timeout <= 0 {
				return fmt.Errorf("--timeout must be positive")
			}
			if wait && interval <= 0 {
				return fmt.Errorf("--interval must be positive")
			}
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			opts := healthOptions{revision: revision, logLines: logLines, allLogs: allLogs, tail: tail, timeout: timeout, interval: interval}
			if wait {
				return waitHealth(cmd, o, c, args[0], opts)
			}
			res, err := runHealth(cmd, o, c, args[0], opts)
			if err != nil {
				return err
			}
			if err := o.print(res, func(w io.Writer) { printHealth(o, w, res) }); err != nil {
				return err
			}
			if res.problems() {
				return ErrProblems
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&revision, "revision", "r", 0, "Release revision to inspect (default: latest)")
	cmd.Flags().BoolVarP(&wait, "wait", "w", false, "Poll until the release is healthy, printing every change of state")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "With --wait: give up after this long and exit with code 2")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "With --wait: time between polls")
	cmd.Flags().IntVar(&logLines, "logs", 0, "Show the last N log lines of problem and not-ready pods (0 = off)")
	cmd.Flags().IntVar(&allLogs, "all-logs", 0, "Show the last N log lines of every container of every pod (0 = off)")
	cmd.Flags().BoolVar(&tail, "tail", false, "With --wait and --logs/--all-logs: print only log lines not shown before")
	return cmd
}

// waitHealth polls the release until it is healthy or the timeout passes. In table mode it prints the
// state at the first poll and again whenever it changes; in json or yaml mode only the final state.
// With opts.tail, log lines that were already shown are not printed again, and a poll where only the
// logs changed prints just the new lines.
func waitHealth(cmd *cobra.Command, o *Options, c *Clients, name string, opts healthOptions) error {
	start := o.Now()
	deadline := start.Add(opts.timeout)
	table := o.Output == "" || o.Output == "table"
	var seen *logMemory
	if opts.tail {
		seen = newLogMemory()
	}
	var last []byte
	for {
		res, err := runHealth(cmd, o, c, name, opts)
		if err != nil {
			return err
		}
		ok := !res.problems()
		now := o.Now()
		if table {
			sig, fresh := healthSignature(res), res
			if seen != nil {
				sig, fresh = healthSignature(withoutLogs(res)), seen.news(res)
			}
			changed := !bytes.Equal(sig, last)
			if changed || seen != nil && hasLogs(fresh) {
				if last != nil {
					fmt.Fprintln(o.Out)
				}
				state := o.paint(unhealthy, statusColor(unhealthy))
				if ok {
					state = o.paint(healthy, statusColor(healthy))
				}
				fmt.Fprintf(o.Out, "[%s +%s] %s\n", now.Format("15:04:05"), formatAge(now.Sub(start)), state)
				if seen == nil {
					printHealth(o, o.Out, res)
				} else {
					if changed {
						printHealthState(o, o.Out, fresh)
					}
					suffix := ""
					if last != nil {
						suffix = ", new lines"
					}
					printLogs(o, o.Out, fresh, suffix)
				}
				last = sig
			}
		}
		if ok || !now.Before(deadline) {
			if table && !ok {
				fmt.Fprintf(o.Out, "\n[%s +%s] timed out after %s, the release is still unhealthy\n", now.Format("15:04:05"), formatAge(now.Sub(start)), opts.timeout)
			} else if !table {
				if err := o.print(res, nil); err != nil {
					return err
				}
			}
			if ok {
				return nil
			}
			return ErrProblems
		}
		if err := o.Sleep(cmd.Context(), min(opts.interval, deadline.Sub(now))); err != nil {
			return err
		}
	}
}

// healthSignature identifies the state worth printing again. A repeat of a known warning only bumps the
// event's count and time, so events are identified by what they say, not by how often they happened.
func healthSignature(res *healthResult) []byte {
	rest := *res
	rest.Events = nil
	events := make([]string, len(res.Events))
	for i, e := range res.Events {
		events[i] = e.Object + "|" + e.Reason + "|" + e.Message
	}
	sig, _ := json.Marshal(struct {
		State  healthResult
		Events []string
	}{rest, events})
	return sig
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func runHealth(cmd *cobra.Command, o *Options, c *Clients, name string, opts healthOptions) (*healthResult, error) {
	rel, err := c.Store.Revision(name, c.Namespace, opts.revision)
	if err != nil {
		return nil, err
	}
	res := &healthResult{Release: rel.Name, Namespace: rel.Namespace, Revision: rel.Version, Status: string(rel.Info.Status), Workloads: []workloadHealth{}}
	for _, obj := range helm.Objects(rel) {
		if len(obj.Hook) > 0 || obj.APIVersion != "apps/v1" || workloadGVR[obj.Kind].Resource == "" {
			continue
		}
		if obj.Namespace == "" {
			obj.Namespace = rel.Namespace
		}
		res.Workloads = append(res.Workloads, workloadHealth{Kind: obj.Kind, Namespace: obj.Namespace, Name: obj.Name})
	}
	pods := make([][]corev1.Pod, len(res.Workloads))
	parallel(len(res.Workloads), o.Concurrency, func(i int) {
		w := &res.Workloads[i]
		gvr := workloadGVR[w.Kind]
		live := c.Cluster.Get(cmd.Context(), kube.Resolved{GVR: gvr, Kind: w.Kind, Namespaced: true}, w.Namespace, w.Name)
		if live.Status != kube.StatusPresent {
			w.Status, w.Message = live.Status, live.Error
			return
		}
		var selector *metav1.LabelSelector
		switch w.Kind {
		case "Deployment":
			var d appsv1.Deployment
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(live.Object.Object, &d); err != nil {
				w.Status, w.Message = kube.StatusError, err.Error()
				return
			}
			w.Desired, w.Ready, w.Message = deploymentState(&d)
			selector = d.Spec.Selector
		case "StatefulSet":
			var s appsv1.StatefulSet
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(live.Object.Object, &s); err != nil {
				w.Status, w.Message = kube.StatusError, err.Error()
				return
			}
			w.Desired, w.Ready, w.Message = statefulSetState(&s)
			selector = s.Spec.Selector
		}
		w.Status = healthy
		if w.Message != "" {
			w.Status = unhealthy
		}
		if selector == nil {
			return
		}
		sel, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			w.Status, w.Message = kube.StatusError, "invalid selector: "+err.Error()
			return
		}
		items, err := c.Cluster.ListPods(cmd.Context(), w.Namespace, sel.String())
		if err != nil {
			w.Status = kube.StatusError
			w.Message = strings.TrimPrefix(w.Message+"; listing pods: "+err.Error(), "; ")
			return
		}
		pods[i] = podsFrom(items)
		var targets []logTarget
		for _, p := range pods[i] {
			ph := podProblem(p)
			if ph != nil {
				w.Pods = append(w.Pods, *ph)
			}
			targets = append(targets, logTargetsOf(p, ph, w.Status == unhealthy, opts)...)
		}
		if c.Logs != nil {
			w.Logs = readLogs(cmd, c, w.Namespace, targets)
		}
	})

	res.Events, res.EventsError = warningEvents(cmd, c, res.Workloads, pods, o.Now())
	return res, nil
}

func podsFrom(items []unstructured.Unstructured) []corev1.Pod {
	pods := make([]corev1.Pod, 0, len(items))
	for _, it := range items {
		var p corev1.Pod
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(it.Object, &p); err == nil {
			pods = append(pods, p)
		}
	}
	return pods
}

// deploymentState reports desired and ready replicas and why the rollout is incomplete (empty when it is complete).
func deploymentState(d *appsv1.Deployment) (desired, ready int32, msg string) {
	desired = 1
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	st := d.Status
	ready = st.ReadyReplicas
	switch {
	case st.ObservedGeneration < d.Generation:
		msg = fmt.Sprintf("generation %d not yet observed by the controller", d.Generation)
	case progressDeadlineExceeded(st.Conditions) != "":
		msg = progressDeadlineExceeded(st.Conditions)
	case st.UpdatedReplicas < desired:
		msg = fmt.Sprintf("%d of %d updated", st.UpdatedReplicas, desired)
	case st.Replicas > st.UpdatedReplicas:
		msg = fmt.Sprintf("%d old replicas pending termination", st.Replicas-st.UpdatedReplicas)
	case st.AvailableReplicas < desired:
		msg = fmt.Sprintf("%d of %d available", st.AvailableReplicas, desired)
	}
	return desired, ready, msg
}

func progressDeadlineExceeded(conds []appsv1.DeploymentCondition) string {
	for _, c := range conds {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			return strings.TrimSpace("ProgressDeadlineExceeded: " + c.Message)
		}
	}
	return ""
}

// statefulSetState is deploymentState for StatefulSets.
func statefulSetState(s *appsv1.StatefulSet) (desired, ready int32, msg string) {
	desired = 1
	if s.Spec.Replicas != nil {
		desired = *s.Spec.Replicas
	}
	st := s.Status
	ready = st.ReadyReplicas
	wantUpdated := desired
	skipUpdated := s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType
	if ru := s.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil {
		wantUpdated = max(desired-*ru.Partition, 0)
	}
	switch {
	case st.ObservedGeneration < s.Generation:
		msg = fmt.Sprintf("generation %d not yet observed by the controller", s.Generation)
	case st.ReadyReplicas < desired:
		msg = fmt.Sprintf("%d of %d ready", st.ReadyReplicas, desired)
	case !skipUpdated && st.UpdatedReplicas < wantUpdated:
		msg = fmt.Sprintf("%d of %d updated", st.UpdatedReplicas, wantUpdated)
	}
	return desired, ready, msg
}

// podProblem returns the pod's state when it failed or a container cannot start or keeps crashing.
func podProblem(p corev1.Pod) *podHealth {
	ph := &podHealth{Name: p.Name, Phase: string(p.Status.Phase)}
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		ph.Restarts += cs.RestartCount
		if w := cs.State.Waiting; w != nil && badWaiting[w.Reason] && ph.Reason == "" {
			ph.Reason, ph.Container = w.Reason, cs.Name
		}
	}
	if ph.Reason == "" && p.Status.Phase == corev1.PodFailed {
		ph.Reason = p.Status.Reason
		if ph.Reason == "" {
			ph.Reason = "Failed"
		}
	}
	if ph.Reason == "" {
		return nil
	}
	return ph
}

// warningEvents returns recent Warning events about the workloads, their ReplicaSets and their pods.
func warningEvents(cmd *cobra.Command, c *Clients, workloads []workloadHealth, pods [][]corev1.Pod, now time.Time) ([]healthEvent, string) {
	type key struct{ namespace, kind, name string }
	related := map[key]bool{}
	namespaces := map[string]bool{}
	for i, w := range workloads {
		namespaces[w.Namespace] = true
		related[key{w.Namespace, w.Kind, w.Name}] = true
		for _, p := range pods[i] {
			related[key{w.Namespace, "Pod", p.Name}] = true
		}
	}
	isReplicaSet := func(w workloadHealth, name string) bool {
		hash, ok := strings.CutPrefix(name, w.Name+"-")
		return w.Kind == "Deployment" && ok && hash != "" && !strings.Contains(hash, "-")
	}
	var events []healthEvent
	for ns := range namespaces {
		items, err := c.Cluster.ListWarningEvents(cmd.Context(), ns)
		if err != nil {
			return nil, err.Error()
		}
		for _, it := range items {
			var ev corev1.Event
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(it.Object, &ev); err != nil || ev.Type != corev1.EventTypeWarning {
				continue
			}
			inv := ev.InvolvedObject
			match := related[key{ns, inv.Kind, inv.Name}]
			if inv.Kind == "ReplicaSet" {
				for _, w := range workloads {
					match = match || w.Namespace == ns && isReplicaSet(w, inv.Name)
				}
			}
			last := eventTime(ev)
			if !match || now.Sub(last) > eventWindow {
				continue
			}
			events = append(events, healthEvent{Object: cleanLine(inv.Kind + "/" + inv.Name), Reason: cleanLine(ev.Reason), Message: cleanLine(ev.Message), Count: ev.Count, LastSeen: last.UTC()})
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].LastSeen.Equal(events[j].LastSeen) {
			return events[i].LastSeen.After(events[j].LastSeen)
		}
		return events[i].Object < events[j].Object
	})
	return events, ""
}

func eventTime(ev corev1.Event) time.Time {
	switch {
	case !ev.LastTimestamp.IsZero():
		return ev.LastTimestamp.Time
	case ev.Series != nil && !ev.Series.LastObservedTime.IsZero():
		return ev.Series.LastObservedTime.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	}
	return ev.CreationTimestamp.Time
}

// printHealth prints the state of the release followed by the logs of its pods.
func printHealth(o *Options, w io.Writer, res *healthResult) {
	printHealthState(o, w, res)
	printLogs(o, w, res, "")
}

// printHealthState prints the release, its workloads, problem pods and warning events.
func printHealthState(o *Options, w io.Writer, res *healthResult) {
	fmt.Fprintf(w, "Release %s/%s revision %d (%s)\n\n", res.Namespace, res.Release, res.Revision, o.paint(res.Status, statusColor(res.Status)))
	if releaseProblem(res.Status) {
		fmt.Fprintf(w, "%s release is %s, so its workloads may not reflect the latest revision yet\n\n", o.paint("warning:", yellow), res.Status)
	}
	if len(res.Workloads) == 0 {
		fmt.Fprintln(w, "no Deployments or StatefulSets in this release revision")
		return
	}
	t := newTable(w)
	t.add("KIND", "NAMESPACE", "NAME", "READY", "STATUS", "DETAILS")
	for _, wl := range res.Workloads {
		ready := ""
		if wl.Status == healthy || wl.Status == unhealthy {
			ready = fmt.Sprintf("%d/%d", wl.Ready, wl.Desired)
		}
		t.add(wl.Kind, wl.Namespace, wl.Name, ready, o.paint(wl.Status, statusColor(wl.Status)), wl.Message)
	}
	t.flush()

	var pods *table
	for _, wl := range res.Workloads {
		for _, p := range wl.Pods {
			if pods == nil {
				fmt.Fprintln(w, "\nPods with problems:")
				pods = newTable(w)
				pods.add("POD", "PHASE", "REASON", "RESTARTS")
			}
			pods.add(p.Name, p.Phase, o.paint(p.Reason, red), fmt.Sprint(p.Restarts))
		}
	}
	if pods != nil {
		pods.flush()
	}

	if res.EventsError != "" {
		fmt.Fprintf(w, "\n%s could not list events: %s\n", o.paint("warning:", yellow), res.EventsError)
	}
	if len(res.Events) > 0 {
		fmt.Fprintln(w, "\nWarning events (last hour):")
		et := newTable(w)
		et.add("LAST SEEN", "OBJECT", "REASON", "COUNT", "MESSAGE")
		for _, e := range res.Events {
			et.add(formatAge(o.Now().Sub(e.LastSeen)), e.Object, o.paint(e.Reason, yellow), fmt.Sprint(e.Count), e.Message)
		}
		et.flush()
	}
}
