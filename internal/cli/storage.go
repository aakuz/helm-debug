package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aakuz/helm-debug/internal/helm"
)

type storageRecord struct {
	Driver   string            `json:"driver"`
	Object   string            `json:"object"`
	Revision int               `json:"revision"`
	Status   string            `json:"status,omitempty"` // decoded status
	Visible  bool              `json:"visible"`          // Helm lists it for the release
	Created  time.Time         `json:"created"`
	Bytes    int               `json:"bytes"`
	Labels   map[string]string `json:"labels,omitempty"`
	Problems []string          `json:"problems,omitempty"`
	Fix      string            `json:"fix,omitempty"`
}

type storageResult struct {
	Release        string          `json:"release"`
	Namespace      string          `json:"namespace"`
	Driver         string          `json:"driver"`
	DriverSource   string          `json:"driverSource"`   // --storage, HELM_DRIVER or default
	LatestRevision int             `json:"latestRevision"` // highest revision Helm sees; 0 when none
	NextObject     string          `json:"nextObject"`
	NextExists     bool            `json:"nextExists"` // the cause of "release: already exists"
	Records        []storageRecord `json:"records"`    // selected driver first, then other; each by revision, object
}

func newStorageCommand(o *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "storage RELEASE",
		Short: "Find release records Helm cannot see (the cause of 'release: already exists')",
		Long: `Find the storage records of a release under both storage drivers (secret and configmap)
and show which of them Helm sees and which it does not.

Records are found by object name (sh.helm.release.v1.RELEASE.vN), not by labels, because a record
with a missing or wrong label is invisible to Helm yet still blocks the next upgrade with
"release: already exists". Nothing is changed: the kubectl commands that fix the records are printed, never run.

Exits with code 2 when a problem is found. An empty result does not rule out a concurrent upgrade,
which leaves no trace in storage: see the stuck command for pending operations.`,
		Example: `  helm-debug storage web -n prod
  helm-debug storage web --storage configmap -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			name, ns, selected := args[0], c.Namespace, c.Store.Driver
			otherDrv := helm.StorageSecret
			if selected == helm.StorageSecret {
				otherDrv = helm.StorageConfigMap
			}
			sel, err := c.Store.Records(cmd.Context(), selected, name, ns)
			if err != nil {
				return err
			}
			other, err := c.Store.Records(cmd.Context(), otherDrv, name, ns)
			if err != nil {
				return err
			}
			if len(sel)+len(other) == 0 {
				return fmt.Errorf("%w: %s/%s", helm.ErrReleaseNotFound, ns, name)
			}
			res := analyzeStorage(name, ns, selected, sel, other)
			res.DriverSource = driverSource(cmd)
			if err := o.print(res, func(w io.Writer) { printStorage(o, w, res) }); err != nil {
				return err
			}
			if hasProblems(res) {
				return ErrProblems
			}
			return nil
		},
	}
	return cmd
}

// driverSource says where the selected driver came from, so a surprising choice can be explained.
func driverSource(cmd *cobra.Command) string {
	switch {
	case cmd.Flags().Changed("storage"):
		return "--storage"
	case os.Getenv("HELM_DRIVER") != "":
		return "HELM_DRIVER"
	}
	return "default"
}

// analyzeStorage reproduces how Helm sees the release in the selected driver and checks every record
// of both drivers against it. It needs no cluster, so the rules can be tested on their own.
func analyzeStorage(name, ns, selected string, sel, other []helm.StoredRecord) storageResult {
	// Helm lists a release by the name and owner labels and takes the next revision from the decoded bodies,
	// so only these records count for the next key, whatever their keys say.
	latest := 0
	for _, r := range sel {
		if isVisible(name, r) {
			latest = max(latest, r.Release.Version)
		}
	}
	res := storageResult{
		Release: name, Namespace: ns, Driver: selected, LatestRevision: latest,
		NextObject: fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, latest+1),
		Records:    []storageRecord{},
	}
	for _, group := range [][]helm.StoredRecord{sel, other} {
		sorted := append([]helm.StoredRecord(nil), group...)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].Revision != sorted[j].Revision {
				return sorted[i].Revision < sorted[j].Revision
			}
			return sorted[i].Object < sorted[j].Object
		})
		for _, r := range sorted {
			rec := checkRecord(name, ns, selected, r, sorted, r.Driver == selected && r.Object == res.NextObject)
			if rec.Object == res.NextObject && rec.Driver == selected {
				res.NextExists = true
			}
			res.Records = append(res.Records, rec)
		}
	}
	return res
}

func isVisible(name string, r helm.StoredRecord) bool {
	return r.Labels["name"] == name && r.Labels["owner"] == "helm" && r.Release != nil
}

// checkRecord lists what is wrong with one record. same holds the records of its driver, for duplicate versions.
func checkRecord(name, ns, selected string, r helm.StoredRecord, same []helm.StoredRecord, isNext bool) storageRecord {
	rec := storageRecord{
		Driver: r.Driver, Object: r.Object, Revision: r.Revision, Created: r.Created, Bytes: r.Bytes, Labels: r.Labels,
		Visible: r.Driver == selected && isVisible(name, r),
	}
	var status string
	if r.Release != nil && r.Release.Info != nil {
		status = string(r.Release.Info.Status)
	}
	rec.Status = status
	add := func(format string, a ...any) { rec.Problems = append(rec.Problems, fmt.Sprintf(format, a...)) }

	if r.DecodeErr != nil {
		add("undecodable: %s", cleanLine(r.DecodeErr.Error()))
	}
	if isNext {
		add("next revision: Helm writes this key on the next upgrade")
	}
	labelProblems := len(rec.Problems)
	for _, l := range []string{"name", "owner", "version", "status"} {
		if _, ok := r.Labels[l]; !ok {
			add("missing label %s", l)
		}
	}
	checkLabel := func(l, want, wantText string) {
		if v, ok := r.Labels[l]; ok && v != want {
			add("label %s=%s, %s", l, cleanLine(v), wantText)
		}
	}
	checkLabel("name", name, "want "+name)
	checkLabel("owner", "helm", "want helm")
	checkLabel("version", strconv.Itoa(r.Revision), fmt.Sprintf("key says %d", r.Revision))
	// An undecodable record has no status to compare the label with.
	if r.Release != nil {
		checkLabel("status", status, "record says "+status)
	}
	if r.Release != nil && (r.Release.Name != name || r.Release.Version != r.Revision) {
		add("record says %s revision %d", cleanLine(r.Release.Name), r.Release.Version)
	}
	if v := r.Labels["version"]; v != "" {
		for _, o := range same {
			if o.Object != r.Object && o.Labels["version"] == v {
				add("duplicate version label %s (also %s)", cleanLine(v), cleanLine(o.Object))
				break
			}
		}
	}
	labelsBroken := len(rec.Problems) > labelProblems

	if r.Driver != selected {
		add("stored by the %s driver, Helm uses %s", r.Driver, selected)
	}
	kind := "secret"
	if r.Driver == helm.StorageConfigMap {
		kind = "configmap"
	}
	switch {
	case r.DecodeErr != nil || isNext:
		rec.Fix = fmt.Sprintf("kubectl delete %s %s -n %s", kind, r.Object, ns)
	case labelsBroken:
		rec.Fix = fmt.Sprintf("kubectl label %s %s -n %s --overwrite name=%s owner=helm version=%d", kind, r.Object, ns, name, r.Revision)
		if r.Release != nil {
			rec.Fix += " status=" + status
		}
	}
	return rec
}

func printStorage(o *Options, w io.Writer, res storageResult) {
	fmt.Fprintf(w, "Release %s in namespace %s, storage driver %s (%s)\n", res.Release, res.Namespace, res.Driver, res.DriverSource)
	if res.LatestRevision == 0 {
		fmt.Fprintf(w, "Helm sees no revision of %s; the next install writes %s.\n", res.Release, res.NextObject)
	} else {
		fmt.Fprintf(w, "Helm sees revisions up to %d; the next upgrade writes %s", res.LatestRevision, res.NextObject)
		if res.NextExists {
			fmt.Fprint(w, ", which already exists: this record causes \"release: already exists\"")
		}
		fmt.Fprintln(w, ".")
	}
	if res.NextExists {
		for _, r := range res.Records {
			if r.Driver == res.Driver && r.Object == res.NextObject {
				keys := make([]string, 0, len(r.Labels))
				for k := range r.Labels {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for i, k := range keys {
					keys[i] = cleanLine(k) + "=" + cleanLine(r.Labels[k])
				}
				fmt.Fprintf(w, "  labels: %s  created: %s  size: %d bytes\n", strings.Join(keys, ", "), formatTime(r.Created), r.Bytes)
			}
		}
	}

	fmt.Fprintln(w)
	t := newTable(w)
	t.add("DRIVER", "OBJECT", "REVISION", "STATUS", "VISIBLE", "CREATED", "SIZE", "PROBLEMS")
	var fixes []string
	for _, r := range res.Records {
		visible := "no"
		if r.Visible {
			visible = "yes"
		}
		problems := strings.Join(r.Problems, "; ")
		if problems != "" {
			problems = o.paint(problems, red)
		}
		status := cleanLine(r.Status)
		if status != "" {
			status = o.paint(status, statusColor(status))
		}
		t.add(r.Driver, r.Object, strconv.Itoa(r.Revision), status, visible, formatTime(r.Created), strconv.Itoa(r.Bytes), problems)
		if r.Fix != "" {
			fixes = append(fixes, r.Fix)
		}
	}
	t.flush()

	if len(fixes) > 0 {
		fmt.Fprintln(w, "\nFixes (not run):")
		for _, f := range fixes {
			fmt.Fprintf(w, "  %s\n", f)
		}
	}
	if !hasProblems(res) {
		fmt.Fprintf(w, "\nNo records Helm cannot see. A concurrent upgrade leaves no trace here; check pending operations with helm-debug stuck.\n")
	}
}

func hasProblems(res storageResult) bool {
	for _, r := range res.Records {
		if len(r.Problems) > 0 {
			return true
		}
	}
	return false
}
