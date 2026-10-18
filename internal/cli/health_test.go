package cli

import (
	"fmt"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/release/common"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// yaml adds a live object described by a YAML document to the fake cluster.
func (c *cluster) yaml(doc string) {
	c.t.Helper()
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
		c.t.Fatal(err)
	}
	c.live = append(c.live, u)
}

func (c *cluster) deployment(name string, generation, observed, desired, updated, ready, available int) {
	c.t.Helper()
	c.yaml(deploymentDoc(name, generation, observed, desired, updated, ready, available))
}

func deploymentDoc(name string, generation, observed, desired, updated, ready, available int) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: {name: %[1]s, namespace: prod, generation: %[2]d}
spec:
  replicas: %[4]d
  selector: {matchLabels: {app: %[1]s}}
status:
  observedGeneration: %[3]d
  replicas: %[4]d
  updatedReplicas: %[5]d
  readyReplicas: %[6]d
  availableReplicas: %[7]d
`, name, generation, observed, desired, updated, ready, available)
}

func (c *cluster) statefulSet(name string, desired, updated, ready int) {
	c.t.Helper()
	c.yaml(fmt.Sprintf(`apiVersion: apps/v1
kind: StatefulSet
metadata: {name: %[1]s, namespace: prod, generation: 1}
spec:
  replicas: %[2]d
  selector: {matchLabels: {app: %[1]s}}
status:
  observedGeneration: 1
  replicas: %[2]d
  updatedReplicas: %[3]d
  readyReplicas: %[4]d
`, name, desired, updated, ready))
}

// pod adds a pod of the app; waiting is the reason of its waiting container, empty for a running one.
func (c *cluster) pod(name, app, phase, waiting string, restarts int) {
	c.t.Helper()
	state := "running: {startedAt: \"2026-10-18T10:00:00Z\"}"
	if waiting != "" {
		state = "waiting: {reason: " + waiting + "}"
	}
	c.yaml(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s, namespace: prod, labels: {app: %s}}
status:
  phase: %s
  containerStatuses:
  - name: main
    restartCount: %d
    state: {%s}
`, name, app, phase, restarts, state))
}

func (c *cluster) event(name, typ, kind, object, reason, message, last string, count int) {
	c.t.Helper()
	c.yaml(fmt.Sprintf(`apiVersion: v1
kind: Event
metadata: {name: %s, namespace: prod}
type: %s
involvedObject: {kind: %s, name: %s, namespace: prod}
reason: %s
message: %q
count: %d
lastTimestamp: %q
`, name, typ, kind, object, reason, message, count, last))
}

const healthManifest = `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
---
apiVersion: v1
kind: Service
metadata:
  name: web
`

func healthCluster(t *testing.T) *cluster {
	c := newCluster(t)
	c.release("web", "prod", 1, common.StatusDeployed, healthManifest)
	return c
}

func TestHealthHealthy(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 3, 3, 2, 2, 2, 2)
	c.statefulSet("db", 1, 1, 1)
	c.pod("web-1", "web", "Running", "", 0)
	c.event("old", "Warning", "Pod", "web-1", "BackOff", "stale", "2026-10-18T09:00:00Z", 1)
	out, err := run(t, c, "prod", "health", "web")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestHealthUnhealthy(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 3, 3, 3, 3, 1, 1)
	c.statefulSet("db", 2, 2, 1)
	c.pod("web-aaa", "web", "Running", "CrashLoopBackOff", 7)
	c.pod("web-bbb", "web", "Pending", "ImagePullBackOff", 0)
	c.pod("web-ccc", "web", "Running", "", 0)
	c.pod("db-0", "db", "Running", "", 0)
	c.pod("other-0", "other", "Running", "CrashLoopBackOff", 9)
	c.event("e1", "Warning", "Pod", "web-aaa", "BackOff", "Back-off restarting failed container", "2026-10-18T11:50:00Z", 12)
	c.event("e2", "Warning", "ReplicaSet", "web-5d8f", "FailedCreate", "exceeded quota", "2026-10-18T11:55:00Z", 1)
	c.event("e3", "Warning", "Pod", "web-ccc", "BackOff", "older than the window", "2026-10-18T10:30:00Z", 1)
	c.event("e4", "Normal", "Pod", "web-aaa", "Pulled", "not a warning", "2026-10-18T11:59:00Z", 1)
	c.event("e5", "Warning", "Pod", "other-0", "BackOff", "unrelated pod", "2026-10-18T11:59:00Z", 1)
	c.event("e6", "Warning", "ReplicaSet", "web-extra-5d8f", "FailedCreate", "belongs to deployment web-extra", "2026-10-18T11:59:00Z", 1)
	out, err := run(t, c, "prod", "health", "web")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestHealthJSON(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 3, 3, 2, 2, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.pod("web-aaa", "web", "Running", "CrashLoopBackOff", 4)
	c.event("e1", "Warning", "Pod", "web-aaa", "BackOff", "Back-off restarting failed container", "2026-10-18T11:50:00Z", 3)
	out, err := run(t, c, "prod", "health", "web", "-o", "json")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestHealthAbsentWorkload(t *testing.T) {
	c := healthCluster(t)
	c.statefulSet("db", 1, 1, 1)
	out, err := run(t, c, "prod", "health", "web")
	wantProblems(t, err, true)
	if !strings.Contains(out, "absent") {
		t.Fatalf("want absent workload in output:\n%s", out)
	}
}

func TestHealthCrashedPodOfReadyWorkload(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 1, 1, 1, 1, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.pod("web-aaa", "web", "Running", "CrashLoopBackOff", 3)
	_, err := run(t, c, "prod", "health", "web")
	wantProblems(t, err, true)
}

func TestHealthNoWorkloads(t *testing.T) {
	c := newCluster(t)
	c.release("cfg", "prod", 1, common.StatusDeployed, "apiVersion: v1\nkind: Service\nmetadata:\n  name: cfg\n")
	out, err := run(t, c, "prod", "health", "cfg")
	wantProblems(t, err, false)
	if !strings.Contains(out, "no Deployments or StatefulSets") {
		t.Fatalf("want a note about missing workloads:\n%s", out)
	}
}

func TestHealthNotFound(t *testing.T) {
	_, err := run(t, newCluster(t), "prod", "health", "nope")
	if err == nil {
		t.Fatal("want an error for an unknown release")
	}
}

func TestDeploymentState(t *testing.T) {
	tests := []struct {
		name string
		d    string
		want string // substring of the message; empty means healthy
	}{
		{"healthy", `{metadata: {generation: 2}, spec: {replicas: 3}, status: {observedGeneration: 2, replicas: 3, updatedReplicas: 3, readyReplicas: 3, availableReplicas: 3}}`, ""},
		{"scaled to zero", `{metadata: {generation: 1}, spec: {replicas: 0}, status: {observedGeneration: 1}}`, ""},
		{"default replica", `{metadata: {generation: 1}, spec: {}, status: {observedGeneration: 1, replicas: 1, updatedReplicas: 1, readyReplicas: 1, availableReplicas: 1}}`, ""},
		{"not observed", `{metadata: {generation: 3}, spec: {replicas: 1}, status: {observedGeneration: 2, replicas: 1, updatedReplicas: 1, readyReplicas: 1, availableReplicas: 1}}`, "not yet observed"},
		{"outdated", `{metadata: {generation: 1}, spec: {replicas: 3}, status: {observedGeneration: 1, replicas: 3, updatedReplicas: 1, readyReplicas: 3, availableReplicas: 3}}`, "1 of 3 updated"},
		{"old replicas", `{metadata: {generation: 1}, spec: {replicas: 2}, status: {observedGeneration: 1, replicas: 3, updatedReplicas: 2, readyReplicas: 3, availableReplicas: 3}}`, "old replicas"},
		{"unavailable", `{metadata: {generation: 1}, spec: {replicas: 3}, status: {observedGeneration: 1, replicas: 3, updatedReplicas: 3, readyReplicas: 1, availableReplicas: 1}}`, "1 of 3 available"},
		{"deadline", `{metadata: {generation: 1}, spec: {replicas: 1}, status: {observedGeneration: 1, replicas: 1, updatedReplicas: 1, readyReplicas: 1, availableReplicas: 1, conditions: [{type: Progressing, status: "False", reason: ProgressDeadlineExceeded}]}}`, "ProgressDeadlineExceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d appsv1.Deployment
			if err := yaml.Unmarshal([]byte(tt.d), &d); err != nil {
				t.Fatal(err)
			}
			_, _, msg := deploymentState(&d)
			if tt.want == "" && msg != "" || !strings.Contains(msg, tt.want) {
				t.Fatalf("message = %q, want %q", msg, tt.want)
			}
		})
	}
}

func TestStatefulSetState(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want string
	}{
		{"healthy", `{metadata: {generation: 1}, spec: {replicas: 2}, status: {observedGeneration: 1, replicas: 2, updatedReplicas: 2, readyReplicas: 2}}`, ""},
		{"not observed", `{metadata: {generation: 2}, spec: {replicas: 1}, status: {observedGeneration: 1, readyReplicas: 1, updatedReplicas: 1}}`, "not yet observed"},
		{"not ready", `{metadata: {generation: 1}, spec: {replicas: 3}, status: {observedGeneration: 1, replicas: 3, updatedReplicas: 3, readyReplicas: 1}}`, "1 of 3 ready"},
		{"outdated", `{metadata: {generation: 1}, spec: {replicas: 3}, status: {observedGeneration: 1, replicas: 3, updatedReplicas: 1, readyReplicas: 3}}`, "1 of 3 updated"},
		{"partitioned", `{metadata: {generation: 1}, spec: {replicas: 3, updateStrategy: {rollingUpdate: {partition: 2}}}, status: {observedGeneration: 1, replicas: 3, updatedReplicas: 1, readyReplicas: 3}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s appsv1.StatefulSet
			if err := yaml.Unmarshal([]byte(tt.s), &s); err != nil {
				t.Fatal(err)
			}
			_, _, msg := statefulSetState(&s)
			if tt.want == "" && msg != "" || !strings.Contains(msg, tt.want) {
				t.Fatalf("message = %q, want %q", msg, tt.want)
			}
		})
	}
}
