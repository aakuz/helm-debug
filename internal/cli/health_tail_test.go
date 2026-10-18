package cli

import (
	"encoding/json"
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
	if strings.Count(r.out, "starting") != 1 || strings.Count(r.out, "Release prod/web") != 1 ||
		strings.Count(r.out, "Logs of") != 1 || strings.Count(r.out, "] unhealthy") != 1 {
		t.Fatalf("an unchanged state and log must print once:\n%s", r.out)
	}
}

func TestHealthWaitTailStructuredOutputKeepsFullLogs(t *testing.T) {
	c := recoveringCluster(t)
	r := runWait(t, c, nil, "health", "web", "--wait", "--logs", "2", "--tail", "--timeout", "2s", "-o", "json")
	wantProblems(t, r.err, true)
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.out), &doc); err != nil {
		t.Fatalf("want a single JSON document: %v\n%s", err, r.out)
	}
	if !strings.Contains(r.out, `"starting"`) || !strings.Contains(r.out, `"panic: no database"`) || strings.Contains(r.out, "new lines") {
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
		{"overlap of a single line", []string{"a", "b"}, []string{"b", "c"}, []string{"c"}},
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

func TestHealthWaitTailWithAllLogsOnly(t *testing.T) {
	c := recoveringCluster(t)
	c.readyPod("web-ok", "web")
	c.setLog("web-ok/main", "ok line\n")
	r := runWait(t, c, func(n int, _ *Clients) {
		if n == 1 {
			c.setLog("web-ok/main", "ok line\nok line 2\n")
		}
	}, "health", "web", "--wait", "--all-logs", "5", "--tail", "--timeout", "2s", "--interval", "2s")
	wantProblems(t, r.err, true)
	if !strings.Contains(r.out, "Logs of web-ok (container main):") || !strings.Contains(r.out, "Logs of web-ok (container main, new lines):") ||
		strings.Count(r.out, "ok line\n") != 1 || strings.Count(r.out, "ok line 2") != 1 {
		t.Fatalf("--tail works with --all-logs alone:\n%s", r.out)
	}
}

func TestHealthWaitTailShowsAReadErrorOncePerStreak(t *testing.T) {
	c := recoveringCluster(t)
	delete(c.logs, "web-aaa/main!previous")
	c.logErrors = map[string]string{"web-aaa/main!previous": "pod is not available"}
	r := runWait(t, c, func(n int, _ *Clients) {
		switch n {
		case 2: // reads work again
			delete(c.logErrors, "web-aaa/main!previous")
			c.setLog("web-aaa/main!previous", "back\n")
		case 4: // and fail again: a new streak is reported
			delete(c.logs, "web-aaa/main!previous")
			c.logErrors["web-aaa/main!previous"] = "pod is not available"
		}
	}, "health", "web", "--wait", "--logs", "5", "--tail", "--timeout", "10s", "--interval", "2s")
	wantProblems(t, r.err, true)
	if got := strings.Count(r.out, "unavailable: pod is not available"); got != 2 {
		t.Fatalf("want the error once per streak (2), got %d:\n%s", got, r.out)
	}
	if strings.Count(r.out, "back") != 1 {
		t.Fatalf("the recovered log must be shown once:\n%s", r.out)
	}
}

func TestHealthWaitTailShowsTheSameCrashAgain(t *testing.T) {
	// a deterministic crash loop writes the same log every time: a new restart is new output
	c := recoveringCluster(t)
	r := runWait(t, c, func(n int, cl *Clients) {
		if n == 1 {
			setPod(t, cl, podDoc("web-aaa", "web", "Running", "CrashLoopBackOff", 8))
		}
	}, "health", "web", "--wait", "--logs", "5", "--tail", "--timeout", "4s", "--interval", "2s")
	wantProblems(t, r.err, true)
	if got := strings.Count(r.out, "panic: no database"); got != 2 {
		t.Fatalf("want the log of the new crash (2 prints), got %d:\n%s", got, r.out)
	}
	if strings.Contains(r.out, "new lines") {
		t.Fatalf("a restarted container is a new stream, not new lines of the old one:\n%s", r.out)
	}
}

func TestHealthWaitTailLabelsOnlyContinuedStreamsAsNewLines(t *testing.T) {
	c := recoveringCluster(t)
	c.setLog("web-aaa/main", "now running\n")
	r := runWait(t, c, func(n int, cl *Clients) {
		if n == 1 { // the container came back: the previous-run key becomes a current-run one
			setPod(t, cl, podDoc("web-aaa", "web", "Running", "", 8))
		}
	}, "health", "web", "--wait", "--logs", "5", "--tail", "--timeout", "2s", "--interval", "2s")
	wantProblems(t, r.err, true)
	if strings.Contains(r.out, "new lines") || !strings.Contains(r.out, "now running") {
		t.Fatalf("a stream seen for the first time is printed in full without the new-lines label:\n%s", r.out)
	}
}

func TestLogMemoryForgetsContainersThatDisappear(t *testing.T) {
	res := func(lines ...string) *healthResult {
		w := workloadHealth{Namespace: "prod", Name: "web"}
		if lines != nil {
			w.Logs = []podLog{{Pod: "web-0", Container: "main", Lines: lines, id: "c1"}}
		}
		return &healthResult{Workloads: []workloadHealth{w}}
	}
	m := newLogMemory()
	m.news(res("a", "b"))
	if got := m.news(res("a", "b", "c")).Workloads[0].Logs; len(got) != 1 || !slices.Equal(got[0].Lines, []string{"c"}) || !got[0].more {
		t.Fatalf("a continued stream shows only the new line, got %+v", got)
	}
	m.news(res())                                       // the pod is gone
	got := m.news(res("a", "b", "c")).Workloads[0].Logs // a pod with the same name came back
	if len(got) != 1 || !slices.Equal(got[0].Lines, []string{"a", "b", "c"}) || got[0].more {
		t.Fatalf("a container that came back is a new stream, got %+v", got)
	}
}

func TestHealthAllLogsIncludesStartedInitContainers(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 1, 1, 1, 1, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.podWithStatus("web-1", "web", `  phase: Running
  conditions: [{type: Ready, status: "True"}]
  initContainerStatuses:
  - {name: migrate, ready: true, restartCount: 0, state: {terminated: {exitCode: 0, reason: Completed}}}
  - {name: waiting-init, ready: false, restartCount: 0, state: {waiting: {reason: PodInitializing}}}
  containerStatuses:
  - {name: main, ready: true, restartCount: 0, state: {running: {startedAt: "2026-10-18T10:00:00Z"}}}
`)
	c.setLog("web-1/migrate", "migrated\n")
	c.setLog("web-1/main", "serving\n")
	out, err := run(t, c, "prod", "health", "web", "--all-logs", "3")
	wantProblems(t, err, false)
	got := slices.Clone(c.logCalls)
	slices.Sort(got)
	if want := []string{"prod/web-1/main", "prod/web-1/migrate"}; !slices.Equal(got, want) {
		t.Fatalf("log requests = %v, want %v (the init container that never started has no log)", got, want)
	}
	if !strings.Contains(out, "migrated") || !strings.Contains(out, "serving") {
		t.Fatalf("want init and app logs:\n%s", out)
	}
}

func TestHealthLogsAndAllLogsUnderWaitTail(t *testing.T) {
	c := recoveringCluster(t)
	c.readyPod("web-ok", "web")
	c.setLog("web-ok/main", "fine\n")
	r := runWait(t, c, nil, "health", "web", "--wait", "--logs", "1", "--all-logs", "3", "--tail", "--timeout", "2s")
	wantProblems(t, r.err, true)
	if c.logTails["web-aaa/main!previous"] != 1 || c.logTails["web-ok/main"] != 3 {
		t.Fatalf("lines asked for = %v, want 1 for the crashing pod and 3 for the healthy one", c.logTails)
	}
}

func TestHealthLogsValidationNamesTheFirstInvalidFlag(t *testing.T) {
	for i := 0; i < 20; i++ { // map iteration order must not decide the message
		_, err := run(t, logsCluster(t), "prod", "health", "web", "--logs", "2000", "--all-logs", "2000")
		if err == nil || !strings.Contains(err.Error(), "--logs ") {
			t.Fatalf("want the --logs error first, got %v", err)
		}
	}
}
