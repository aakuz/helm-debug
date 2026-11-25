package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"time"

	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"

	"github.com/aakuz/helm-debug/internal/helm"
)

var helmStatuses = []common.Status{
	common.StatusUnknown, common.StatusDeployed, common.StatusUninstalled, common.StatusSuperseded, common.StatusFailed,
	common.StatusUninstalling, common.StatusPendingInstall, common.StatusPendingUpgrade, common.StatusPendingRollback,
}

type timelineRow struct {
	Namespace string `json:"namespace"`
	Release   string `json:"release"`
	historyRow
}

type timelineResult struct {
	DecodeErrors int           `json:"decodeErrors,omitempty"`
	Revisions    []timelineRow `json:"revisions"`
}

func releaseKey(namespace, name string) string { return namespace + "/" + name }

// writtenBefore reports whether a record last written at t cannot hold a revision updated at or after cutoff.
// Helm sets Info.LastDeployed before it creates or updates the record, and both labels are
// truncated to seconds, so LastDeployed is always below the write time + 1s.
func writtenBefore(t, cutoff time.Time) bool {
	return !t.IsZero() && !t.Add(time.Second).After(cutoff)
}

// runTimeline lists revisions of all releases in ns (all namespaces when empty), newest first.
// Labels on the record metadata let it skip decoding records that cannot match the filters.
func runTimeline(ctx context.Context, o *Options, c *Clients, ns string, since time.Duration, statuses []string, limit int) error {
	refs, err := c.Store.ListRefs(ctx, ns)
	if err != nil {
		return err
	}
	latest := map[string]helm.Ref{}
	for _, r := range refs {
		if cur, ok := latest[releaseKey(r.Namespace, r.Release)]; !ok || r.Version > cur.Version {
			latest[releaseKey(r.Namespace, r.Release)] = r
		}
	}
	var cutoff time.Time
	if since > 0 {
		cutoff = o.Now().Add(-since)
	}
	wantStatus := func(s string) bool { return len(statuses) == 0 || slices.Contains(statuses, s) }

	cands := refs[:0:0]
	for _, r := range refs {
		if r.Status != "" && !wantStatus(r.Status) {
			continue
		}
		if since > 0 && writtenBefore(r.Written, cutoff) {
			continue
		}
		cands = append(cands, r)
	}
	// Records without a write time come first as they cannot be ruled out.
	sort.SliceStable(cands, func(i, j int) bool {
		zi, zj := cands[i].Written.IsZero(), cands[j].Written.IsZero()
		if zi != zj {
			return zi
		}
		return cands[i].Written.After(cands[j].Written)
	})

	loaded := map[string]*rspb.Release{}
	var rows []timelineRow
	decodeErrors := 0
	keep := func(rel *rspb.Release) (timelineRow, bool) {
		h := toHistoryRow(rel)
		if !wantStatus(h.Status) || (since > 0 && h.Updated.Before(cutoff)) {
			return timelineRow{}, false
		}
		return timelineRow{Namespace: rel.Namespace, Release: rel.Name, historyRow: h}, true
	}
	sortRows := func() {
		sort.Slice(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			switch {
			case !a.Updated.Equal(b.Updated):
				return a.Updated.After(b.Updated)
			case a.Namespace != b.Namespace:
				return a.Namespace < b.Namespace
			case a.Release != b.Release:
				return a.Release < b.Release
			}
			return a.Revision > b.Revision
		})
	}

	batch := len(cands)
	if limit > 0 {
		batch = max(o.Concurrency, 1)
	}
	for start := 0; start < len(cands); start += batch {
		part := cands[start:min(start+batch, len(cands))]
		rels := make([]*rspb.Release, len(part))
		parallel(len(part), o.Concurrency, func(i int) {
			rel, err := c.Store.Load(part[i])
			if err != nil {
				slog.Debug("cannot decode release record", "namespace", part[i].Namespace, "object", part[i].Object, "error", err)
				return
			}
			rels[i] = rel
		})
		for i, rel := range rels {
			if rel == nil {
				decodeErrors++
				continue
			}
			loaded[releaseKey(part[i].Namespace, part[i].Object)] = rel
			if row, ok := keep(rel); ok {
				rows = append(rows, row)
			}
		}
		if limit > 0 && len(rows) >= limit && start+batch < len(cands) {
			sortRows()
			next := cands[start+batch]
			if !next.Written.IsZero() && writtenBefore(next.Written, rows[limit-1].Updated) {
				break
			}
		}
	}
	sortRows()
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}

	res := timelineResult{DecodeErrors: decodeErrors, Revisions: rows}
	if res.Revisions == nil {
		res.Revisions = []timelineRow{}
	}
	if err := o.print(res, func(w io.Writer) { printTimeline(o, w, res) }); err != nil {
		return err
	}

	// The latest revision may be filtered out of the rows, so the exit code looks at it separately.
	var keys []string
	seen := map[string]bool{}
	for _, r := range rows {
		if k := releaseKey(r.Namespace, r.Release); !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	problem := make([]bool, len(keys))
	parallel(len(keys), o.Concurrency, func(i int) {
		ref := latest[keys[i]]
		st := ref.Status
		if rel, ok := loaded[releaseKey(ref.Namespace, ref.Object)]; ok {
			st = ""
			if rel.Info != nil {
				st = string(rel.Info.Status)
			}
		} else if st == "" {
			rel, err := c.Store.Load(ref)
			if err != nil || rel.Info == nil {
				return
			}
			st = string(rel.Info.Status)
		}
		problem[i] = common.Status(st).IsPending() || common.Status(st) == common.StatusFailed
	})
	if slices.Contains(problem, true) {
		return ErrProblems
	}
	return nil
}

func printTimeline(o *Options, w io.Writer, res timelineResult) {
	t := newTable(w)
	t.add("NAMESPACE", "RELEASE", "REVISION", "UPDATED", "STATUS", "CHART", "APP VERSION", "DESCRIPTION")
	for _, r := range res.Revisions {
		t.add(r.Namespace, r.Release, fmt.Sprint(r.Revision), formatTime(r.Updated), o.paint(r.Status, statusColor(r.Status)), r.Chart, r.AppVersion, r.Description)
	}
	t.flush()
	if res.DecodeErrors > 0 {
		fmt.Fprintf(w, "\n%s %d release record(s) could not be decoded\n", o.paint("warning:", yellow), res.DecodeErrors)
	}
}
