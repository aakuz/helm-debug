package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/aakuz/helm-debug/internal/helm"
	"github.com/aakuz/helm-debug/internal/kube"
)

// Audit findings.
const (
	findingRevisions  = "revisions"
	findingRecordSize = "record-size"
	findingOrphaned   = "orphaned"
)

type auditRow struct {
	Release        string   `json:"release"`
	Namespace      string   `json:"namespace"`
	Status         string   `json:"status,omitempty"`
	Revisions      int      `json:"revisions"`
	LatestRevision int      `json:"latestRevision"`
	LargestBytes   int      `json:"largestBytes"`
	TotalBytes     int      `json:"totalBytes"`
	Findings       []string `json:"findings"`
}

type auditResult struct {
	Scanned        int        `json:"scanned"`
	DecodeErrors   int        `json:"decodeErrors,omitempty"`
	MaxRevisions   int        `json:"maxRevisions"`
	MaxRecordBytes int        `json:"maxRecordBytes"`
	Releases       []auditRow `json:"releases"`
}

func newAuditCommand(o *Options) *cobra.Command {
	var allNamespaces bool
	var maxRevisions int
	var maxRecordSize string
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Report revision counts, record sizes and releases whose objects are all gone",
		Long: `Report, for every release, how many revisions its history holds, how large its
records are (a record must stay below the 1 MiB Kubernetes object limit) and whether
all objects of its latest revision are gone from the cluster. Nothing is changed.

A release is flagged when it has more revisions than --max-revisions, when its largest
record exceeds --max-record-size, or when it is orphaned: its latest revision declares
objects and none of them exist. Uninstalled releases that keep their history are not orphaned.

Exits with code 2 when a release is flagged.`,
		Example: `  helm-debug audit -A
  helm-debug audit -n prod --max-revisions 10 --max-record-size 512Ki`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			limit, err := resource.ParseQuantity(maxRecordSize)
			if err != nil || limit.Value() <= 0 {
				return fmt.Errorf("invalid --max-record-size %q (want a size such as 800Ki)", maxRecordSize)
			}
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			ns := c.Namespace
			if allNamespaces {
				ns = ""
			}
			sizes, err := c.Store.RecordSizes(cmd.Context(), ns)
			if err != nil {
				return err
			}
			res := auditResult{MaxRevisions: maxRevisions, MaxRecordBytes: int(limit.Value()), Releases: []auditRow{}}
			groups := groupRecords(sizes)
			rows := make([]auditRow, len(groups))
			failed := make([]bool, len(groups))
			parallel(len(groups), o.Concurrency, func(i int) {
				rows[i], failed[i] = auditRelease(cmd.Context(), c, groups[i], res.MaxRevisions, res.MaxRecordBytes)
			})
			res.Scanned = len(groups)
			res.Releases = append(res.Releases, rows...)
			for _, f := range failed {
				if f {
					res.DecodeErrors++
				}
			}
			if err := o.print(res, func(w io.Writer) { printAudit(o, w, res) }); err != nil {
				return err
			}
			if slices.ContainsFunc(rows, func(r auditRow) bool { return len(r.Findings) > 0 }) {
				return ErrProblems
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "Audit releases in all namespaces")
	cmd.Flags().IntVar(&maxRevisions, "max-revisions", 20, "Flag releases with more revisions than this")
	cmd.Flags().StringVar(&maxRecordSize, "max-record-size", "800Ki", "Flag releases whose largest record exceeds this size (the limit is 1Mi)")
	return cmd
}

// groupRecords gathers the records of each release, sorted by namespace and release name.
func groupRecords(sizes []helm.RecordSize) [][]helm.RecordSize {
	byRelease := map[string][]helm.RecordSize{}
	for _, s := range sizes {
		key := s.Namespace + "/" + s.Release
		byRelease[key] = append(byRelease[key], s)
	}
	keys := make([]string, 0, len(byRelease))
	for k := range byRelease {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	groups := make([][]helm.RecordSize, len(keys))
	for i, k := range keys {
		groups[i] = byRelease[k]
	}
	return groups
}

// auditRelease measures one release; failed is true when its latest record cannot be decoded.
func auditRelease(ctx context.Context, c *Clients, records []helm.RecordSize, maxRevisions, maxRecordBytes int) (row auditRow, failed bool) {
	latest := records[0]
	row = auditRow{
		Release: latest.Release, Namespace: latest.Namespace, Revisions: len(records), Findings: []string{},
	}
	for _, r := range records {
		row.TotalBytes += r.Bytes
		row.LargestBytes = max(row.LargestBytes, r.Bytes)
		if r.Version > latest.Version {
			latest = r
		}
	}
	row.LatestRevision = latest.Version
	if row.Revisions > maxRevisions {
		row.Findings = append(row.Findings, findingRevisions)
	}
	if row.LargestBytes > maxRecordBytes {
		row.Findings = append(row.Findings, findingRecordSize)
	}
	rel, err := c.Store.Load(latest.Ref)
	if err != nil {
		slog.Debug("cannot decode release record", "namespace", latest.Namespace, "object", latest.Object, "error", err)
		return row, true
	}
	if rel.Info != nil {
		row.Status = string(rel.Info.Status)
	}
	if allObjectsGone(ctx, c, rel) {
		row.Findings = append(row.Findings, findingOrphaned)
	}
	return row, false
}

// allObjectsGone is true when the revision declares objects and every one of them is verifiably absent.
// A failed lookup or an unknown kind proves nothing, so it keeps the release from being flagged.
// Hooks are ignored, since they are meant to disappear; so are releases being installed or removed.
func allObjectsGone(ctx context.Context, c *Clients, rel *rspb.Release) bool {
	if rel.Info != nil && (rel.Info.Status.IsPending() || rel.Info.Status == common.StatusUninstalled || rel.Info.Status == common.StatusUninstalling) {
		return false
	}
	declared := 0
	for _, obj := range helm.Objects(rel) {
		if len(obj.Hook) > 0 {
			continue
		}
		declared++
		res, err := c.Cluster.Resolve(obj.APIVersion, obj.Kind)
		if err != nil {
			return false
		}
		ns := obj.Namespace
		switch {
		case !res.Namespaced:
			ns = ""
		case ns == "":
			ns = rel.Namespace
		}
		if c.Cluster.Get(ctx, res, ns, obj.Name).Status != kube.StatusAbsent {
			return false
		}
	}
	return declared > 0
}

func printAudit(o *Options, w io.Writer, res auditResult) {
	if len(res.Releases) == 0 {
		fmt.Fprintln(w, "No releases found")
	} else {
		t := newTable(w)
		t.add("NAMESPACE", "RELEASE", "STATUS", "REVISIONS", "LARGEST", "TOTAL", "FINDINGS")
		for _, r := range res.Releases {
			t.add(r.Namespace, r.Release, o.paint(r.Status, statusColor(r.Status)), fmt.Sprint(r.Revisions),
				formatSize(r.LargestBytes), formatSize(r.TotalBytes), o.paint(strings.Join(r.Findings, ","), red))
		}
		t.flush()
	}
	if res.DecodeErrors > 0 {
		fmt.Fprintf(w, "\n%s %d release(s) have a latest record that could not be decoded\n", o.paint("warning:", yellow), res.DecodeErrors)
	}
}

func formatSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}
