package cli

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
)

// logTarget is the container whose log explains why a pod needs attention.
type logTarget struct {
	pod, container string
	previous       bool
	lines          int // how many lines to read

	id       string // container ID and restart count identify the run whose log is read
	restarts int32
}

// logTargetsOf lists the containers whose logs to read for a pod. The container that explains a problem
// gets --logs lines (or --all-logs when --logs is off); with --all-logs every other started container
// gets --all-logs lines.
func logTargetsOf(p corev1.Pod, problem *podHealth, workloadUnhealthy bool, opts healthOptions) []logTarget {
	var targets []logTarget
	if t, ok := logTargetOf(p, problem, workloadUnhealthy); ok {
		t.lines = cmp.Or(opts.logLines, opts.allLogs)
		targets = append(targets, t)
	}
	if opts.allLogs > 0 {
		for _, cs := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
			if cs.State.Waiting != nil && cs.RestartCount == 0 {
				continue // never started: no log
			}
			if !slices.ContainsFunc(targets, func(t logTarget) bool { return t.container == cs.Name }) {
				targets = append(targets, logTarget{pod: p.Name, container: cs.Name, previous: cs.RestartCount > 0 && cs.State.Waiting != nil, lines: opts.allLogs, id: cs.ContainerID, restarts: cs.RestartCount})
			}
		}
	}
	return slices.DeleteFunc(targets, func(t logTarget) bool { return t.lines == 0 })
}

// logTargetOf picks the container to read logs from: the failing container of a problem pod, or the
// first container that is not ready in a not-ready pod of an unhealthy workload.
func logTargetOf(p corev1.Pod, problem *podHealth, workloadUnhealthy bool) (logTarget, bool) {
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	var cs *corev1.ContainerStatus
	switch {
	case problem != nil:
		if noLogs[problem.Reason] {
			return logTarget{}, false
		}
		for i := range statuses {
			if w := statuses[i].State.Waiting; w != nil && w.Reason == problem.Reason {
				cs = &statuses[i]
				break
			}
		}
		if cs == nil && len(statuses) > 0 {
			cs = &statuses[0]
		}
	case workloadUnhealthy && !podReady(p):
		for i := range statuses {
			if !statuses[i].Ready {
				cs = &statuses[i]
				break
			}
		}
	}
	if cs == nil || cs.State.Waiting != nil && cs.RestartCount == 0 {
		return logTarget{}, false // no container, or one that never started and has no log
	}
	return logTarget{pod: p.Name, container: cs.Name, previous: cs.RestartCount > 0 && cs.State.Waiting != nil, id: cs.ContainerID, restarts: cs.RestartCount}, true
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// readLogs reads the targets one by one: workloads are already read in parallel, so this keeps the
// number of simultaneous log requests within --concurrency.
func readLogs(cmd *cobra.Command, c *Clients, namespace string, targets []logTarget) []podLog {
	slices.SortFunc(targets, func(a, b logTarget) int {
		return cmp.Or(strings.Compare(a.pod, b.pod), strings.Compare(a.container, b.container))
	})
	logs := make([]podLog, len(targets))
	for i, t := range targets {
		logs[i] = podLog{Pod: t.pod, Container: t.container, Previous: t.previous, id: t.id, restarts: t.restarts}
		text, err := c.Logs(cmd.Context(), namespace, t.pod, t.container, int64(t.lines), t.previous)
		if err != nil {
			logs[i].Error = cleanLine(err.Error())
			continue
		}
		logs[i].Lines = sanitizeLog(text)
	}
	return logs
}

// logKey identifies one stream of log lines: a container run of a pod. A restart starts a new stream,
// even when the log of the crashed run reads exactly like the one before.
type logKey struct {
	namespace, pod, container string
	previous                  bool
	id                        string
	restarts                  int32
}

// logMemory remembers which log lines were already printed, for --tail.
type logMemory struct {
	lines map[logKey][]string
	errs  map[logKey]string
}

func newLogMemory() *logMemory {
	return &logMemory{lines: map[logKey][]string{}, errs: map[logKey]string{}}
}

// news returns a copy of res whose logs hold only what was not printed before, and remembers what it
// returned. A container that printed nothing new, or failed with the same error again, is left out.
// Streams that are absent from res are forgotten, so a pod that comes back is shown in full.
func (m *logMemory) news(res *healthResult) *healthResult {
	out := *res
	out.Workloads = make([]workloadHealth, len(res.Workloads))
	present := map[logKey]bool{}
	for i, w := range res.Workloads {
		var fresh []podLog
		for _, l := range w.Logs {
			key := logKey{w.Namespace, l.Pod, l.Container, l.Previous, l.id, l.restarts}
			present[key] = true
			if l.Error != "" {
				if m.errs[key] != l.Error {
					m.errs[key] = l.Error
					fresh = append(fresh, l)
				}
				continue
			}
			delete(m.errs, key)
			prev, seen := m.lines[key]
			m.lines[key] = l.Lines
			if !seen {
				fresh = append(fresh, l)
				continue
			}
			if l.Lines = newLines(prev, l.Lines); len(l.Lines) > 0 {
				l.more = true
				fresh = append(fresh, l)
			}
		}
		w.Logs = fresh
		out.Workloads[i] = w
	}
	for key := range m.lines {
		if !present[key] {
			delete(m.lines, key)
		}
	}
	for key := range m.errs {
		if !present[key] {
			delete(m.errs, key)
		}
	}
	return &out
}

// newLines returns the lines of cur that follow the longest tail of prev that cur starts with: the
// window of a log moves forward, so the end of the old window is the start of the new one.
func newLines(prev, cur []string) []string {
	for k := min(len(prev), len(cur)); k > 0; k-- {
		if slices.Equal(prev[len(prev)-k:], cur[:k]) {
			return cur[k:]
		}
	}
	return cur
}

func withoutLogs(res *healthResult) *healthResult {
	out := *res
	out.Workloads = make([]workloadHealth, len(res.Workloads))
	for i, w := range res.Workloads {
		w.Logs = nil
		out.Workloads[i] = w
	}
	return &out
}

func hasLogs(res *healthResult) bool {
	for _, w := range res.Workloads {
		if len(w.Logs) > 0 {
			return true
		}
	}
	return false
}

// printLogs prints the log tail of every container in res; lines that continue a log already shown are
// labelled as new.
func printLogs(o *Options, w io.Writer, res *healthResult) {
	for _, wl := range res.Workloads {
		for _, l := range wl.Logs {
			run := ""
			if l.Previous {
				run = ", previous run"
			}
			if l.more {
				run += ", new lines"
			}
			fmt.Fprintf(w, "\nLogs of %s (container %s%s):\n", l.Pod, l.Container, run)
			switch {
			case l.Error != "":
				fmt.Fprintf(w, "  %s unavailable: %s\n", o.paint("warning:", yellow), l.Error)
			case len(l.Lines) == 0:
				fmt.Fprintln(w, "  (no output)")
			}
			for _, line := range l.Lines {
				fmt.Fprintln(w, "  "+line)
			}
		}
	}

}
