package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/release/common"
)

// Fixtures for the kinds helm --wait waits for besides Deployments and StatefulSets (issue #46).
// Manifest documents are listed in kind order so the table order is the same whether the objects are
// taken in manifest order or sorted by kind.

// manifestDoc is a manifest document of a namespaced object of the release; spec is a YAML flow
// mapping body (without braces), empty for none.
func manifestDoc(apiVersion, kind, name, spec string) string {
	doc := fmt.Sprintf("apiVersion: %s\nkind: %s\nmetadata:\n  name: %s\n", apiVersion, kind, name)
	if spec != "" {
		doc += "spec: {" + spec + "}\n"
	}
	return doc
}

// kindsManifest joins manifest documents into a release manifest.
func kindsManifest(docs ...string) string {
	return "---\n" + strings.Join(docs, "---\n")
}

// kindsCluster is a fake cluster with release prod/app whose manifest is docs.
func kindsCluster(t *testing.T, docs ...string) *cluster {
	t.Helper()
	c := newCluster(t)
	c.release("app", "prod", 1, common.StatusDeployed, kindsManifest(docs...))
	return c
}

// daemonSetDoc is a live DaemonSet whose pods carry app: name; strategy is its updateStrategy as a
// YAML flow mapping.
func daemonSetDoc(name, strategy string, generation, observed, desired, updated, ready int) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: DaemonSet
metadata: {name: %[1]s, namespace: prod, generation: %[3]d}
spec:
  selector: {matchLabels: {app: %[1]s}}
  updateStrategy: %[2]s
status:
  observedGeneration: %[4]d
  desiredNumberScheduled: %[5]d
  updatedNumberScheduled: %[6]d
  numberReady: %[7]d
`, name, strategy, generation, observed, desired, updated, ready)
}

// jobDoc is a live Job whose pods carry app: name; spec and status are YAML flow mapping bodies.
func jobDoc(name, spec, status string) string {
	sel := "selector: {matchLabels: {app: " + name + "}}"
	if spec != "" {
		sel += ", " + spec
	}
	return fmt.Sprintf("apiVersion: batch/v1\nkind: Job\nmetadata: {name: %s, namespace: prod}\nspec: {%s}\nstatus: {%s}\n", name, sel, status)
}

// pvcDoc is a live PersistentVolumeClaim in the given phase (empty for none).
func pvcDoc(name, phase string) string {
	status := "{}"
	if phase != "" {
		status = "{phase: " + phase + "}"
	}
	return fmt.Sprintf("apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: %s, namespace: prod}\nspec: {accessModes: [ReadWriteOnce]}\nstatus: %s\n", name, status)
}

// serviceDoc is a live Service; spec and status are YAML flow mapping bodies.
func serviceDoc(name, spec, status string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: Service\nmetadata: {name: %s, namespace: prod}\nspec: {%s}\nstatus: {%s}\n", name, spec, status)
}

// barePodDoc is a live pod with app: name whose Ready condition is ready ("True", "False", or empty for no conditions).
func barePodDoc(name, ready string) string {
	conds := ""
	if ready != "" {
		conds = "  conditions: [{type: Ready, status: \"" + ready + "\"}]\n"
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %[1]s, namespace: prod, labels: {app: %[1]s}}
status:
  phase: Running
%[2]s  containerStatuses:
  - {name: main, ready: %[3]t, restartCount: 0, state: {running: {startedAt: "2026-10-18T10:00:00Z"}}}
`, name, conds, ready == "True")
}

// kindsJSON is the part of `health -o json` these tests look at.
type kindsJSON struct {
	Workloads []struct {
		Kind    string      `json:"kind"`
		Name    string      `json:"name"`
		Status  string      `json:"status"`
		Message string      `json:"message"`
		Ready   int32       `json:"ready"`
		Desired int32       `json:"desired"`
		Pods    []podHealth `json:"pods"`
		Logs    []struct {
			Pod string `json:"pod"`
		} `json:"logs"`
	} `json:"workloads"`
	Events []struct {
		Object string `json:"object"`
		Reason string `json:"reason"`
	} `json:"events"`
	WaitingFor []string `json:"waitingFor"`
}

func decodeKinds(t *testing.T, out string) kindsJSON {
	t.Helper()
	var res kindsJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decoding %q: %v", out, err)
	}
	return res
}

// goldenKindsManifest has one object of every new kind, plus a ClusterIP Service that must not be listed.
var goldenKindsManifest = []string{
	manifestDoc("apps/v1", "DaemonSet", "agent", ""),
	manifestDoc("batch/v1", "Job", "migrate", ""),
	manifestDoc("v1", "PersistentVolumeClaim", "data", ""),
	manifestDoc("v1", "Pod", "debug", ""),
	manifestDoc("v1", "Service", "internal", "type: ClusterIP"),
	manifestDoc("v1", "Service", "lb", "type: LoadBalancer"),
}

func TestHealthWaitKindsUnhealthy(t *testing.T) {
	c := kindsCluster(t, goldenKindsManifest...)
	c.yaml(daemonSetDoc("agent", "{type: RollingUpdate, rollingUpdate: {maxUnavailable: 1}}", 1, 1, 3, 3, 1))
	c.yaml(jobDoc("migrate", "backoffLimit: 2, completions: 1", "failed: 3"))
	c.yaml(pvcDoc("data", "Pending"))
	c.yaml(barePodDoc("debug", "False"))
	c.yaml(serviceDoc("internal", "type: ClusterIP, clusterIP: 10.0.0.4", ""))
	c.yaml(serviceDoc("lb", "type: LoadBalancer, clusterIP: 10.0.0.5", "loadBalancer: {}"))
	c.event("e1", "Warning", "PersistentVolumeClaim", "data", "ProvisioningFailed", `storageclass.storage.k8s.io "fast" not found`, "2026-10-18T11:58:00Z", 4)
	c.event("e2", "Warning", "Service", "lb", "SyncLoadBalancerFailed", "Error syncing load balancer: quota exceeded", "2026-10-18T11:55:00Z", 2)
	c.event("e3", "Warning", "Pod", "debug", "FailedScheduling", "0/3 nodes are available: 3 Insufficient cpu.", "2026-10-18T11:50:00Z", 5)
	c.event("e4", "Warning", "Service", "internal", "SyncLoadBalancerFailed", "not a listed object", "2026-10-18T11:59:00Z", 1)
	out, err := run(t, c, "prod", "health", "app", "--wait-for-jobs")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestHealthWaitKindsHealthy(t *testing.T) {
	c := kindsCluster(t, goldenKindsManifest...)
	c.yaml(daemonSetDoc("agent", "{type: RollingUpdate, rollingUpdate: {maxUnavailable: 1}}", 2, 2, 3, 3, 3))
	c.readyPod("agent-aaa", "agent")
	c.yaml(jobDoc("migrate", "backoffLimit: 2, completions: 1", "succeeded: 1"))
	c.yaml(pvcDoc("data", "Bound"))
	c.yaml(barePodDoc("debug", "True"))
	c.yaml(serviceDoc("internal", "type: ClusterIP, clusterIP: 10.0.0.4", ""))
	c.yaml(serviceDoc("lb", "type: LoadBalancer, clusterIP: 10.0.0.5", "loadBalancer: {ingress: [{ip: 203.0.113.7}]}"))
	out, err := run(t, c, "prod", "health", "app", "--wait-for-jobs")
	wantProblems(t, err, false)
	if strings.Contains(out, "WAITING FOR") {
		t.Errorf("want no WAITING FOR line when everything is healthy:\n%s", out)
	}
	for _, row := range []string{"DaemonSet", "Job", "PersistentVolumeClaim", "Pod", "Service"} {
		found := false
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, row+" ") && strings.Contains(line, " healthy ") {
				found = true
			}
		}
		if !found {
			t.Errorf("want a healthy %s row:\n%s", row, out)
		}
	}
	if strings.Contains(out, "internal") {
		t.Errorf("a ClusterIP Service must not be listed:\n%s", out)
	}
}

// TestHealthWaitKindRules checks the readiness rule of every new kind through -o json.
func TestHealthWaitKindRules(t *testing.T) {
	ds := func(strategy string, generation, observed, desired, updated, ready int) (string, string) {
		return manifestDoc("apps/v1", "DaemonSet", "agent", ""), daemonSetDoc("agent", strategy, generation, observed, desired, updated, ready)
	}
	job := func(spec, status string) (string, string) {
		return manifestDoc("batch/v1", "Job", "migrate", ""), jobDoc("migrate", spec, status)
	}
	pvc := func(phase string) (string, string) {
		return manifestDoc("v1", "PersistentVolumeClaim", "data", ""), pvcDoc("data", phase)
	}
	lb := func(spec, status string) (string, string) {
		return manifestDoc("v1", "Service", "lb", "type: LoadBalancer"), serviceDoc("lb", spec, status)
	}
	pod := func(ready string) (string, string) {
		return manifestDoc("v1", "Pod", "debug", ""), barePodDoc("debug", ready)
	}
	type want struct {
		kind, name, status, message string
		ready, desired              int32
	}
	tests := []struct {
		name    string
		objects func() (manifest, live string)
		args    []string
		want    *want // nil: the object is not listed
	}{
		// DaemonSet: Helm daemonSetReady.
		{"daemonset generation not observed", func() (string, string) { return ds("{type: RollingUpdate}", 2, 1, 3, 3, 3) }, nil,
			&want{"DaemonSet", "agent", unhealthy, "generation 2 not yet observed by the controller", 3, 3}},
		{"daemonset OnDelete is healthy", func() (string, string) { return ds("{type: OnDelete}", 1, 1, 3, 0, 0) }, nil,
			&want{"DaemonSet", "agent", healthy, "", 0, 3}},
		{"daemonset not all updated", func() (string, string) {
			return ds("{type: RollingUpdate, rollingUpdate: {maxUnavailable: 1}}", 1, 1, 3, 1, 3)
		}, nil, &want{"DaemonSet", "agent", unhealthy, "1 of 3 updated", 3, 3}},
		{"daemonset ready below maxUnavailable int", func() (string, string) {
			return ds("{type: RollingUpdate, rollingUpdate: {maxUnavailable: 1}}", 1, 1, 4, 4, 2)
		}, nil, &want{"DaemonSet", "agent", unhealthy, "2 of 4 ready", 2, 4}},
		{"daemonset ready within maxUnavailable int", func() (string, string) {
			return ds("{type: RollingUpdate, rollingUpdate: {maxUnavailable: 1}}", 1, 1, 4, 4, 3)
		}, nil, &want{"DaemonSet", "agent", healthy, "", 3, 4}},
		{"daemonset ready below maxUnavailable percent", func() (string, string) {
			return ds(`{type: RollingUpdate, rollingUpdate: {maxUnavailable: "50%"}}`, 1, 1, 4, 4, 1)
		}, nil, &want{"DaemonSet", "agent", unhealthy, "1 of 4 ready", 1, 4}},
		{"daemonset maxUnavailable percent rounds up", func() (string, string) {
			return ds(`{type: RollingUpdate, rollingUpdate: {maxUnavailable: "50%"}}`, 1, 1, 3, 3, 1)
		}, nil, &want{"DaemonSet", "agent", healthy, "", 1, 3}},
		{"daemonset without rollingUpdate tolerates all unavailable", func() (string, string) { return ds("{type: RollingUpdate}", 1, 1, 3, 3, 0) }, nil,
			&want{"DaemonSet", "agent", healthy, "", 0, 3}},
		{"daemonset invalid maxUnavailable tolerates all unavailable", func() (string, string) {
			return ds(`{type: RollingUpdate, rollingUpdate: {maxUnavailable: "x"}}`, 1, 1, 3, 3, 0)
		}, nil, &want{"DaemonSet", "agent", healthy, "", 0, 3}},

		// Job: Helm jobReady, only with --wait-for-jobs.
		{"job not listed without --wait-for-jobs", func() (string, string) { return job("backoffLimit: 2, completions: 1", "failed: 3") }, nil, nil},
		{"job failures exceed backoffLimit", func() (string, string) { return job("backoffLimit: 2, completions: 1", "failed: 3") }, []string{"--wait-for-jobs"},
			&want{"Job", "migrate", unhealthy, "failed: 3 failures exceed backoffLimit 2", 0, 1}},
		{"job failures equal to backoffLimit", func() (string, string) { return job("backoffLimit: 2, completions: 1", "failed: 2, succeeded: 1") }, []string{"--wait-for-jobs"},
			&want{"Job", "migrate", healthy, "", 1, 1}},
		{"job backoffLimit defaults to 6", func() (string, string) { return job("completions: 1", "failed: 7") }, []string{"--wait-for-jobs"},
			&want{"Job", "migrate", unhealthy, "failed: 7 failures exceed backoffLimit 6", 0, 1}},
		{"job within default backoffLimit", func() (string, string) { return job("completions: 1", "failed: 6, succeeded: 1") }, []string{"--wait-for-jobs"},
			&want{"Job", "migrate", healthy, "", 1, 1}},
		{"job not all succeeded", func() (string, string) { return job("backoffLimit: 2, completions: 3", "succeeded: 1") }, []string{"--wait-for-jobs"},
			&want{"Job", "migrate", unhealthy, "1 of 3 succeeded", 1, 3}},
		{"job without completions is healthy", func() (string, string) { return job("backoffLimit: 2", "") }, []string{"--wait-for-jobs"},
			&want{"Job", "migrate", healthy, "", 0, 1}},

		// PersistentVolumeClaim.
		{"pvc pending", func() (string, string) { return pvc("Pending") }, nil, &want{"PersistentVolumeClaim", "data", unhealthy, "phase Pending", 0, 0}},
		{"pvc lost", func() (string, string) { return pvc("Lost") }, nil, &want{"PersistentVolumeClaim", "data", unhealthy, "phase Lost", 0, 0}},
		{"pvc without phase", func() (string, string) { return pvc("") }, nil, &want{"PersistentVolumeClaim", "data", unhealthy, "phase unknown", 0, 0}},
		{"pvc bound", func() (string, string) { return pvc("Bound") }, nil, &want{"PersistentVolumeClaim", "data", healthy, "", 0, 0}},

		// Service of type LoadBalancer: Helm serviceReady.
		{"loadbalancer without ingress", func() (string, string) { return lb("type: LoadBalancer, clusterIP: 10.0.0.5", "loadBalancer: {}") }, nil,
			&want{"Service", "lb", unhealthy, "no load balancer address yet", 0, 0}},
		{"loadbalancer with ingress hostname", func() (string, string) {
			return lb("type: LoadBalancer, clusterIP: 10.0.0.5", "loadBalancer: {ingress: [{hostname: lb.example.com}]}")
		}, nil, &want{"Service", "lb", healthy, "", 0, 0}},
		{"loadbalancer with externalIPs", func() (string, string) {
			return lb("type: LoadBalancer, clusterIP: 10.0.0.5, externalIPs: [203.0.113.9]", "loadBalancer: {}")
		}, nil, &want{"Service", "lb", healthy, "", 0, 0}},
		{"loadbalancer without cluster IP", func() (string, string) { return lb("type: LoadBalancer", "loadBalancer: {}") }, nil,
			&want{"Service", "lb", unhealthy, "no cluster IP", 0, 0}},
		{"service is listed by the manifest type, not the live one", func() (string, string) {
			return manifestDoc("v1", "Service", "lb", "type: ClusterIP"), serviceDoc("lb", "type: LoadBalancer, clusterIP: 10.0.0.5", "loadBalancer: {}")
		}, nil, nil},

		// Pod.
		{"pod ready", func() (string, string) { return pod("True") }, nil, &want{"Pod", "debug", healthy, "", 1, 1}},
		{"pod not ready", func() (string, string) { return pod("False") }, nil, &want{"Pod", "debug", unhealthy, "not ready", 0, 1}},
		{"pod without conditions", func() (string, string) { return pod("") }, nil, &want{"Pod", "debug", unhealthy, "not ready", 0, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			manifest, live := tt.objects()
			c := kindsCluster(t, manifest)
			c.yaml(live)
			out, err := run(t, c, "prod", append([]string{"health", "app", "-o", "json"}, tt.args...)...)
			if tt.want == nil {
				wantProblems(t, err, false)
				if res := decodeKinds(t, out); len(res.Workloads) != 0 || len(res.WaitingFor) != 0 {
					t.Fatalf("want nothing listed, got %s", out)
				}
				return
			}
			wantProblems(t, err, tt.want.status != healthy)
			res := decodeKinds(t, out)
			if len(res.Workloads) != 1 {
				t.Fatalf("want one listed object, got %s", out)
			}
			w := res.Workloads[0]
			got := want{w.Kind, w.Name, w.Status, w.Message, w.Ready, w.Desired}
			if got != *tt.want {
				t.Errorf("got %+v, want %+v", got, *tt.want)
			}
			var wantWaiting []string
			if tt.want.status != healthy {
				wantWaiting = []string{tt.want.kind + "/" + tt.want.name}
			}
			if !slices.Equal(res.WaitingFor, wantWaiting) {
				t.Errorf("waitingFor = %q, want %q", res.WaitingFor, wantWaiting)
			}
			if wantWaiting == nil && strings.Contains(out, "waitingFor") {
				t.Errorf("waitingFor must be omitted when empty:\n%s", out)
			}
		})
	}
}

func TestHealthWaitKindsJobListedOnlyWithFlag(t *testing.T) {
	for _, tt := range []struct {
		name   string
		args   []string
		listed bool
	}{
		{"without --wait-for-jobs", nil, false},
		{"with --wait-for-jobs", []string{"--wait-for-jobs"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := kindsCluster(t, manifestDoc("batch/v1", "Job", "migrate", ""))
			c.yaml(jobDoc("migrate", "completions: 1", "succeeded: 1"))
			out, err := run(t, c, "prod", append([]string{"health", "app", "-o", "json"}, tt.args...)...)
			wantProblems(t, err, false)
			res := decodeKinds(t, out)
			if listed := len(res.Workloads) == 1 && res.Workloads[0].Kind == "Job"; listed != tt.listed {
				t.Fatalf("Job listed = %v, want %v:\n%s", listed, tt.listed, out)
			}
		})
	}
}

func TestHealthWaitKindsAbsent(t *testing.T) {
	c := kindsCluster(t,
		manifestDoc("apps/v1", "DaemonSet", "agent", ""),
		manifestDoc("v1", "PersistentVolumeClaim", "data", ""),
		manifestDoc("v1", "Pod", "debug", ""),
		manifestDoc("v1", "Service", "lb", "type: LoadBalancer"),
	)
	out, err := run(t, c, "prod", "health", "app", "-o", "json")
	wantProblems(t, err, true)
	res := decodeKinds(t, out)
	want := []string{"DaemonSet/agent", "PersistentVolumeClaim/data", "Pod/debug", "Service/lb"}
	var got []string
	for _, w := range res.Workloads {
		if w.Status != "absent" {
			t.Errorf("%s/%s status = %q, want absent", w.Kind, w.Name, w.Status)
		}
		got = append(got, w.Kind+"/"+w.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("listed %q, want %q", got, want)
	}
	if !slices.Equal(res.WaitingFor, want) {
		t.Errorf("waitingFor = %q, want %q", res.WaitingFor, want)
	}
}

func TestHealthWaitKindsCrashingPodNotWaitedFor(t *testing.T) {
	c := kindsCluster(t,
		manifestDoc("apps/v1", "Deployment", "web", ""),
		manifestDoc("v1", "PersistentVolumeClaim", "data", ""),
	)
	c.deployment("web", 1, 1, 1, 1, 1, 1)
	c.pod("web-aaa", "web", "Running", "CrashLoopBackOff", 3)
	c.yaml(pvcDoc("data", "Pending"))

	out, err := run(t, c, "prod", "health", "app", "-o", "json")
	wantProblems(t, err, true)
	res := decodeKinds(t, out)
	if want := []string{"PersistentVolumeClaim/data"}; !slices.Equal(res.WaitingFor, want) {
		t.Errorf("waitingFor = %q, want %q (a healthy Deployment with a crashing pod is not waited for)", res.WaitingFor, want)
	}

	out, err = run(t, c, "prod", "health", "app")
	wantProblems(t, err, true)
	if !strings.Contains(out, "\nWAITING FOR: PersistentVolumeClaim/data\n") {
		t.Errorf("want WAITING FOR naming only the PVC:\n%s", out)
	}
}

func TestHealthWaitKindsDeploymentCrashingPodOnly(t *testing.T) {
	c := kindsCluster(t, manifestDoc("apps/v1", "Deployment", "web", ""))
	c.deployment("web", 1, 1, 1, 1, 1, 1)
	c.pod("web-aaa", "web", "Running", "CrashLoopBackOff", 3)
	out, err := run(t, c, "prod", "health", "app")
	wantProblems(t, err, true)
	if strings.Contains(out, "WAITING FOR") {
		t.Errorf("a healthy Deployment with a crashing pod is not waited for:\n%s", out)
	}
}

func TestHealthWaitKindsPodsWithProblems(t *testing.T) {
	c := kindsCluster(t,
		manifestDoc("apps/v1", "DaemonSet", "agent", ""),
		manifestDoc("v1", "Pod", "debug", ""),
	)
	c.yaml(daemonSetDoc("agent", "{type: RollingUpdate, rollingUpdate: {maxUnavailable: 1}}", 1, 1, 2, 2, 2))
	c.pod("agent-aaa", "agent", "Running", "CrashLoopBackOff", 5)
	c.readyPod("agent-bbb", "agent")
	c.pod("debug", "debug", "Running", "CrashLoopBackOff", 2)
	c.pod("other-0", "other", "Running", "CrashLoopBackOff", 9)
	c.setLog("agent-aaa/main!previous", "panic: agent\n")
	c.setLog("debug/main!previous", "panic: debug\n")

	out, err := run(t, c, "prod", "health", "app")
	wantProblems(t, err, true)
	_, pods, ok := strings.Cut(out, "Pods with problems:\n")
	if !ok {
		t.Fatalf("want a Pods with problems section:\n%s", out)
	}
	pods, _, _ = strings.Cut(pods, "\n\n")
	for _, p := range []string{"agent-aaa", "debug"} {
		if !strings.Contains(pods, "\n"+p+" ") {
			t.Errorf("want %s under Pods with problems:\n%s", p, out)
		}
	}
	for _, p := range []string{"agent-bbb", "other-0"} {
		if strings.Contains(pods, p) {
			t.Errorf("%s must not be under Pods with problems:\n%s", p, out)
		}
	}

	out, err = run(t, c, "prod", "health", "app", "-o", "json", "--logs", "5")
	wantProblems(t, err, true)
	res := decodeKinds(t, out)
	podsOf := map[string][]string{}
	logsOf := map[string][]string{}
	for _, w := range res.Workloads {
		for _, p := range w.Pods {
			podsOf[w.Kind] = append(podsOf[w.Kind], p.Name)
		}
		for _, l := range w.Logs {
			logsOf[w.Kind] = append(logsOf[w.Kind], l.Pod)
		}
	}
	if !slices.Equal(podsOf["DaemonSet"], []string{"agent-aaa"}) || !slices.Equal(podsOf["Pod"], []string{"debug"}) {
		t.Errorf("problem pods = %v, want DaemonSet [agent-aaa] and Pod [debug]", podsOf)
	}
	if !slices.Contains(logsOf["DaemonSet"], "agent-aaa") || !slices.Contains(logsOf["Pod"], "debug") {
		t.Errorf("logs of pods = %v, want agent-aaa under DaemonSet and debug under Pod", logsOf)
	}
}

func TestHealthWaitKindsJobEvent(t *testing.T) {
	for _, tt := range []struct {
		name  string
		args  []string
		shown bool
	}{
		{"without --wait-for-jobs", nil, false},
		{"with --wait-for-jobs", []string{"--wait-for-jobs"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := kindsCluster(t, manifestDoc("batch/v1", "Job", "migrate", ""))
			c.yaml(jobDoc("migrate", "backoffLimit: 1, completions: 1", "failed: 2"))
			c.event("e1", "Warning", "Job", "migrate", "BackoffLimitExceeded", "Job has reached the specified backoff limit", "2026-10-18T11:58:00Z", 1)
			out, err := run(t, c, "prod", append([]string{"health", "app", "-o", "json"}, tt.args...)...)
			wantProblems(t, err, tt.shown)
			res := decodeKinds(t, out)
			shown := slices.ContainsFunc(res.Events, func(e struct {
				Object string `json:"object"`
				Reason string `json:"reason"`
			}) bool {
				return e.Object == "Job/migrate" && e.Reason == "BackoffLimitExceeded"
			})
			if shown != tt.shown {
				t.Fatalf("BackoffLimitExceeded shown = %v, want %v:\n%s", shown, tt.shown, out)
			}
		})
	}
}

func TestHealthWaitKindsNothingToWaitFor(t *testing.T) {
	c := kindsCluster(t,
		manifestDoc("v1", "ConfigMap", "cfg", ""),
		manifestDoc("v1", "Service", "internal", "type: ClusterIP"),
	)
	out, err := run(t, c, "prod", "health", "app")
	wantProblems(t, err, false)
	if !strings.Contains(out, "no objects that helm --wait waits for in this release revision") {
		t.Fatalf("want a note that nothing is waited for:\n%s", out)
	}
}
