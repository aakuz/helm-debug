package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// readLog is the fake pod log reader; it records every request.
func (c *cluster) readLog(_ context.Context, ns, pod, container string, tail int64, previous bool) (string, error) {
	key := pod + "/" + container
	if previous {
		key += "!previous"
	}
	c.logMu.Lock()
	defer c.logMu.Unlock()
	c.logCalls = append(c.logCalls, ns+"/"+key)
	c.logTail = tail
	if msg, ok := c.logErrors[key]; ok {
		return "", errors.New(msg)
	}
	text, ok := c.logs[key]
	if !ok {
		return "", errors.New("no logs for " + key)
	}
	return text, nil
}

func (c *cluster) setLog(key, text string) {
	if c.logs == nil {
		c.logs = map[string]string{}
	}
	c.logs[key] = text
}

// readyPod adds a running pod that passed its readiness check.
func (c *cluster) readyPod(name, app string) {
	c.t.Helper()
	c.yaml(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s, namespace: prod, labels: {app: %s}}
status:
  phase: Running
  conditions: [{type: Ready, status: "True"}]
  containerStatuses:
  - {name: main, ready: true, restartCount: 0, state: {running: {startedAt: "2026-10-18T10:00:00Z"}}}
`, name, app))
}

// notReadyPod adds a running pod whose container fails its readiness check.
func (c *cluster) notReadyPod(name, app string) {
	c.t.Helper()
	c.yaml(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s, namespace: prod, labels: {app: %s}}
status:
  phase: Running
  conditions: [{type: Ready, status: "False"}]
  containerStatuses:
  - {name: main, ready: false, restartCount: 0, state: {running: {startedAt: "2026-10-18T10:00:00Z"}}}
`, name, app))
}

func logsCluster(t *testing.T) *cluster {
	c := healthCluster(t)
	c.deployment("web", 3, 3, 3, 3, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.pod("web-aaa", "web", "Running", "CrashLoopBackOff", 7) // crashed: previous run's logs
	c.pod("web-bbb", "web", "Pending", "ImagePullBackOff", 0) // never started: no logs to read
	c.readyPod("web-ccc", "web")
	c.notReadyPod("web-ddd", "web") // readiness failing: current logs
	c.setLog("web-aaa/main!previous", "starting\npanic: no database\n")
	c.setLog("web-ddd/main", "listening on :8080\nreadiness probe failed\n")
	return c
}

func TestHealthLogs(t *testing.T) {
	c := logsCluster(t)
	out, err := run(t, c, "prod", "health", "web", "--logs", "2")
	wantProblems(t, err, true)
	golden(t, out)
	if c.logTail != 2 {
		t.Errorf("tail = %d, want 2", c.logTail)
	}
	want := map[string]bool{"prod/web-aaa/main!previous": true, "prod/web-ddd/main": true}
	if len(c.logCalls) != len(want) {
		t.Fatalf("log requests = %v, want only %v", c.logCalls, want)
	}
	for _, call := range c.logCalls {
		if !want[call] {
			t.Errorf("unexpected log request %q", call)
		}
	}
}

func TestHealthLogsOffByDefault(t *testing.T) {
	c := logsCluster(t)
	out, err := run(t, c, "prod", "health", "web")
	wantProblems(t, err, true)
	if len(c.logCalls) != 0 || strings.Contains(out, "panic") {
		t.Fatalf("logs requested without --logs: %v\n%s", c.logCalls, out)
	}
}

func TestHealthLogsJSON(t *testing.T) {
	c := logsCluster(t)
	out, err := run(t, c, "prod", "health", "web", "--logs", "5", "-o", "json")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestHealthLogsSkipsNotReadyPodsOfHealthyWorkload(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 1, 1, 1, 1, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.notReadyPod("web-extra", "web")
	c.setLog("web-extra/main", "noise\n")
	_, err := run(t, c, "prod", "health", "web", "--logs", "3")
	wantProblems(t, err, false)
	if len(c.logCalls) != 0 {
		t.Fatalf("logs requested for a healthy workload: %v", c.logCalls)
	}
}

// podWithStatus adds a pod from raw status YAML.
func (c *cluster) podWithStatus(name, app, status string) {
	c.t.Helper()
	c.yaml(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s, namespace: prod, labels: {app: %s}}
status:
%s`, name, app, status))
}

func TestHealthLogsRestartedButRunningContainerReadsCurrentLog(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 1, 1, 2, 2, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.podWithStatus("web-aaa", "web", `  phase: Running
  conditions: [{type: Ready, status: "False"}]
  containerStatuses:
  - {name: main, ready: false, restartCount: 3, state: {running: {startedAt: "2026-10-18T11:00:00Z"}}}
`)
	c.setLog("web-aaa/main", "current run\n")
	out, err := run(t, c, "prod", "health", "web", "--logs", "1")
	wantProblems(t, err, true)
	if len(c.logCalls) != 1 || c.logCalls[0] != "prod/web-aaa/main" || !strings.Contains(out, "current run") || strings.Contains(out, "previous run") {
		t.Fatalf("want the current log of a running container, got %v:\n%s", c.logCalls, out)
	}
}

func TestHealthLogsPodWithoutReadyConditionCountsAsNotReady(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 1, 1, 2, 2, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.podWithStatus("web-aaa", "web", `  phase: Running
  containerStatuses:
  - {name: main, ready: false, restartCount: 0, state: {running: {startedAt: "2026-10-18T11:00:00Z"}}}
`)
	c.setLog("web-aaa/main", "booting\n")
	_, err := run(t, c, "prod", "health", "web", "--logs", "1")
	wantProblems(t, err, true)
	if len(c.logCalls) != 1 {
		t.Fatalf("want one log request, got %v", c.logCalls)
	}
}

func TestHealthLogsUnknownReadyConditionCountsAsNotReady(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 1, 1, 2, 2, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.podWithStatus("web-aaa", "web", `  phase: Running
  conditions: [{type: Ready, status: "Unknown"}]
  containerStatuses:
  - {name: main, ready: false, restartCount: 0, state: {running: {startedAt: "2026-10-18T11:00:00Z"}}}
`)
	c.setLog("web-aaa/main", "node lost contact\n")
	_, err := run(t, c, "prod", "health", "web", "--logs", "1")
	wantProblems(t, err, true)
	if len(c.logCalls) != 1 {
		t.Fatalf("want one log request, got %v", c.logCalls)
	}
}

func TestHealthLogsSkipsContainersThatNeverStarted(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 1, 1, 2, 2, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.podWithStatus("web-creating", "web", `  phase: Pending
  conditions: [{type: Ready, status: "False"}]
  containerStatuses:
  - {name: main, ready: false, restartCount: 0, state: {waiting: {reason: ContainerCreating}}}
`)
	out, err := run(t, c, "prod", "health", "web", "--logs", "5")
	wantProblems(t, err, true)
	if len(c.logCalls) != 0 || strings.Contains(out, "Logs of") || strings.Contains(out, "unavailable") {
		t.Fatalf("a container that never started has no log; got %v:\n%s", c.logCalls, out)
	}
}

func TestHealthLogsError(t *testing.T) {
	c := logsCluster(t)
	delete(c.logs, "web-aaa/main!previous")
	out, err := run(t, c, "prod", "health", "web", "--logs", "2")
	wantProblems(t, err, true)
	if !strings.Contains(out, "no logs for web-aaa/main!previous") || !strings.Contains(out, "readiness probe failed") {
		t.Fatalf("want the error for one pod and the logs of the other:\n%s", out)
	}
}

func TestHealthLogsNegative(t *testing.T) {
	_, err := run(t, logsCluster(t), "prod", "health", "web", "--logs", "-1")
	if err == nil || !strings.Contains(err.Error(), "--logs") {
		t.Fatalf("want an error about --logs, got %v", err)
	}
}

func TestSanitizeLog(t *testing.T) {
	long := strings.Repeat("x", maxLogLine+50)
	exact := strings.Repeat("y", maxLogLine)
	oneOver := strings.Repeat("z", maxLogLine+1)
	tests := []struct {
		name, in string
		want     []string
	}{
		{"plain lines", "a\nb\n", []string{"a", "b"}},
		{"crlf and blank tail", "a\r\nb\r\n\r\n", []string{"a", "b"}},
		{"colors stripped", "\x1b[31mred\x1b[0m ok", []string{"red ok"}},
		{"osc title stripped", "\x1b]0;pwned\x07after", []string{"after"}},
		{"control characters dropped, tab kept", "a\x00b\x07c\td\x7f", []string{"abc\td"}},
		{"carriage return rewrites dropped", "progress 10%\rprogress 100%", []string{"progress 10%progress 100%"}},
		{"long line truncated", long, []string{long[:maxLogLine] + "…"}},
		{"line of exactly the limit is kept", exact, []string{exact}},
		{"one rune over the limit is cut", oneOver, []string{oneOver[:maxLogLine] + "…"}},
		{"multibyte runes counted as runes", strings.Repeat("я", maxLogLine), []string{strings.Repeat("я", maxLogLine)}},
		{"bidi override and zero width characters dropped", "a\u202eb\u200bc\u2066d", []string{"abcd"}},
		{"dcs string body removed", "x\x1bPsecret\x1b\\y", []string{"xy"}},
		{"apc string body removed", "x\x1b_apc\x1b\\y", []string{"xy"}},
		{"unterminated string body ends at the line", "a\x1bPsecret\nnext", []string{"a", "next"}},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeLog(tt.in)
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") || len(got) != len(tt.want) {
				t.Fatalf("sanitizeLog(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHealthLogsTerminatedContainerReadsCurrentLog(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 1, 1, 2, 2, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.podWithStatus("web-aaa", "web", `  phase: Running
  conditions: [{type: Ready, status: "False"}]
  containerStatuses:
  - {name: main, ready: false, restartCount: 2, state: {terminated: {exitCode: 1, reason: Error}}}
`)
	c.setLog("web-aaa/main", "just exited\n")
	out, err := run(t, c, "prod", "health", "web", "--logs", "1")
	wantProblems(t, err, true)
	if len(c.logCalls) != 1 || c.logCalls[0] != "prod/web-aaa/main" || !strings.Contains(out, "just exited") {
		t.Fatalf("want the log of the run that just exited, got %v:\n%s", c.logCalls, out)
	}
}

func TestHealthLogsLimit(t *testing.T) {
	_, err := run(t, logsCluster(t), "prod", "health", "web", "--logs", "1001")
	if err == nil || !strings.Contains(err.Error(), "1000") {
		t.Fatalf("want an error naming the limit, got %v", err)
	}
	if _, err := run(t, logsCluster(t), "prod", "health", "web", "--logs", "1000"); !errors.Is(err, ErrProblems) {
		t.Fatalf("1000 lines must be accepted, got %v", err)
	}
}

func TestHealthLogsDisabledWithoutReader(t *testing.T) {
	c := logsCluster(t)
	var out bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut = &out, &bytes.Buffer{}
	o.NewClients = func(*Options) (*Clients, error) {
		cl := c.clients("prod")
		cl.Logs = nil
		return cl, nil
	}
	cmd := NewRootCommand(o)
	cmd.SetArgs([]string{"health", "web", "--logs", "3"})
	if err := cmd.ExecuteContext(context.Background()); !errors.Is(err, ErrProblems) {
		t.Fatalf("want ErrProblems, got %v", err)
	}
	if strings.Contains(out.String(), "Logs of") {
		t.Fatalf("no reader, no logs:\n%s", out.String())
	}
}

func TestHealthLogsErrorTextIsSanitized(t *testing.T) {
	c := logsCluster(t)
	delete(c.logs, "web-aaa/main!previous")
	c.logErrors = map[string]string{"web-aaa/main!previous": "\x1b[2Jcleared \u202eevil"}
	out, err := run(t, c, "prod", "health", "web", "--logs", "1")
	wantProblems(t, err, true)
	if strings.ContainsRune(out, '\x1b') || strings.ContainsRune(out, '\u202e') || !strings.Contains(out, "cleared evil") {
		t.Fatalf("error text must be sanitized:\n%q", out)
	}
}

func TestHealthEventMessageIsSanitized(t *testing.T) {
	c := healthCluster(t)
	c.deployment("web", 1, 1, 2, 2, 1, 1)
	c.statefulSet("db", 1, 1, 1)
	c.event("e1", "Warning", "Deployment", "web", "FailedCreate", "\x1b]0;pwned\x07quota \u202eexceeded", "2026-10-18T11:55:00Z", 1)
	out, err := run(t, c, "prod", "health", "web")
	wantProblems(t, err, true)
	if strings.ContainsRune(out, '\x1b') || strings.ContainsRune(out, '\u202e') || !strings.Contains(out, "quota exceeded") {
		t.Fatalf("event message must be sanitized:\n%q", out)
	}
}

func TestCleanLine(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "quota exceeded", "quota exceeded"},
		{"newlines and tabs become single spaces", "line one\nline two\t\tend\r\n", "line one line two end"},
		{"escape sequences removed", "\x1b[31mfailed\x1b[0m: \x1b]0;title\x07reason", "failed: reason"},
		{"bidi override removed", "a‮b", "ab"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanLine(tt.in); got != tt.want {
				t.Fatalf("cleanLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
