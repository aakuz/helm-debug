package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
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
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Reason   string `json:"reason"`
	Restarts int32  `json:"restarts"`
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

func (r *healthResult) problems() bool {
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

func newHealthCommand(o *Options) *cobra.Command {
	var revision int
	cmd := &cobra.Command{
		Use:   "health RELEASE",
		Short: "Check readiness of the Deployments and StatefulSets of a release",
		Long: `Check the rollout status of the Deployments and StatefulSets declared by a release
revision, list their pods that crash or cannot pull an image, and show the warning events of
the last hour for these workloads and pods. Nothing is changed.

Exits with code 2 when a workload is not fully rolled out or absent, or a pod is crashing.`,
		Example: `  helm-debug health backend -n prod
  helm-debug health backend -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			res, err := runHealth(cmd, o, c, args[0], revision)
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
	return cmd
}

func runHealth(cmd *cobra.Command, o *Options, c *Clients, name string, revision int) (*healthResult, error) {
	rel, err := c.Store.Revision(name, c.Namespace, revision)
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
		for _, p := range pods[i] {
			if ph := podProblem(p); ph != nil {
				w.Pods = append(w.Pods, *ph)
			}
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
			ph.Reason = w.Reason
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
			events = append(events, healthEvent{Object: inv.Kind + "/" + inv.Name, Reason: ev.Reason, Message: ev.Message, Count: ev.Count, LastSeen: last.UTC()})
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

func printHealth(o *Options, w io.Writer, res *healthResult) {
	fmt.Fprintf(w, "Release %s/%s revision %d (%s)\n\n", res.Namespace, res.Release, res.Revision, o.paint(res.Status, statusColor(res.Status)))
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
