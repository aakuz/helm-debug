package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// releaseAt stores a revision like c.release, but with explicit times: updated becomes Info.LastDeployed and created
// the createdAt label (unix seconds) that Helm sets when it creates the record. A zero created removes the label.
func (c *cluster) releaseAt(name, ns string, version int, status common.Status, updated, created time.Time) {
	c.t.Helper()
	rel := &rspb.Release{
		Name: name, Namespace: ns, Version: version, Manifest: simpleManifest,
		Info: &rspb.Info{
			Status:       status,
			LastDeployed: updated,
			Description:  map[common.Status]string{common.StatusFailed: "Upgrade \"" + name + "\" failed: timed out waiting for the condition"}[status],
		},
		Chart: &chart.Chart{Metadata: &chart.Metadata{Name: name, Version: fmt.Sprintf("1.%d.0", version), AppVersion: "2.0"}},
	}
	key := fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, version)
	if err := driver.NewSecrets(c.kube.CoreV1().Secrets(ns)).Create(key, rel); err != nil {
		c.t.Fatal(err)
	}
	c.relabel(ns, key, func(l map[string]string) {
		if created.IsZero() {
			delete(l, "createdAt")
		} else {
			l["createdAt"] = strconv.FormatInt(created.Unix(), 10)
		}
	})
}

// releaseModifiedAt stores a revision like releaseAt, but labelled the way Helm's driver leaves a record after an
// Update (every install, upgrade and rollback ends with one): a modifiedAt label set to modified and no createdAt.
func (c *cluster) releaseModifiedAt(name, ns string, version int, status common.Status, updated, modified time.Time) {
	c.t.Helper()
	c.releaseAt(name, ns, version, status, updated, time.Time{})
	c.relabel(ns, fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, version), func(l map[string]string) {
		l["modifiedAt"] = strconv.FormatInt(modified.Unix(), 10)
	})
}

// writeLabels are the two ways a record carries the time of its last write: createdAt as written by Helm's Create,
// and modifiedAt (without createdAt) as left by Helm's Update.
var writeLabels = []struct {
	name  string
	store func(c *cluster, name, ns string, version int, status common.Status, updated, written time.Time)
}{
	{"createdAt", (*cluster).releaseAt},
	{"modifiedAt", (*cluster).releaseModifiedAt},
}

// relabel edits the labels of a stored release record.
func (c *cluster) relabel(ns, object string, edit func(map[string]string)) {
	c.t.Helper()
	secrets := c.kube.CoreV1().Secrets(ns)
	s, err := secrets.Get(context.Background(), object, metav1.GetOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
	edit(s.Labels)
	if _, err := secrets.Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
		c.t.Fatal(err)
	}
}

// secretGets counts the secret GETs recorded on the fake clientset: each one is a Store.Load of a release record.
func secretGets(c *cluster) int {
	n := 0
	for _, a := range c.kube.Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == "secrets" {
			n++
		}
	}
	return n
}

type timelineJSON struct {
	DecodeErrors int `json:"decodeErrors"`
	Revisions    []struct {
		Namespace   string    `json:"namespace"`
		Release     string    `json:"release"`
		Revision    int       `json:"revision"`
		Updated     time.Time `json:"updated"`
		Status      string    `json:"status"`
		Chart       string    `json:"chart"`
		AppVersion  string    `json:"appVersion"`
		Description string    `json:"description"`
	} `json:"revisions"`
}

// timeline runs `history -o json` without RELEASE in namespace prod and returns the parsed result and the error.
func timeline(t *testing.T, c *cluster, args ...string) (timelineJSON, error) {
	t.Helper()
	out, err := run(t, c, "prod", append([]string{"history", "-o", "json"}, args...)...)
	var res timelineJSON
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("bad json (err=%v): %v\n%s", err, jerr, out)
	}
	return res, err
}

// revs renders the rows as "ns/release/revision" in output order.
func revs(res timelineJSON) string {
	var s []string
	for _, r := range res.Revisions {
		s = append(s, fmt.Sprintf("%s/%s/%d", r.Namespace, r.Release, r.Revision))
	}
	return strings.Join(s, ",")
}

// orderCluster has revisions with equal update times across namespaces, releases and revisions.
func orderCluster(t *testing.T) *cluster {
	c := newCluster(t)
	t0 := now.Add(-2 * time.Hour)
	at := func(name, ns string, v int, st common.Status, u time.Time) { c.releaseAt(name, ns, v, st, u, u) }
	at("web", "prod", 1, common.StatusSuperseded, t0.Add(-time.Hour))
	at("web", "prod", 2, common.StatusDeployed, t0)
	at("api", "prod", 1, common.StatusSuperseded, t0.Add(-time.Hour))
	at("api", "prod", 2, common.StatusDeployed, t0)
	at("web", "dev", 1, common.StatusSuperseded, t0.Add(-2*time.Hour))
	at("web", "dev", 2, common.StatusDeployed, t0)
	at("api", "dev", 1, common.StatusDeployed, t0.Add(-time.Hour))
	at("db", "prod", 3, common.StatusSuperseded, t0.Add(30*time.Minute))
	at("db", "prod", 4, common.StatusDeployed, t0.Add(30*time.Minute))
	return c
}

func TestHistoryTimelineOrderAndScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"namespace", nil, "prod/db/4,prod/db/3,prod/api/2,prod/web/2,prod/api/1,prod/web/1"},
		{"all namespaces", []string{"-A"},
			"prod/db/4,prod/db/3,dev/web/2,prod/api/2,prod/web/2,dev/api/1,prod/api/1,prod/web/1,dev/web/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := timeline(t, orderCluster(t), tc.args...)
			wantProblems(t, err, false)
			if got := revs(res); got != tc.want {
				t.Errorf("order:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestHistoryTimelineJSONRow(t *testing.T) {
	c := newCluster(t)
	u := now.Add(-time.Hour)
	c.releaseAt("web", "prod", 2, common.StatusFailed, u, u)
	res, err := timeline(t, c)
	wantProblems(t, err, true)
	if len(res.Revisions) != 1 {
		t.Fatalf("got %+v", res)
	}
	r := res.Revisions[0]
	if r.Namespace != "prod" || r.Release != "web" || r.Revision != 2 || !r.Updated.Equal(u) || r.Status != "failed" ||
		r.Chart != "web-1.2.0" || r.AppVersion != "2.0" || !strings.Contains(r.Description, "timed out waiting") {
		t.Errorf("row = %+v", r)
	}
}

// filterCluster: web v1..v4 updated 9h, 8h, 7h, 6h ago (c.release), api v1 updated 9h ago.
func filterCluster(t *testing.T) *cluster {
	c := newCluster(t)
	c.release("web", "prod", 1, common.StatusFailed, simpleManifest)
	c.release("web", "prod", 2, common.StatusFailed, simpleManifest)
	c.release("web", "prod", 3, common.StatusSuperseded, simpleManifest)
	c.release("web", "prod", 4, common.StatusDeployed, simpleManifest)
	c.release("api", "prod", 1, common.StatusFailed, simpleManifest)
	c.release("api", "prod", 2, common.StatusDeployed, simpleManifest)
	return c
}

func TestHistoryTimelineFilters(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no filters", nil, "prod/web/4,prod/web/3,prod/api/2,prod/web/2,prod/api/1,prod/web/1"},
		{"since excludes older", []string{"--since", "6h30m"}, "prod/web/4"},
		{"since is inclusive", []string{"--since", "7h"}, "prod/web/4,prod/web/3"},
		{"one status", []string{"--status", "failed"}, "prod/web/2,prod/api/1,prod/web/1"},
		{"repeated status is OR", []string{"--status", "failed", "--status", "superseded"}, "prod/web/3,prod/web/2,prod/api/1,prod/web/1"},
		{"max keeps newest", []string{"--max", "2"}, "prod/web/4,prod/web/3"},
		{"max applies after filters", []string{"--status", "failed", "--max", "1"}, "prod/web/2"},
		{"max 0 is all", []string{"--max", "0"}, "prod/web/4,prod/web/3,prod/api/2,prod/web/2,prod/api/1,prod/web/1"},
		{"negative max is all", []string{"--max", "-1"}, "prod/web/4,prod/web/3,prod/api/2,prod/web/2,prod/api/1,prod/web/1"},
		{"since and status", []string{"--since", "8h30m", "--status", "failed"}, "prod/web/2"},
		{"since, status and max", []string{"--since", "10h", "--status", "failed", "--status", "deployed", "--max", "2"}, "prod/web/4,prod/api/2"},
		{"nothing matches", []string{"--status", "uninstalled"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := timeline(t, filterCluster(t), tc.args...)
			wantProblems(t, err, false)
			if got := revs(res); got != tc.want {
				t.Errorf("got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestHistoryTimelineInvalidFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown status", []string{"history", "--status", "bogus"}, "bogus"},
		{"one bad status among good ones", []string{"history", "--status", "failed", "--status", "brokn"}, "brokn"},
		{"negative since", []string{"history", "--since", "-1h"}, "since"},
		{"release with -A", []string{"history", "web", "-A"}, "without RELEASE"},
		{"release with --since", []string{"history", "web", "--since", "1h"}, "without RELEASE"},
		{"release with --status", []string{"history", "web", "--status", "failed"}, "without RELEASE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run(t, filterCluster(t), "prod", tc.args...)
			wantExecError(t, err, tc.want)
		})
	}
}

func TestHistoryTimelineAcceptsEveryHelmStatus(t *testing.T) {
	for _, st := range []string{"unknown", "deployed", "uninstalled", "superseded", "failed", "uninstalling", "pending-install", "pending-upgrade", "pending-rollback"} {
		if _, err := timeline(t, filterCluster(t), "--status", st); err != nil {
			t.Errorf("--status %s: %v", st, err)
		}
	}
}

func TestHistoryTimelineExitCode(t *testing.T) {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	for _, tc := range []struct {
		name  string
		setup func(c *cluster)
		args  []string
		want  bool
	}{
		{"all deployed", func(c *cluster) {
			c.release("web", "prod", 1, common.StatusSuperseded, simpleManifest)
			c.release("web", "prod", 2, common.StatusDeployed, simpleManifest)
		}, nil, false},
		{"latest pending shown", func(c *cluster) {
			c.release("web", "prod", 1, common.StatusPendingUpgrade, simpleManifest)
		}, nil, true},
		{"old failure, latest deployed, filtered to failed", func(c *cluster) {
			c.release("web", "prod", 1, common.StatusFailed, simpleManifest)
			c.release("web", "prod", 2, common.StatusDeployed, simpleManifest)
		}, []string{"--status", "failed"}, false},
		{"failed latest hidden by --status, older revision shown", func(c *cluster) {
			c.release("web", "prod", 1, common.StatusSuperseded, simpleManifest)
			c.release("web", "prod", 2, common.StatusFailed, simpleManifest)
		}, []string{"--status", "superseded"}, true},
		{"failed latest hidden by --since, older revision shown", func(c *cluster) {
			c.releaseAt("web", "prod", 1, common.StatusSuperseded, ago(30*time.Minute), ago(30*time.Minute))
			c.releaseAt("web", "prod", 2, common.StatusFailed, ago(5*time.Hour), ago(5*time.Hour))
		}, []string{"--since", "1h"}, true},
		{"failed release without rows does not count", func(c *cluster) {
			c.releaseAt("broken", "prod", 1, common.StatusFailed, ago(5*time.Hour), ago(5*time.Hour))
			c.releaseAt("web", "prod", 1, common.StatusDeployed, ago(10*time.Minute), ago(10*time.Minute))
		}, []string{"--since", "1h"}, false},
		{"failed release cut by --max does not count", func(c *cluster) {
			c.releaseAt("broken", "prod", 1, common.StatusFailed, ago(5*time.Hour), ago(5*time.Hour))
			c.releaseAt("web", "prod", 1, common.StatusDeployed, ago(10*time.Minute), ago(10*time.Minute))
		}, []string{"--max", "1"}, false},
		{"failed release in another namespace is out of scope", func(c *cluster) {
			c.release("broken", "dev", 1, common.StatusFailed, simpleManifest)
			c.release("web", "prod", 1, common.StatusDeployed, simpleManifest)
		}, nil, false},
		{"failed release in another namespace with -A", func(c *cluster) {
			c.release("broken", "dev", 1, common.StatusFailed, simpleManifest)
			c.release("web", "prod", 1, common.StatusDeployed, simpleManifest)
		}, []string{"-A"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t)
			tc.setup(c)
			_, err := timeline(t, c, tc.args...)
			wantProblems(t, err, tc.want)
		})
	}
}

func TestHistoryTimelineDecodeErrors(t *testing.T) {
	c := newCluster(t)
	c.release("web", "prod", 1, common.StatusDeployed, simpleManifest)
	c.badRecord("bad", "prod")

	out, err := run(t, c, "prod", "history")
	wantProblems(t, err, false)
	if !strings.Contains(out, "\n\nwarning: 1 release record(s) could not be decoded\n") || !strings.Contains(out, "web") {
		t.Fatalf("want the readable row and the warning after a blank line:\n%s", out)
	}

	res, err := timeline(t, c)
	wantProblems(t, err, false)
	if res.DecodeErrors != 1 || revs(res) != "prod/web/1" {
		t.Fatalf("got %+v", res)
	}
}

func TestHistoryTimelineEmpty(t *testing.T) {
	out, err := run(t, newCluster(t), "prod", "history", "-o", "json")
	wantProblems(t, err, false)
	if !strings.Contains(out, `"revisions": []`) || strings.Contains(out, "decodeErrors") {
		t.Fatalf("revisions must serialize as [] and decodeErrors be omitted:\n%s", out)
	}

	out, err = run(t, newCluster(t), "prod", "history")
	wantProblems(t, err, false)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 || !slices.Equal(strings.Fields(lines[0]), timelineHeader) {
		t.Fatalf("empty timeline must print only the header:\n%q", out)
	}
}

var timelineHeader = []string{"NAMESPACE", "RELEASE", "REVISION", "UPDATED", "STATUS", "CHART", "APP", "VERSION", "DESCRIPTION"}

// Golden file: create it with `go test ./internal/cli -run TestHistoryTimelineTable -update` once the command exists.
func TestHistoryTimelineTable(t *testing.T) {
	c := newCluster(t)
	c.releaseAt("web", "prod", 1, common.StatusSuperseded, now.Add(-3*time.Hour), now.Add(-3*time.Hour))
	c.releaseAt("web", "prod", 2, common.StatusFailed, now.Add(-time.Hour), now.Add(-time.Hour))
	c.releaseAt("api", "dev", 1, common.StatusDeployed, now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	out, err := run(t, c, "prod", "history", "-A")
	wantProblems(t, err, true)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 || !slices.Equal(strings.Fields(lines[0]), timelineHeader) {
		t.Fatalf("want header and 3 rows:\n%s", out)
	}
	for i, want := range [][]string{
		{"prod", "web", "2", "2026-10-18", "11:00:00", "failed", "web-1.2.0", "2.0", "Upgrade"},
		{"dev", "api", "1", "2026-10-18", "10:00:00", "deployed", "api-1.1.0", "2.0"},
		{"prod", "web", "1", "2026-10-18", "09:00:00", "superseded", "web-1.1.0", "2.0"},
	} {
		if got := strings.Fields(lines[i+1]); len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
			t.Errorf("row %d = %q, want prefix %q", i+1, lines[i+1], want)
		}
	}
	if !strings.Contains(lines[1], "timed out waiting for the condition") {
		t.Errorf("failed row lacks its description: %q", lines[1])
	}
	golden(t, out)
}

func TestHistoryTimelineHelpExplainsOrder(t *testing.T) {
	var buf bytes.Buffer
	o := NewOptions()
	o.Out = &buf
	cmd := NewRootCommand(o)
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"history", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"newest first", "oldest first", "--since", "--status", "--all-namespaces"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q:\n%s", want, out)
		}
	}
}

// ---- decode only what is needed ----

// manyRevisions stores web v1..n in prod with store; revision v has status st(v) and was written and updated at(v).
func manyRevisions(c *cluster, store func(c *cluster, name, ns string, version int, status common.Status, updated, written time.Time), n int, st func(v int) common.Status, at func(v int) time.Time) {
	for v := 1; v <= n; v++ {
		store(c, "web", "prod", v, st(v), at(v), at(v))
	}
}

func TestHistoryTimelineSinceSkipsOldRecords(t *testing.T) {
	for _, wl := range writeLabels {
		t.Run(wl.name, func(t *testing.T) {
			c := newCluster(t)
			manyRevisions(c, wl.store, 22, func(v int) common.Status {
				if v == 22 {
					return common.StatusDeployed
				}
				return common.StatusSuperseded
			}, func(v int) time.Time {
				switch v {
				case 21:
					return now.Add(-40 * time.Minute)
				case 22:
					return now.Add(-20 * time.Minute)
				}
				return now.Add(-30*24*time.Hour + time.Duration(v)*time.Minute)
			})
			c.kube.ClearActions()
			res, err := timeline(t, c, "--since", "1h")
			wantProblems(t, err, false)
			if got := revs(res); got != "prod/web/22,prod/web/21" {
				t.Fatalf("got %s", got)
			}
			if n := secretGets(c); n != 2 {
				t.Errorf("--since 1h loaded %d records, want 2 (records written before the cutoff must not be loaded)", n)
			}
		})
	}
}

func TestHistoryTimelineStatusSkipsOtherRecords(t *testing.T) {
	for _, tc := range []struct {
		name   string
		latest common.Status
		want   bool
	}{
		{"latest deployed", common.StatusDeployed, false},
		{"latest pending", common.StatusPendingUpgrade, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t)
			manyRevisions(c, (*cluster).releaseAt, 22, func(v int) common.Status {
				switch v {
				case 5, 12:
					return common.StatusFailed
				case 22:
					return tc.latest
				}
				return common.StatusSuperseded
			}, func(v int) time.Time { return now.Add(-time.Duration(23-v) * time.Hour) })
			c.kube.ClearActions()
			res, err := timeline(t, c, "--status", "failed")
			wantProblems(t, err, tc.want)
			if got := revs(res); got != "prod/web/12,prod/web/5" {
				t.Fatalf("got %s", got)
			}
			// The exit code needs the status of the latest revision: its status label must be used instead of loading it.
			if n := secretGets(c); n != 2 {
				t.Errorf("--status failed loaded %d records, want 2 (records with another status label must not be loaded)", n)
			}
		})
	}
}

func TestHistoryTimelineMaxStopsLoading(t *testing.T) {
	for _, wl := range writeLabels {
		t.Run(wl.name, func(t *testing.T) {
			c := newCluster(t)
			manyRevisions(c, wl.store, 30, func(v int) common.Status {
				if v == 30 {
					return common.StatusDeployed
				}
				return common.StatusSuperseded
			}, func(v int) time.Time { return now.Add(-time.Duration(31-v) * time.Hour) })
			c.kube.ClearActions()
			res, err := timeline(t, c, "--max", "1", "--concurrency", "1")
			wantProblems(t, err, false)
			if got := revs(res); got != "prod/web/30" {
				t.Fatalf("got %s", got)
			}
			if n := secretGets(c); n > 2 {
				t.Errorf("--max 1 loaded %d of 30 records, want at most 2 (newest written first, stop once older ones cannot qualify)", n)
			}
		})
	}
}

// Records without the labels cannot be skipped: they must be loaded and filtered by their decoded content.
func TestHistoryTimelineRecordsWithoutLabels(t *testing.T) {
	newer := func(c *cluster) {
		for v := 1; v <= 5; v++ {
			u := now.Add(-time.Duration(6-v) * time.Hour)
			c.releaseAt("web", "prod", v, common.StatusDeployed, u, u)
		}
	}
	t.Run("max loads records without write time labels first", func(t *testing.T) {
		c := newCluster(t)
		newer(c)
		c.releaseAt("legacy", "prod", 1, common.StatusDeployed, now.Add(-time.Minute), time.Time{})
		res, err := timeline(t, c, "--max", "1")
		wantProblems(t, err, false)
		if got := revs(res); got != "prod/legacy/1" {
			t.Errorf("got %s, want the newest revision even though it has no createdAt or modifiedAt label", got)
		}
	})
	t.Run("since keeps a recent record without write time labels", func(t *testing.T) {
		c := newCluster(t)
		c.releaseAt("web", "prod", 1, common.StatusDeployed, now.Add(-48*time.Hour), now.Add(-48*time.Hour))
		c.releaseAt("legacy", "prod", 1, common.StatusDeployed, now.Add(-10*time.Minute), time.Time{})
		res, err := timeline(t, c, "--since", "1h")
		wantProblems(t, err, false)
		if got := revs(res); got != "prod/legacy/1" {
			t.Errorf("got %s", got)
		}
	})
	t.Run("status keeps a matching record without status label", func(t *testing.T) {
		c := newCluster(t)
		newer(c)
		c.releaseAt("legacy", "prod", 1, common.StatusFailed, now.Add(-10*time.Hour), now.Add(-10*time.Hour))
		c.relabel("prod", "sh.helm.release.v1.legacy.v1", func(l map[string]string) { delete(l, "status") })
		res, err := timeline(t, c, "--status", "failed")
		wantProblems(t, err, true)
		if got := revs(res); got != "prod/legacy/1" {
			t.Errorf("got %s", got)
		}
	})
}

// createdAt and modifiedAt are truncated to seconds and set after LastDeployed, so a record written in the cutoff second
// may still qualify.
func TestHistoryTimelineSinceWrittenBoundary(t *testing.T) {
	for _, wl := range writeLabels {
		t.Run(wl.name, func(t *testing.T) {
			c := newCluster(t)
			cutoff := now.Add(-time.Hour)
			in := cutoff.Add(500 * time.Millisecond)
			out := cutoff.Add(-500 * time.Millisecond)
			wl.store(c, "in", "prod", 1, common.StatusDeployed, in, in)    // label = cutoff
			wl.store(c, "out", "prod", 1, common.StatusDeployed, out, out) // label = cutoff - 1s
			res, err := timeline(t, c, "--since", "1h")
			wantProblems(t, err, false)
			if got := revs(res); got != "prod/in/1" {
				t.Errorf("got %s, want only prod/in/1", got)
			}
		})
	}
}
