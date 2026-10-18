package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

var (
	deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	podsGVR        = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
)

// waitRun is the result of a health --wait run against a fake cluster and a fake clock.
type waitRun struct {
	out    string
	err    error
	sleeps []time.Duration
}

// runWait runs the CLI with a fake clock: every pause advances time by its duration and then calls
// onSleep(n, clients), which can change the cluster between two polls (n counts pauses from 1).
func runWait(t *testing.T, c *cluster, onSleep func(n int, cl *Clients), args ...string) waitRun {
	t.Helper()
	cl := c.clients("prod")
	clock := now
	var res waitRun
	var out bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut = &out, &bytes.Buffer{}
	o.Now = func() time.Time { return clock }
	o.NewClients = func(*Options) (*Clients, error) { return cl, nil }
	o.Sleep = func(_ context.Context, d time.Duration) error {
		res.sleeps = append(res.sleeps, d)
		clock = clock.Add(d)
		if onSleep != nil {
			onSleep(len(res.sleeps), cl)
		}
		return nil
	}
	cmd := NewRootCommand(o)
	cmd.SetArgs(args)
	res.err = cmd.ExecuteContext(context.Background())
	res.out = out.String()
	return res
}

func setDeployment(t *testing.T, cl *Clients, doc string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Cluster.Dynamic.Resource(deploymentsGVR).Namespace("prod").Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func deletePod(t *testing.T, cl *Clients, name string) {
	t.Helper()
	if err := cl.Cluster.Dynamic.Resource(podsGVR).Namespace("prod").Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
}

// recoveringCluster is web 1/3 with a crash-looping pod; healthy tells the cluster to heal.
func recoveringCluster(t *testing.T) *cluster {
	c := healthCluster(t)
	c.deployment("web", 3, 3, 3, 3, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.pod("web-aaa", "web", "Running", "CrashLoopBackOff", 7)
	c.setLog("web-aaa/main!previous", "starting\npanic: no database\n")
	return c
}

func TestHealthWaitBecomesHealthy(t *testing.T) {
	c := recoveringCluster(t)
	r := runWait(t, c, func(n int, cl *Clients) {
		switch n {
		case 1: // a replica came up, the pod keeps crashing with a new reason in its log
			setDeployment(t, cl, deploymentDoc("web", 3, 3, 3, 3, 2, 2))
			c.setLog("web-aaa/main!previous", "starting\nconnection refused\n")
		case 2: // fully rolled out
			setDeployment(t, cl, deploymentDoc("web", 3, 3, 3, 3, 3, 3))
			deletePod(t, cl, "web-aaa")
		}
	}, "health", "web", "--wait", "--logs", "2")
	wantProblems(t, r.err, false)
	golden(t, r.out)
	if want := []time.Duration{2 * time.Second, 2 * time.Second}; !equalDurations(r.sleeps, want) {
		t.Errorf("pauses = %v, want %v", r.sleeps, want)
	}
}

func TestHealthWaitAlreadyHealthy(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 1, 1, 1, 1, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	r := runWait(t, c, nil, "health", "web", "--wait")
	wantProblems(t, r.err, false)
	if len(r.sleeps) != 0 || !strings.Contains(r.out, "] healthy") || strings.Count(r.out, "Release prod/web") != 1 {
		t.Fatalf("want one healthy snapshot and no pauses, got %v:\n%s", r.sleeps, r.out)
	}
}

func TestHealthWaitTimeout(t *testing.T) {
	c := recoveringCluster(t)
	r := runWait(t, c, nil, "health", "web", "--wait", "--timeout", "5s", "--interval", "2s")
	wantProblems(t, r.err, true)
	// polls at 0s, 2s, 4s and 5s: the last pause is cut to the time left
	if want := []time.Duration{2 * time.Second, 2 * time.Second, time.Second}; !equalDurations(r.sleeps, want) {
		t.Errorf("pauses = %v, want %v", r.sleeps, want)
	}
	if strings.Count(r.out, "Release prod/web") != 1 {
		t.Errorf("an unchanged state must be printed once:\n%s", r.out)
	}
	if !strings.Contains(r.out, "timed out after 5s") {
		t.Errorf("want a timeout line:\n%s", r.out)
	}
}

func TestHealthWaitPrintsUpdateWhenOnlyLogsChange(t *testing.T) {
	c := recoveringCluster(t)
	r := runWait(t, c, func(n int, _ *Clients) {
		if n == 1 {
			c.setLog("web-aaa/main!previous", "starting\nconnection refused\n")
		}
	}, "health", "web", "--wait", "--logs", "2", "--timeout", "4s", "--interval", "2s")
	wantProblems(t, r.err, true)
	if strings.Count(r.out, "Release prod/web") != 2 || !strings.Contains(r.out, "connection refused") {
		t.Fatalf("want a second snapshot with the new log line:\n%s", r.out)
	}
}

func TestHealthWaitStructuredOutputIsFinalSnapshotOnly(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			c := recoveringCluster(t)
			r := runWait(t, c, func(n int, cl *Clients) {
				if n == 1 {
					setDeployment(t, cl, deploymentDoc("web", 3, 3, 3, 3, 3, 3))
					deletePod(t, cl, "web-aaa")
				}
			}, "health", "web", "--wait", "-o", format)
			wantProblems(t, r.err, false)
			if format == "json" {
				var v map[string]any
				if err := json.Unmarshal([]byte(r.out), &v); err != nil {
					t.Fatalf("output is not a single JSON document: %v\n%s", err, r.out)
				}
			}
			if strings.Count(r.out, "release:")+strings.Count(r.out, `"release"`) != 1 || strings.Contains(r.out, "unhealthy") {
				t.Fatalf("want only the final healthy snapshot:\n%s", r.out)
			}
		})
	}
}

func TestHealthWaitStructuredTimeoutPrintsSnapshot(t *testing.T) {
	c := recoveringCluster(t)
	r := runWait(t, c, nil, "health", "web", "--wait", "--timeout", "2s", "-o", "json")
	wantProblems(t, r.err, true)
	var v map[string]any
	if err := json.Unmarshal([]byte(r.out), &v); err != nil {
		t.Fatalf("output is not a single JSON document: %v\n%s", err, r.out)
	}
}

func TestHealthWaitInvalidDurations(t *testing.T) {
	c := recoveringCluster(t)
	for _, args := range [][]string{{"--timeout", "0s"}, {"--interval", "0s"}, {"--timeout", "-1s"}} {
		r := runWait(t, c, nil, append([]string{"health", "web", "--wait"}, args...)...)
		if r.err == nil || errors.Is(r.err, ErrProblems) || !strings.Contains(r.err.Error(), args[0]) {
			t.Errorf("%v: want an error naming the flag, got %v", args, r.err)
		}
		if len(r.sleeps) != 0 {
			t.Errorf("%v: waited despite invalid flags", args)
		}
	}
}

func TestHealthWaitUnknownReleaseFailsFast(t *testing.T) {
	r := runWait(t, newCluster(t), nil, "health", "nope", "--wait")
	if r.err == nil || errors.Is(r.err, ErrProblems) || len(r.sleeps) != 0 {
		t.Fatalf("want an immediate error, got %v after %d pauses", r.err, len(r.sleeps))
	}
}

func TestHealthWaitInterrupted(t *testing.T) {
	c := recoveringCluster(t)
	cl := c.clients("prod")
	o := NewOptions()
	o.Out, o.ErrOut = &bytes.Buffer{}, &bytes.Buffer{}
	o.Now = func() time.Time { return now }
	o.NewClients = func(*Options) (*Clients, error) { return cl, nil }
	o.Sleep = func(context.Context, time.Duration) error { return context.Canceled }
	cmd := NewRootCommand(o)
	cmd.SetArgs([]string{"health", "web", "--wait"})
	err := cmd.ExecuteContext(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestSleepContext(t *testing.T) {
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
