package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func jobHook(name string, phase rspb.HookPhase, weight int, policies ...rspb.HookDeletePolicy) *rspb.Hook {
	return &rspb.Hook{
		Name: name, Kind: "Job", Weight: weight, DeletePolicies: policies,
		Events:   []rspb.HookEvent{rspb.HookPreUpgrade, rspb.HookPostUpgrade},
		Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: " + name + "\n",
		LastRun:  rspb.HookExecution{Phase: phase, StartedAt: now.Add(-2 * time.Hour), CompletedAt: now.Add(-time.Hour)},
	}
}

type hookJSON struct {
	Name, Kind, Phase, Event, Status string
	Weight                           int
	DeletePolicies                   []string
	Orphan                           bool
	LastRun                          time.Time
}

func hooksJSON(t *testing.T, c *cluster, args ...string) (rows []hookJSON, err error) {
	t.Helper()
	out, err := run(t, c, "prod", append([]string{"hooks", "-o", "json"}, args...)...)
	var res struct{ Hooks []hookJSON }
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("bad json: %v\n%s", jerr, out)
	}
	return res.Hooks, err
}

func TestHooksOrphanDetected(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, "", jobHook("api-migrate", rspb.HookPhaseSucceeded, 5, rspb.HookSucceeded))
	c.object("batch/v1", "Job", "prod", "api-migrate", "", "")
	out, err := run(t, c, "prod", "hooks", "api")
	wantProblems(t, err, true)
	for _, want := range []string{"api-migrate", "pre-upgrade,post-upgrade", "hook-succeeded", "Succeeded", "orphan"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestHooksCleanCases(t *testing.T) {
	tests := []struct {
		name string
		hook *rspb.Hook
		live bool
	}{
		{"object gone", jobHook("j", rspb.HookPhaseSucceeded, 0, rspb.HookSucceeded), false},
		{"kept by policy", jobHook("j", rspb.HookPhaseSucceeded, 0, rspb.HookBeforeHookCreation), true},
		{"no policy", jobHook("j", rspb.HookPhaseSucceeded, 0), true},
		{"still running", jobHook("j", rspb.HookPhaseRunning, 0, rspb.HookSucceeded), true},
		{"failed kept for hook-succeeded only", jobHook("j", rspb.HookPhaseFailed, 0, rspb.HookSucceeded), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCluster(t)
			c.release("api", "prod", 1, common.StatusDeployed, "", tt.hook)
			if tt.live {
				c.object("batch/v1", "Job", "prod", "j", "", "")
			}
			rows, err := hooksJSON(t, c, "api")
			wantProblems(t, err, false)
			if len(rows) != 1 || rows[0].Orphan {
				t.Fatalf("rows = %+v", rows)
			}
		})
	}
}

func TestHooksFailedOrphan(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusFailed, "", jobHook("j", rspb.HookPhaseFailed, 0, rspb.HookFailed))
	// A finished Job: one without a Complete or Failed condition is still running (issue #47).
	c.yaml(jobDoc("j", "", `failed: 1, conditions: [{type: Failed, status: "True"}]`))
	rows, err := hooksJSON(t, c, "api")
	wantProblems(t, err, true)
	if !rows[0].Orphan || rows[0].Status != "present" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestHooksJSONFields(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, "", jobHook("j", rspb.HookPhaseSucceeded, 7, rspb.HookSucceeded, rspb.HookFailed))
	rows, err := hooksJSON(t, c, "api")
	wantProblems(t, err, false)
	r := rows[0]
	if r.Name != "j" || r.Kind != "Job" || r.Phase != "Succeeded" || r.Weight != 7 || r.Event != "pre-upgrade,post-upgrade" ||
		strings.Join(r.DeletePolicies, ",") != "hook-succeeded,hook-failed" || !r.LastRun.Equal(now.Add(-time.Hour)) {
		t.Fatalf("row = %+v", r)
	}
}

func TestHooksRevision(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusSuperseded, "", jobHook("old", rspb.HookPhaseSucceeded, 0))
	c.release("api", "prod", 2, common.StatusDeployed, "", jobHook("new", rspb.HookPhaseSucceeded, 0))
	rows, _ := hooksJSON(t, c, "api")
	if len(rows) != 1 || rows[0].Name != "new" {
		t.Fatalf("latest: %+v", rows)
	}
	rows, _ = hooksJSON(t, c, "api", "--revision", "1")
	if len(rows) != 1 || rows[0].Name != "old" {
		t.Fatalf("rev 1: %+v", rows)
	}
}

func TestHooksSortedByWeight(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, "", jobHook("b", rspb.HookPhaseSucceeded, 2), jobHook("a", rspb.HookPhaseSucceeded, 2), jobHook("c", rspb.HookPhaseSucceeded, -1))
	rows, _ := hooksJSON(t, c, "api")
	if len(rows) != 3 || rows[0].Name != "c" || rows[1].Name != "a" || rows[2].Name != "b" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestHooksNoHooks(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, simpleManifest)
	out, err := run(t, c, "prod", "hooks", "api")
	wantProblems(t, err, false)
	if !strings.Contains(out, "no hooks") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestHooksUnknownRelease(t *testing.T) {
	_, err := run(t, newCluster(t), "prod", "hooks", "nope")
	if err == nil {
		t.Fatal("want error")
	}
}

func TestHooksNeverRun(t *testing.T) {
	c := newCluster(t)
	h := jobHook("j", "", 0, rspb.HookSucceeded)
	h.LastRun = rspb.HookExecution{}
	c.release("api", "prod", 1, common.StatusDeployed, "", h)
	c.object("batch/v1", "Job", "prod", "j", "", "")
	rows, err := hooksJSON(t, c, "api")
	wantProblems(t, err, false)
	if rows[0].Orphan || !rows[0].LastRun.IsZero() {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestHooksLookupErrorIsProblem(t *testing.T) {
	c := newCluster(t)
	// Pod is not served by the fake cluster, so the lookup fails with unknown-kind.
	c.release("api", "prod", 1, common.StatusDeployed, "", &rspb.Hook{
		Name: "p", Kind: "Pod", Manifest: "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\n",
	})
	out, err := run(t, c, "prod", "hooks", "api")
	wantProblems(t, err, true)
	if !strings.Contains(out, "error:") {
		t.Fatalf("out:\n%s", out)
	}
}

// Issue #47: running state, warning events, logs and absent reasons of hook Jobs and Pods.

// podKind serves Pods through the fake API server so Pod hooks can be looked up.
var podKind = kindInfo{schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, "pods", true}

func podHook(name string, phase rspb.HookPhase, policies ...rspb.HookDeletePolicy) *rspb.Hook {
	return &rspb.Hook{
		Name: name, Kind: "Pod", DeletePolicies: policies,
		Events:   []rspb.HookEvent{rspb.HookPreInstall},
		Manifest: "apiVersion: v1\nkind: Pod\nmetadata:\n  name: " + name + "\n",
		LastRun:  rspb.HookExecution{Phase: phase, StartedAt: now.Add(-2 * time.Hour)},
	}
}

// hooksDetailJSON is the part of `hooks -o json` the issue #47 tests look at.
type hooksDetailJSON struct {
	Hooks []struct {
		Name         string `json:"name"`
		Status       string `json:"status"`
		Orphan       bool   `json:"orphan"`
		Active       int    `json:"active"`
		Failed       int    `json:"failed"`
		AbsentReason string `json:"absentReason"`
		Error        string `json:"error"`
		Logs         []struct {
			Pod       string   `json:"pod"`
			Container string   `json:"container"`
			Previous  bool     `json:"previous"`
			Lines     []string `json:"lines"`
			Error     string   `json:"error"`
		} `json:"logs"`
	} `json:"hooks"`
	Events []struct {
		Object  string `json:"object"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"events"`
	EventsError string `json:"eventsError"`
}

func hooksDetail(t *testing.T, c *cluster, args ...string) (hooksDetailJSON, error) {
	t.Helper()
	out, err := run(t, c, "prod", append([]string{"hooks", "-o", "json"}, args...)...)
	var res hooksDetailJSON
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("bad json: %v\n%s", jerr, out)
	}
	return res, err
}

// hookLine returns the table line of the hook named name.
func hookLine(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, name+" ") {
			return line
		}
	}
	t.Fatalf("no table line for hook %q:\n%s", name, out)
	return ""
}

func TestHooksJobRunningState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		phase        rspb.HookPhase
		policies     []rspb.HookDeletePolicy
		status       string // Job status YAML flow mapping body
		wantStatus   string
		wantActive   int
		wantFailed   int
		wantOrphan   bool
		wantProblems bool
	}{
		{name: "no condition is running", phase: rspb.HookPhaseRunning, status: "active: 1, failed: 2",
			wantStatus: "running", wantActive: 1, wantFailed: 2},
		{name: "complete condition false is running", phase: rspb.HookPhaseRunning, status: `active: 1, conditions: [{type: Complete, status: "False"}]`,
			wantStatus: "running", wantActive: 1},
		{name: "complete is present", phase: rspb.HookPhaseSucceeded, status: `succeeded: 1, conditions: [{type: Complete, status: "True"}]`,
			wantStatus: "present"},
		{name: "failed is present", phase: rspb.HookPhaseFailed, status: `failed: 3, conditions: [{type: Failed, status: "True"}]`,
			wantStatus: "present", wantFailed: 3},
		{name: "complete with hook-succeeded is orphan", phase: rspb.HookPhaseSucceeded, policies: []rspb.HookDeletePolicy{rspb.HookSucceeded},
			status: `succeeded: 1, conditions: [{type: Complete, status: "True"}]`, wantStatus: "present", wantOrphan: true, wantProblems: true},
		{name: "failed with hook-failed is orphan", phase: rspb.HookPhaseFailed, policies: []rspb.HookDeletePolicy{rspb.HookFailed},
			status: `failed: 1, conditions: [{type: Failed, status: "True"}]`, wantStatus: "present", wantFailed: 1, wantOrphan: true, wantProblems: true},
		{name: "running with hook-succeeded is not orphan", phase: rspb.HookPhaseRunning, policies: []rspb.HookDeletePolicy{rspb.HookSucceeded},
			status: "active: 1", wantStatus: "running", wantActive: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newCluster(t)
			c.release("api", "prod", 1, common.StatusDeployed, "", jobHook("migrate", tt.phase, 0, tt.policies...))
			c.yaml(jobDoc("migrate", "backoffLimit: 6", tt.status))

			res, err := hooksDetail(t, c, "api")
			wantProblems(t, err, tt.wantProblems)
			if len(res.Hooks) != 1 {
				t.Fatalf("hooks = %+v", res.Hooks)
			}
			r := res.Hooks[0]
			if r.Status != tt.wantStatus || r.Active != tt.wantActive || r.Failed != tt.wantFailed || r.Orphan != tt.wantOrphan {
				t.Errorf("row = %+v, want status %q active %d failed %d orphan %t", r, tt.wantStatus, tt.wantActive, tt.wantFailed, tt.wantOrphan)
			}

			out, err := run(t, c, "prod", "hooks", "api")
			wantProblems(t, err, tt.wantProblems)
			header := hookLine(t, out, "NAME")
			if !strings.Contains(header, "STATUS") || !strings.Contains(header, "DETAILS") || strings.Index(header, "DETAILS") < strings.Index(header, "STATUS") {
				t.Errorf("want a DETAILS column after STATUS: %q", header)
			}
			line := hookLine(t, out, "migrate")
			wantStatus := tt.wantStatus
			if tt.wantOrphan {
				wantStatus = "orphan"
			}
			details := fmt.Sprintf("active %d, failed %d", tt.wantActive, tt.wantFailed)
			if !strings.Contains(line, " "+wantStatus+" ") || !strings.Contains(line, details) {
				t.Errorf("table line %q, want status %q and details %q", line, wantStatus, details)
			}
		})
	}
}

func TestHooksPodRunningState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, podPhase, wantStatus string
	}{
		{"running pod is running", "Running", "running"},
		{"pending pod is running", "Pending", "running"},
		{"succeeded pod is present", "Succeeded", "present"},
		{"failed pod is present", "Failed", "present"},
		{"unknown pod is running", "Unknown", "running"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newCluster(t)
			c.extraKinds = []kindInfo{podKind}
			c.release("api", "prod", 1, common.StatusDeployed, "", podHook("check", rspb.HookPhaseRunning))
			c.pod("check", "check", tt.podPhase, "", 0)

			res, err := hooksDetail(t, c, "api")
			wantProblems(t, err, false)
			if len(res.Hooks) != 1 || res.Hooks[0].Status != tt.wantStatus || res.Hooks[0].Orphan {
				t.Fatalf("hooks = %+v, want status %q", res.Hooks, tt.wantStatus)
			}
			out, err := run(t, c, "prod", "hooks", "api")
			wantProblems(t, err, false)
			if line := hookLine(t, out, "check"); !strings.Contains(line, " "+tt.wantStatus) {
				t.Errorf("table line %q, want status %q", line, tt.wantStatus)
			}
		})
	}
}

func TestHooksPodOrphanUnchanged(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.extraKinds = []kindInfo{podKind}
	c.release("api", "prod", 1, common.StatusDeployed, "", podHook("check", rspb.HookPhaseSucceeded, rspb.HookSucceeded))
	c.pod("check", "check", "Succeeded", "", 0)
	res, err := hooksDetail(t, c, "api")
	wantProblems(t, err, true)
	if len(res.Hooks) != 1 || res.Hooks[0].Status != "present" || !res.Hooks[0].Orphan {
		t.Fatalf("hooks = %+v", res.Hooks)
	}
}

// hookEventsCluster has hook Job migrate with pod migrate-aaa and an unrelated pod, and warning,
// normal, stale and unrelated events.
func hookEventsCluster(t *testing.T, hookPhase rspb.HookPhase, jobStatus string) *cluster {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusFailed, "", jobHook("migrate", hookPhase, 0, rspb.HookBeforeHookCreation))
	c.yaml(jobDoc("migrate", "backoffLimit: 2", jobStatus))
	c.pod("migrate-aaa", "migrate", "Running", "CrashLoopBackOff", 2)
	c.pod("other-0", "other", "Running", "CrashLoopBackOff", 9)
	c.event("e1", "Warning", "Job", "migrate", "BackoffLimitExceeded", "Job has reached the specified backoff limit", "2026-10-18T11:58:00Z", 1)
	c.event("e2", "Warning", "Pod", "migrate-aaa", "BackOff", "Back-off restarting failed container", "2026-10-18T11:50:00Z", 4)
	c.event("e3", "Normal", "Pod", "migrate-aaa", "Pulled", "not a warning", "2026-10-18T11:59:00Z", 1)
	c.event("e4", "Warning", "Pod", "other-0", "BackOff", "unrelated pod", "2026-10-18T11:59:00Z", 1)
	c.event("e5", "Warning", "Pod", "migrate-aaa", "BackOff", "older than the window", "2026-10-18T10:30:00Z", 1)
	c.event("e6", "Warning", "Job", "other", "BackoffLimitExceeded", "unrelated job", "2026-10-18T11:59:00Z", 1)
	return c
}

func TestHooksWarningEvents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, jobStatus string
		hookPhase       rspb.HookPhase
	}{
		{"running hook Job", "active: 1, failed: 2", rspb.HookPhaseRunning},
		{"failed hook Job", `failed: 3, conditions: [{type: Failed, status: "True"}]`, rspb.HookPhaseFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := hookEventsCluster(t, tt.hookPhase, tt.jobStatus)
			res, err := hooksDetail(t, c, "api")
			wantProblems(t, err, false) // warning events and failed or running hooks are not problems
			var got []string
			for _, e := range res.Events {
				got = append(got, e.Object+" "+e.Reason)
			}
			want := []string{"Job/migrate BackoffLimitExceeded", "Pod/migrate-aaa BackOff"}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("events = %q, want %q", got, want)
			}
			if res.EventsError != "" {
				t.Errorf("eventsError = %q", res.EventsError)
			}

			out, err := run(t, c, "prod", "hooks", "api")
			wantProblems(t, err, false)
			for _, s := range []string{"Warning events (last hour):", "LAST SEEN", "Job/migrate", "Job has reached the specified backoff limit", "Pod/migrate-aaa", "Back-off restarting failed container"} {
				if !strings.Contains(out, s) {
					t.Errorf("missing %q:\n%s", s, out)
				}
			}
			for _, s := range []string{"not a warning", "unrelated pod", "older than the window", "unrelated job"} {
				if strings.Contains(out, s) {
					t.Errorf("unexpected %q:\n%s", s, out)
				}
			}
		})
	}
}

func TestHooksWarningEventsOfPodHook(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.extraKinds = []kindInfo{podKind}
	c.release("api", "prod", 1, common.StatusDeployed, "", podHook("check", rspb.HookPhaseRunning))
	c.pod("check", "check", "Pending", "ImagePullBackOff", 0)
	c.event("e1", "Warning", "Pod", "check", "Failed", "Failed to pull image", "2026-10-18T11:58:00Z", 3)
	c.event("e2", "Warning", "Pod", "other", "Failed", "unrelated pod", "2026-10-18T11:58:00Z", 3)
	res, err := hooksDetail(t, c, "api")
	wantProblems(t, err, false)
	if len(res.Events) != 1 || res.Events[0].Object != "Pod/check" || res.Events[0].Message != "Failed to pull image" {
		t.Fatalf("events = %+v", res.Events)
	}
}

func TestHooksNoWarningEventsSection(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, "", jobHook("migrate", rspb.HookPhaseRunning, 0))
	c.yaml(jobDoc("migrate", "", "active: 1"))
	out, err := run(t, c, "prod", "hooks", "api")
	wantProblems(t, err, false)
	if strings.Contains(out, "Warning events") {
		t.Fatalf("no events, no section:\n%s", out)
	}
}

// hookLogsCluster has running hook Job migrate with pods: one with two started containers, one whose
// container restarted and waits, one that never started; and running Pod hook check.
func hookLogsCluster(t *testing.T) *cluster {
	c := newCluster(t)
	c.extraKinds = []kindInfo{podKind}
	c.release("api", "prod", 1, common.StatusDeployed, "",
		jobHook("migrate", rspb.HookPhaseRunning, 0), podHook("check", rspb.HookPhaseRunning))
	c.yaml(jobDoc("migrate", "backoffLimit: 6", "active: 2, failed: 1"))
	c.podWithStatus("migrate-aaa", "migrate", `  phase: Running
  containerStatuses:
  - {name: main, ready: true, restartCount: 0, state: {running: {startedAt: "2026-10-18T11:00:00Z"}}}
  - {name: proxy, ready: true, restartCount: 0, state: {running: {startedAt: "2026-10-18T11:00:00Z"}}}
`)
	c.pod("migrate-bbb", "migrate", "Running", "CrashLoopBackOff", 3)
	c.pod("migrate-ccc", "migrate", "Pending", "ContainerCreating", 0)
	c.pod("check", "check", "Running", "", 0)
	c.pod("other-0", "other", "Running", "", 0)
	c.setLog("migrate-aaa/main", "applying 0042\n")
	c.setLog("migrate-aaa/proxy", "proxy ready\n")
	c.setLog("migrate-bbb/main!previous", "panic: lock timeout\n")
	c.setLog("check/main", "checking\n")
	c.setLog("other-0/main", "unrelated\n")
	return c
}

func TestHooksLogs(t *testing.T) {
	t.Parallel()
	c := hookLogsCluster(t)
	res, err := hooksDetail(t, c, "api", "--logs", "3")
	wantProblems(t, err, false)

	wantTails := map[string]int64{"migrate-aaa/main": 3, "migrate-aaa/proxy": 3, "migrate-bbb/main!previous": 3, "check/main": 3}
	if len(c.logTails) != len(wantTails) {
		t.Errorf("log requests = %v, want %v", c.logCalls, wantTails)
	}
	for key, n := range wantTails {
		if got, ok := c.logTails[key]; !ok || got != n {
			t.Errorf("log %s: tail %d (requested %t), want %d", key, got, ok, n)
		}
	}
	for _, call := range c.logCalls {
		if !strings.HasPrefix(call, "prod/") {
			t.Errorf("log request %q outside the hook namespace", call)
		}
	}

	logs := map[string][]string{}
	for _, h := range res.Hooks {
		for _, l := range h.Logs {
			key := l.Pod + "/" + l.Container
			if l.Previous {
				key += "!previous"
			}
			logs[h.Name] = append(logs[h.Name], key+"="+strings.Join(l.Lines, ";"))
		}
	}
	slices.Sort(logs["migrate"])
	if got, want := strings.Join(logs["migrate"], "|"), "migrate-aaa/main=applying 0042|migrate-aaa/proxy=proxy ready|migrate-bbb/main!previous=panic: lock timeout"; got != want {
		t.Errorf("migrate logs = %q, want %q", got, want)
	}
	if got, want := strings.Join(logs["check"], "|"), "check/main=checking"; got != want {
		t.Errorf("check logs = %q, want %q", got, want)
	}

	out, err := run(t, c, "prod", "hooks", "api", "--logs", "3")
	wantProblems(t, err, false)
	for _, s := range []string{
		"Logs of migrate-aaa (container main):", "applying 0042",
		"Logs of migrate-aaa (container proxy):", "proxy ready",
		"Logs of migrate-bbb (container main, previous run):", "panic: lock timeout",
		"Logs of check (container main):", "checking",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "migrate-ccc") || strings.Contains(out, "unrelated") {
		t.Errorf("logs of a container that never started or of an unrelated pod:\n%s", out)
	}
}

func TestHooksLogsOffByDefault(t *testing.T) {
	t.Parallel()
	c := hookLogsCluster(t)
	out, err := run(t, c, "prod", "hooks", "api")
	wantProblems(t, err, false)
	if len(c.logCalls) != 0 || strings.Contains(out, "Logs of") {
		t.Fatalf("logs requested without --logs: %v\n%s", c.logCalls, out)
	}
}

func TestHooksLogsOutOfRange(t *testing.T) {
	t.Parallel()
	for _, n := range []string{"-1", "1001"} {
		t.Run(n, func(t *testing.T) {
			t.Parallel()
			_, err := run(t, hookLogsCluster(t), "prod", "hooks", "api", "--logs", n)
			if err == nil || !strings.Contains(err.Error(), "--logs must be between 0 and 1000") {
				t.Fatalf("want a --logs range error, got %v", err)
			}
		})
	}
}

func TestHooksAbsentReason(t *testing.T) {
	t.Parallel()
	const (
		bySucceeded = "deleted by its hook-succeeded policy after the hook succeeded"
		byFailed    = "deleted by its hook-failed policy after the hook failed"
		notRun      = "the hook has not run"
		beforeHook  = "not explained by its delete policy: before-hook-creation deletes it only when the hook runs again; a later run or someone else removed it"
		unexplained = "not explained by its delete policy"
	)
	tests := []struct {
		name     string
		phase    rspb.HookPhase
		policies []rspb.HookDeletePolicy
		want     string
	}{
		{"succeeded with hook-succeeded", rspb.HookPhaseSucceeded, []rspb.HookDeletePolicy{rspb.HookSucceeded}, bySucceeded},
		{"succeeded with hook-succeeded and before-hook-creation", rspb.HookPhaseSucceeded, []rspb.HookDeletePolicy{rspb.HookBeforeHookCreation, rspb.HookSucceeded}, bySucceeded},
		{"failed with hook-failed", rspb.HookPhaseFailed, []rspb.HookDeletePolicy{rspb.HookFailed}, byFailed},
		{"never ran", "", []rspb.HookDeletePolicy{rspb.HookSucceeded}, notRun},
		{"never ran without policy", "", nil, notRun},
		{"succeeded with before-hook-creation", rspb.HookPhaseSucceeded, []rspb.HookDeletePolicy{rspb.HookBeforeHookCreation}, beforeHook},
		{"succeeded without policy defaults to before-hook-creation", rspb.HookPhaseSucceeded, nil, beforeHook},
		{"running without policy", rspb.HookPhaseRunning, nil, beforeHook},
		{"failed with hook-succeeded only", rspb.HookPhaseFailed, []rspb.HookDeletePolicy{rspb.HookSucceeded}, unexplained},
		{"succeeded with hook-failed only", rspb.HookPhaseSucceeded, []rspb.HookDeletePolicy{rspb.HookFailed}, unexplained},
		{"running with hook-succeeded", rspb.HookPhaseRunning, []rspb.HookDeletePolicy{rspb.HookSucceeded}, unexplained},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := jobHook("migrate", tt.phase, 0, tt.policies...)
			if tt.phase == "" {
				h.LastRun = rspb.HookExecution{}
			}
			c := newCluster(t)
			c.release("api", "prod", 1, common.StatusDeployed, "", h)

			res, err := hooksDetail(t, c, "api")
			wantProblems(t, err, false) // absent objects are not problems
			if len(res.Hooks) != 1 || res.Hooks[0].Status != "absent" || res.Hooks[0].AbsentReason != tt.want {
				t.Fatalf("hooks = %+v, want absentReason %q", res.Hooks, tt.want)
			}

			out, err := run(t, c, "prod", "hooks", "api")
			wantProblems(t, err, false)
			// DETAILS is the last column; a suffix match tells the short reason from the longer one it prefixes.
			if line := hookLine(t, out, "migrate"); !strings.HasSuffix(strings.TrimRight(line, " "), " "+tt.want) {
				t.Errorf("table line %q, want details %q", line, tt.want)
			}
		})
	}
}

func TestHooksPresentObjectHasNoAbsentReason(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, "", jobHook("migrate", rspb.HookPhaseSucceeded, 0))
	c.yaml(jobDoc("migrate", "", `succeeded: 1, conditions: [{type: Complete, status: "True"}]`))
	res, err := hooksDetail(t, c, "api")
	wantProblems(t, err, false)
	if len(res.Hooks) != 1 || res.Hooks[0].AbsentReason != "" {
		t.Fatalf("hooks = %+v", res.Hooks)
	}
}

func TestHooksWarningEventsOfAbsentHook(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, kind, object string
		hook               *rspb.Hook
	}{
		// hook-failed deleted the Job after it failed; its BackoffLimitExceeded event outlives it.
		{"absent Job", "Job", "migrate", jobHook("migrate", rspb.HookPhaseFailed, 0, rspb.HookFailed)},
		{"absent Pod", "Pod", "check", podHook("check", rspb.HookPhaseFailed, rspb.HookFailed)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newCluster(t)
			c.extraKinds = []kindInfo{podKind}
			c.release("api", "prod", 1, common.StatusFailed, "", tt.hook)
			c.event("e1", "Warning", tt.kind, tt.object, "BackoffLimitExceeded", "hook object failed", "2026-10-18T11:58:00Z", 1)
			c.event("e2", "Warning", tt.kind, "other", "BackoffLimitExceeded", "unrelated object", "2026-10-18T11:59:00Z", 1)
			c.event("e3", "Warning", tt.kind, tt.object, "BackoffLimitExceeded", "older than the window", "2026-10-18T10:30:00Z", 1)
			c.event("e4", "Normal", tt.kind, tt.object, "Created", "not a warning", "2026-10-18T11:59:00Z", 1)

			res, err := hooksDetail(t, c, "api")
			wantProblems(t, err, false) // an absent hook object deleted by its policy is not a problem
			if len(res.Hooks) != 1 || res.Hooks[0].Status != "absent" {
				t.Fatalf("hooks = %+v", res.Hooks)
			}
			want := tt.kind + "/" + tt.object
			if len(res.Events) != 1 || res.Events[0].Object != want || res.Events[0].Message != "hook object failed" {
				t.Errorf("events = %+v, want only the warning event of %s", res.Events, want)
			}

			out, err := run(t, c, "prod", "hooks", "api")
			wantProblems(t, err, false)
			for _, s := range []string{"Warning events (last hour):", want, "hook object failed"} {
				if !strings.Contains(out, s) {
					t.Errorf("missing %q:\n%s", s, out)
				}
			}
			for _, s := range []string{"unrelated object", "older than the window", "not a warning"} {
				if strings.Contains(out, s) {
					t.Errorf("unexpected %q:\n%s", s, out)
				}
			}
		})
	}
}

// runPodListFails is run with a fake API server that fails every list of pods.
func runPodListFails(t *testing.T, c *cluster, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut, o.Now = &out, &errOut, func() time.Time { return now }
	o.NewClients = func(*Options) (*Clients, error) {
		cl := c.clients("prod")
		cl.Cluster.Dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("pods are forbidden")
		})
		return cl, nil
	}
	cmd := NewRootCommand(o)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestHooksJobPodListErrorKeepsRunningState(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, "", jobHook("migrate", rspb.HookPhaseRunning, 0))
	c.yaml(jobDoc("migrate", "backoffLimit: 6", "active: 1, failed: 2"))
	c.event("e1", "Warning", "Job", "migrate", "BackoffLimitExceeded", "Job has reached the specified backoff limit", "2026-10-18T11:58:00Z", 1)

	out, err := runPodListFails(t, c, "hooks", "api", "-o", "json")
	wantProblems(t, err, true) // a failed lookup is a problem
	var res hooksDetailJSON
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("bad json: %v\n%s", jerr, out)
	}
	if len(res.Hooks) != 1 {
		t.Fatalf("hooks = %+v", res.Hooks)
	}
	r := res.Hooks[0]
	if r.Status != "running" || r.Active != 1 || r.Failed != 2 || !strings.Contains(r.Error, "listing pods") {
		t.Errorf("row = %+v, want status running, active 1, failed 2 and a listing pods error", r)
	}
	if len(res.Events) != 1 || res.Events[0].Object != "Job/migrate" {
		t.Errorf("events = %+v, want the warning event of Job/migrate", res.Events)
	}

	out, err = runPodListFails(t, c, "hooks", "api")
	wantProblems(t, err, true)
	if line := hookLine(t, out, "migrate"); !strings.Contains(line, " running ") || !strings.Contains(line, "active 1, failed 2") {
		t.Errorf("table line %q, want status running and details active 1, failed 2", line)
	}
	for _, s := range []string{"listing pods", "Warning events (last hour):", "Job has reached the specified backoff limit"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q:\n%s", s, out)
		}
	}
}
