package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
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
	c.object("batch/v1", "Job", "prod", "j", "", "")
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
