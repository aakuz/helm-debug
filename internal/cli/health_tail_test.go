package cli

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestHealthWaitShorthand(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 1, 1, 1, 1, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	r := runWait(t, c, nil, "health", "web", "-w")
	wantProblems(t, r.err, false)
	if !strings.Contains(r.out, "] healthy") || len(r.sleeps) != 0 {
		t.Fatalf("-w must behave like --wait:\n%s", r.out)
	}
}

// containers adds a ready pod with the given running containers.
func (c *cluster) containers(name, app string, names ...string) {
	c.t.Helper()
	var list string
	for _, n := range names {
		list += "\n  - {name: " + n + ", ready: true, restartCount: 0, state: {running: {startedAt: \"2026-10-18T10:00:00Z\"}}}"
	}
	c.podWithStatus(name, app, "  phase: Running\n  conditions: [{type: Ready, status: \"True\"}]\n  containerStatuses:"+list+"\n")
}

func TestHealthAllLogs(t *testing.T) {
	c := logsCluster(t)
	c.setLog("web-ccc/main", "healthy replica log\n")
	out, err := run(t, c, "prod", "health", "web", "--all-logs", "2")
	wantProblems(t, err, true)
	golden(t, out)
	got := slices.Clone(c.logCalls)
	slices.Sort(got)
	want := []string{"prod/web-aaa/main!previous", "prod/web-ccc/main", "prod/web-ddd/main"}
	if !slices.Equal(got, want) {
		t.Fatalf("log requests = %v, want %v (never-started web-bbb must be skipped)", got, want)
	}
	if c.logTail != 2 {
		t.Errorf("tail = %d, want 2", c.logTail)
	}
}

func TestHealthAllLogsHealthyRelease(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 1, 1, 1, 1, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.containers("web-1", "web", "app", "sidecar")
	c.setLog("web-1/app", "app line\n")
	c.setLog("web-1/sidecar", "sidecar line\n")
	out, err := run(t, c, "prod", "health", "web", "--all-logs", "5")
	wantProblems(t, err, false)
	if !strings.Contains(out, "Logs of web-1 (container app)") || !strings.Contains(out, "Logs of web-1 (container sidecar)") ||
		!strings.Contains(out, "app line") || !strings.Contains(out, "sidecar line") {
		t.Fatalf("want the logs of every container of every pod:\n%s", out)
	}
	if _, err := run(t, c, "prod", "health", "web", "--logs", "5"); err != nil || len(c.logCalls) != 2 {
		t.Fatalf("--logs must not read healthy pods (calls %v, err %v)", c.logCalls, err)
	}
}

func TestHealthLogsAndAllLogsAreIndependent(t *testing.T) {
	// web-aaa (crashing) and web-ddd (not ready) need attention; web-ccc is a healthy replica
	tests := []struct {
		name string
		args []string
		want map[string]int64 // log key -> lines asked for
	}{
		{"both flags", []string{"--logs", "2", "--all-logs", "5"}, map[string]int64{"web-aaa/main!previous": 2, "web-ddd/main": 2, "web-ccc/main": 5}},
		{"logs larger than all-logs", []string{"--logs", "9", "--all-logs", "1"}, map[string]int64{"web-aaa/main!previous": 9, "web-ddd/main": 9, "web-ccc/main": 1}},
		{"all-logs alone covers every pod", []string{"--all-logs", "4"}, map[string]int64{"web-aaa/main!previous": 4, "web-ddd/main": 4, "web-ccc/main": 4}},
		{"logs alone keeps to pods that need attention", []string{"--logs", "3"}, map[string]int64{"web-aaa/main!previous": 3, "web-ddd/main": 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := logsCluster(t)
			c.setLog("web-ccc/main", "healthy replica log\n")
			_, err := run(t, c, "prod", append([]string{"health", "web"}, tt.args...)...)
			wantProblems(t, err, true)
			if len(c.logTails) != len(tt.want) {
				t.Fatalf("requests = %v, want %v", c.logTails, tt.want)
			}
			for key, lines := range tt.want {
				if c.logTails[key] != lines {
					t.Errorf("%s: asked for %d lines, want %d (all: %v)", key, c.logTails[key], lines, c.logTails)
				}
			}
		})
	}
}

func TestHealthLogsFlagsValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"all-logs too large", []string{"--all-logs", "1001"}, "1000"},
		{"all-logs negative", []string{"--all-logs", "-1"}, "--all-logs"},
		{"tail without wait", []string{"--logs", "3", "--tail"}, "--tail requires --wait"},
		{"tail without a logs flag", []string{"--wait", "--tail"}, "--logs or --all-logs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := run(t, logsCluster(t), "prod", append([]string{"health", "web"}, tt.args...)...)
			if err == nil || errors.Is(err, ErrProblems) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want an error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestHealthWaitTail(t *testing.T) {
	c := recoveringCluster(t)
	r := runWait(t, c, func(n int, cl *Clients) {
		switch n {
		case 1: // only the log grows: print just the new line
			c.setLog("web-aaa/main!previous", "starting\npanic: no database\nretry 1\n")
		case 2: // the state changes and the log grows: snapshot plus the new line
			setDeployment(t, cl, deploymentDoc("web", 3, 3, 3, 3, 2, 2))
			c.setLog("web-aaa/main!previous", "starting\npanic: no database\nretry 1\nretry 2\n")
		case 3: // healed
			setDeployment(t, cl, deploymentDoc("web", 3, 3, 3, 3, 3, 3))
			deletePod(t, cl, "web-aaa")
		}
	}, "health", "web", "--wait", "--logs", "10", "--tail")
	wantProblems(t, r.err, false)
	golden(t, r.out)
	if strings.Count(r.out, "panic: no database") != 1 {
		t.Errorf("a line that was already shown must not be printed again:\n%s", r.out)
	}
}

func TestHealthWaitTailUnchangedLogsPrintNothing(t *testing.T) {
	c := recoveringCluster(t)
	r := runWait(t, c, nil, "health", "web", "--wait", "--logs", "2", "--tail", "--timeout", "4s", "--interval", "2s")
	wantProblems(t, r.err, true)
	if strings.Count(r.out, "starting") != 1 || strings.Count(r.out, "Release prod/web") != 1 {
		t.Fatalf("an unchanged state and log must print once:\n%s", r.out)
	}
}

func TestHealthWaitTailStructuredOutputKeepsFullLogs(t *testing.T) {
	c := recoveringCluster(t)
	r := runWait(t, c, nil, "health", "web", "--wait", "--logs", "2", "--tail", "--timeout", "2s", "-o", "json")
	wantProblems(t, r.err, true)
	if !strings.Contains(r.out, `"starting"`) || !strings.Contains(r.out, `"panic: no database"`) {
		t.Fatalf("json output carries the whole tail:\n%s", r.out)
	}
}

func TestHealthWaitWithoutTailRepeatsLogs(t *testing.T) {
	c := recoveringCluster(t)
	r := runWait(t, c, func(n int, _ *Clients) {
		if n == 1 {
			c.setLog("web-aaa/main!previous", "starting\npanic: no database\nretry 1\n")
		}
	}, "health", "web", "--wait", "--logs", "10", "--timeout", "2s")
	wantProblems(t, r.err, true)
	if strings.Count(r.out, "panic: no database") != 2 {
		t.Fatalf("without --tail every update repeats the whole tail:\n%s", r.out)
	}
}

func TestNewLines(t *testing.T) {
	tests := []struct {
		name      string
		prev, cur []string
		want      []string
	}{
		{"first read", nil, []string{"a", "b"}, []string{"a", "b"}},
		{"nothing new", []string{"a", "b"}, []string{"a", "b"}, nil},
		{"appended", []string{"a", "b"}, []string{"a", "b", "c"}, []string{"c"}},
		{"sliding window", []string{"a", "b", "c"}, []string{"b", "c", "d", "e"}, []string{"d", "e"}},
		{"no overlap prints everything", []string{"a", "b"}, []string{"x", "y"}, []string{"x", "y"}},
		{"log restarted", []string{"a", "b", "c"}, []string{"a"}, []string{"a"}},
		{"empty now", []string{"a"}, nil, nil},
		{"repeated lines", []string{"x", "x"}, []string{"x", "x", "x"}, []string{"x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newLines(tt.prev, tt.cur); !slices.Equal(got, tt.want) {
				t.Fatalf("newLines(%q, %q) = %q, want %q", tt.prev, tt.cur, got, tt.want)
			}
		})
	}
}
