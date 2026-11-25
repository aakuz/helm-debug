package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"helm.sh/helm/v4/pkg/release/common"
	releaseutil "helm.sh/helm/v4/pkg/release/v1/util"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"

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

// maxLogs is the largest accepted --logs or --all-logs value.
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

	// id and restarts tell one run of a container from another; more marks lines that continue a log
	// already shown (--tail). None of them is part of the output.
	id       string
	restarts int32
	more     bool
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

	gvr schema.GroupVersionResource // where to read the live object; not part of the output
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

	// WaitingFor are the objects (Kind/Name) that are not healthy, as helm --wait would still be waiting for them.
	WaitingFor []string `json:"waitingFor,omitempty"`
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

// The kinds and readiness rules below mirror the ReadyChecker of helm --wait:
// https://github.com/helm/helm/blob/v4.3.1/pkg/kube/ready.go
// Keep them in step with it, so that "healthy" here means "helm --wait would stop waiting".
// Helm 4 uses it with --wait=legacy; the default --wait=watcher judges readiness with kstatus instead.
type kindKey struct{ apiVersion, kind string }

var workloadGVR = map[kindKey]schema.GroupVersionResource{
	{"apps/v1", "Deployment"}:       {Group: "apps", Version: "v1", Resource: "deployments"},
	{"apps/v1", "StatefulSet"}:      {Group: "apps", Version: "v1", Resource: "statefulsets"},
	{"apps/v1", "DaemonSet"}:        {Group: "apps", Version: "v1", Resource: "daemonsets"},
	{"batch/v1", "Job"}:             {Group: "batch", Version: "v1", Resource: "jobs"},
	{"v1", "PersistentVolumeClaim"}: {Version: "v1", Resource: "persistentvolumeclaims"},
	{"v1", "Service"}:               {Version: "v1", Resource: "services"},
	{"v1", "Pod"}:                   {Version: "v1", Resource: "pods"},
}

// waitsFor reports whether helm --wait waits for the object. A Service is judged by its manifest type
// because the live one is only read to check it.
func waitsFor(obj helm.Object, opts healthOptions) bool {
	switch {
	case len(obj.Hook) > 0, workloadGVR[kindKey{obj.APIVersion, obj.Kind}].Resource == "":
		return false
	case obj.Kind == "Job":
		return opts.waitForJobs
	case obj.Kind == "Service":
		return obj.ServiceType == string(corev1.ServiceTypeLoadBalancer)
	}
	return true
}

// healthOptions are the flags that shape one run of the health command.
type healthOptions struct {
	revision    int
	logLines    int  // --logs: lines of log of the pods that need attention; 0 = none
	allLogs     int  // --all-logs: lines of log of every started container of every pod; 0 = none
	tail        bool // with --wait: print only log lines that were not shown before
	waitForJobs bool // --wait-for-jobs: also check Jobs

	timeout, interval time.Duration
}

func newHealthCommand(o *Options) *cobra.Command {
	var revision, logLines, allLogs int
	var wait, tail, waitForJobs bool
	var timeout, interval time.Duration
	cmd := &cobra.Command{
		Use:   "health RELEASE",
		Short: "Check readiness of the objects of a release that helm --wait waits for",
		Long: `Check the readiness of the objects declared by a release revision that helm --wait waits for:
Deployments, StatefulSets, DaemonSets, Pods, PersistentVolumeClaims and Services of type LoadBalancer,
and with --wait-for-jobs also Jobs. The rules are those of helm --wait. The objects that are not healthy are
named on the WAITING FOR line. It lists the pods of the workloads that crash or cannot pull an image, and
shows the warning events of the last hour for these objects and pods. With --logs N it also shows the last N log lines of
the pods that crash, fail to start or are not ready; --all-logs M shows the last M lines of every
container of every pod. The two are independent and can be combined: pods that need attention get N
lines, every other container M. Nothing is changed.

With --wait (-w) it polls until the release is healthy, printing the state again whenever it changes
(including the log lines when --logs or --all-logs is set), and exits with code 2 if --timeout passes
first. Add --tail to print only the log lines that were not shown before, like tail -f.
With -o json or -o yaml only the final state is printed.

Exits with code 2 when an object is not ready or absent, a pod is crashing, or the
release itself is pending, uninstalling or failed.`,
		Example: `  helm-debug health backend -n prod
  helm-debug health backend -w --logs 20
  helm-debug health backend -w --logs 50 --all-logs 5 --tail
  helm-debug health backend -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, f := range []struct {
				name string
				n    int
			}{{"logs", logLines}, {"all-logs", allLogs}} {
				if f.n < 0 || f.n > maxLogs {
					return fmt.Errorf("--%s must be between 0 and %d", f.name, maxLogs)
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
			opts := healthOptions{revision: revision, logLines: logLines, allLogs: allLogs, tail: tail, waitForJobs: waitForJobs, timeout: timeout, interval: interval}
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
	cmd.Flags().IntVar(&allLogs, "all-logs", 0, "Show the last N log lines of every started container of every pod; --logs sets N for the pods that need attention (0 = off)")
	cmd.Flags().BoolVar(&waitForJobs, "wait-for-jobs", false, "Also check that Jobs complete, as helm --wait-for-jobs does")
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
					printLogs(o, o.Out, fresh)
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
		if !waitsFor(obj, opts) {
			continue
		}
		if obj.Namespace == "" {
			obj.Namespace = rel.Namespace
		}
		res.Workloads = append(res.Workloads, workloadHealth{Kind: obj.Kind, Namespace: obj.Namespace, Name: obj.Name, gvr: workloadGVR[kindKey{obj.APIVersion, obj.Kind}]})
	}
	// Helm waits for the objects in install order, so the table and WAITING FOR follow it too.
	sort.SliceStable(res.Workloads, func(i, j int) bool {
		return installRank(res.Workloads[i].Kind) < installRank(res.Workloads[j].Kind)
	})
	pods := make([][]corev1.Pod, len(res.Workloads))
	parallel(len(res.Workloads), o.Concurrency, func(i int) {
		w := &res.Workloads[i]
		live := c.Cluster.Get(cmd.Context(), kube.Resolved{GVR: w.gvr, Kind: w.Kind, Namespaced: true}, w.Namespace, w.Name)
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
		case "DaemonSet":
			var d appsv1.DaemonSet
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(live.Object.Object, &d); err != nil {
				w.Status, w.Message = kube.StatusError, err.Error()
				return
			}
			w.Desired, w.Ready, w.Message = daemonSetState(&d)
			selector = d.Spec.Selector
		case "Job":
			var j batchv1.Job
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(live.Object.Object, &j); err != nil {
				w.Status, w.Message = kube.StatusError, err.Error()
				return
			}
			w.Desired, w.Ready, w.Message = jobState(&j)
			selector = j.Spec.Selector
		case "PersistentVolumeClaim":
			var pvc corev1.PersistentVolumeClaim
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(live.Object.Object, &pvc); err != nil {
				w.Status, w.Message = kube.StatusError, err.Error()
				return
			}
			w.Message = pvcState(&pvc)
		case "Service":
			var svc corev1.Service
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(live.Object.Object, &svc); err != nil {
				w.Status, w.Message = kube.StatusError, err.Error()
				return
			}
			w.Message = serviceState(&svc)
		case "Pod":
			var p corev1.Pod
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(live.Object.Object, &p); err != nil {
				w.Status, w.Message = kube.StatusError, err.Error()
				return
			}
			w.Desired, w.Ready, w.Message = podState(&p)
			pods[i] = []corev1.Pod{p}
		}
		w.Status = healthy
		if w.Message != "" {
			w.Status = unhealthy
		}
		if w.Kind != "Pod" {
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
		}
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

	for _, w := range res.Workloads {
		if w.Status != healthy {
			res.WaitingFor = append(res.WaitingFor, w.Kind+"/"+w.Name)
		}
	}
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

// daemonSetState is deploymentState for DaemonSets, with Helm's rules rather than kubectl's.
func daemonSetState(d *appsv1.DaemonSet) (desired, ready int32, msg string) {
	st := d.Status
	desired, ready = st.DesiredNumberScheduled, st.NumberReady
	if st.ObservedGeneration != d.Generation {
		return desired, ready, fmt.Sprintf("generation %d not yet observed by the controller", d.Generation)
	}
	if d.Spec.UpdateStrategy.Type != appsv1.RollingUpdateDaemonSetStrategyType {
		return desired, ready, ""
	}
	if st.UpdatedNumberScheduled != desired {
		return desired, ready, fmt.Sprintf("%d of %d updated", st.UpdatedNumberScheduled, desired)
	}
	// Like Helm, tolerate every pod being unavailable when maxUnavailable is missing or invalid.
	maxUnavailable := int(desired)
	if ru := d.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.MaxUnavailable != nil {
		if n, err := intstr.GetScaledValueFromIntOrPercent(ru.MaxUnavailable, int(desired), true); err == nil {
			maxUnavailable = n
		}
	}
	if int(ready) < int(desired)-maxUnavailable {
		msg = fmt.Sprintf("%d of %d ready", ready, desired)
	}
	return desired, ready, msg
}

// jobState is deploymentState for Jobs: desired is the completions and ready the succeeded pods.
func jobState(j *batchv1.Job) (desired, ready int32, msg string) {
	desired, ready = 1, j.Status.Succeeded
	if j.Spec.Completions != nil {
		desired = *j.Spec.Completions
	}
	backoffLimit := int32(6)
	if j.Spec.BackoffLimit != nil {
		backoffLimit = *j.Spec.BackoffLimit
	}
	switch {
	case j.Status.Failed > backoffLimit:
		msg = fmt.Sprintf("failed: %d failures exceed backoffLimit %d", j.Status.Failed, backoffLimit)
	case j.Spec.Completions != nil && j.Status.Succeeded < desired:
		msg = fmt.Sprintf("%d of %d succeeded", j.Status.Succeeded, desired)
	}
	return desired, ready, msg
}

// pvcState names the phase, because Pending (no provisioner, missing StorageClass) and Lost need different fixes.
func pvcState(p *corev1.PersistentVolumeClaim) string {
	switch p.Status.Phase {
	case corev1.ClaimBound:
		return ""
	case "":
		return "phase unknown"
	}
	return "phase " + string(p.Status.Phase)
}

// serviceState checks a LoadBalancer Service; other types are never listed.
func serviceState(s *corev1.Service) string {
	switch {
	case s.Spec.ClusterIP == "":
		return "no cluster IP"
	case len(s.Spec.ExternalIPs) > 0:
		return ""
	case len(s.Status.LoadBalancer.Ingress) == 0:
		return "no load balancer address yet"
	}
	return ""
}

// podState is deploymentState for a bare Pod, which counts as one replica.
func podState(p *corev1.Pod) (desired, ready int32, msg string) {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return 1, 1, ""
		}
	}
	return 1, 0, "not ready"
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
	printLogs(o, w, res)
}

// printHealthState prints the release, its workloads, problem pods and warning events.
func printHealthState(o *Options, w io.Writer, res *healthResult) {
	fmt.Fprintf(w, "Release %s/%s revision %d (%s)\n\n", res.Namespace, res.Release, res.Revision, o.paint(res.Status, statusColor(res.Status)))
	if releaseProblem(res.Status) {
		fmt.Fprintf(w, "%s release is %s, so its workloads may not reflect the latest revision yet\n\n", o.paint("warning:", yellow), res.Status)
	}
	if len(res.Workloads) == 0 {
		fmt.Fprintln(w, "no objects that helm --wait waits for in this release revision")
		return
	}
	t := newTable(w)
	t.add("KIND", "NAMESPACE", "NAME", "READY", "STATUS", "DETAILS")
	for _, wl := range res.Workloads {
		ready := ""
		if (wl.Status == healthy || wl.Status == unhealthy) && wl.Kind != "PersistentVolumeClaim" && wl.Kind != "Service" {
			ready = fmt.Sprintf("%d/%d", wl.Ready, wl.Desired)
		}
		t.add(wl.Kind, wl.Namespace, wl.Name, ready, o.paint(wl.Status, statusColor(wl.Status)), wl.Message)
	}
	t.flush()
	if len(res.WaitingFor) > 0 {
		fmt.Fprintf(w, "\nWAITING FOR: %s\n", strings.Join(res.WaitingFor, ", "))
	}

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

// installRank is the position of a kind in Helm's install order.
func installRank(kind string) int {
	return slices.Index(releaseutil.InstallOrder, kind)
}
