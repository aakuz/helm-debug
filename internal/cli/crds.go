package cli

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	releaseutil "helm.sh/helm/v4/pkg/release/v1/util"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/aakuz/helm-debug/internal/kube"
)

var crdResource = kube.Resolved{
	GVR:  schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
	Kind: "CustomResourceDefinition",
}

type crdVersion struct {
	Name    string `json:"name"`
	Served  bool   `json:"served"`
	Storage bool   `json:"storage"`
}

type crdRow struct {
	Name            string       `json:"name"`
	Source          string       `json:"source"`
	Status          string       `json:"status"`
	ChartVersions   []crdVersion `json:"chartVersions"`
	ClusterVersions []crdVersion `json:"clusterVersions"`
	StoredVersions  []string     `json:"storedVersions"`
	Conversion      string       `json:"conversion,omitempty"`
	WebhookService  string       `json:"webhookService,omitempty"`
	Changes         []string     `json:"changes,omitempty"`
	Diff            string       `json:"diff,omitempty"`
	Notes           []string     `json:"notes,omitempty"`
	Error           string       `json:"error,omitempty"`
}

type crdsResult struct {
	Release   string   `json:"release"`
	Namespace string   `json:"namespace"`
	Revision  int      `json:"revision"`
	CRDs      []crdRow `json:"crds"`
}

// crdSource is a CRD document of the release and where it came from.
type crdSource struct {
	obj    *unstructured.Unstructured
	source string
}

func newCrdsCommand(o *Options) *cobra.Command {
	var revision int
	cmd := &cobra.Command{
		Use:   "crds RELEASE",
		Short: "Compare the CRDs a release ships with the CRDs in the cluster",
		Long: `Compare the CustomResourceDefinitions a release revision ships with the ones in
the cluster. Helm installs the files under a chart's crds/ directory once and
never upgrades or deletes them, so they silently fall behind the chart. CRDs
rendered from templates/ are included too.
Only spec is compared, as in drift: metadata, status and live-only fields (API
defaults such as conversion.strategy) are ignored. The conversion webhook
caBundle is ignored as well. Each outdated CRD is
followed by a unified diff of the chart's (expected) and the cluster's spec.
The table lists the versions of both sides ("*" marks the storage version, a
version in parentheses is not served), the versions the API server stored
objects in, and the conversion strategy. Notes warn about stored versions the
chart drops and about a changed storage version.
Subchart crds/ files are not included: stored release records do not keep
subcharts. Exits with code 2 for absent or outdated CRDs; notes do not change
the exit code.`,
		Example: `  helm-debug crds backend
  helm-debug crds backend --revision 3 -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if revision < 0 {
				return fmt.Errorf("invalid revision %d: must not be negative", revision)
			}
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			rel, err := c.Store.Revision(args[0], c.Namespace, revision)
			if err != nil {
				return err
			}
			sources, err := crdSources(rel.Manifest, rel.Chart)
			if err != nil {
				return err
			}
			rows := make([]crdRow, len(sources))
			parallel(len(sources), o.Concurrency, func(i int) {
				rows[i] = crdCompare(cmd, c, sources[i])
			})
			sort.SliceStable(rows, func(i, j int) bool {
				if rows[i].Name != rows[j].Name {
					return rows[i].Name < rows[j].Name
				}
				return rows[i].Source < rows[j].Source
			})
			result := &crdsResult{Release: rel.Name, Namespace: rel.Namespace, Revision: rel.Version, CRDs: rows}
			if err := o.print(result, func(w io.Writer) { printCrds(o, w, result) }); err != nil {
				return err
			}
			var lookupErr error
			outdated := false
			for _, row := range rows {
				if row.Error != "" && lookupErr == nil {
					lookupErr = fmt.Errorf("%s: %s", row.Name, row.Error)
				}
				outdated = outdated || row.Status == "outdated" || row.Status == kube.StatusAbsent
			}
			if lookupErr != nil {
				return lookupErr
			}
			if outdated {
				return ErrProblems
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&revision, "revision", "r", 0, "Release revision to inspect (default: latest)")
	return cmd
}

// crdSources collects the CRD documents of the chart's crds/ files and of the
// rendered manifest. Documents of other kinds and empty documents are skipped.
func crdSources(manifest string, ch *chart.Chart) ([]crdSource, error) {
	var sources []crdSource
	add := func(text, source string) error {
		for _, doc := range releaseutil.SplitManifests(text) {
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
				return fmt.Errorf("parsing CRD in %s: %w", source, err)
			}
			if obj.GetKind() != crdResource.Kind {
				continue
			}
			if obj.GetName() == "" {
				return fmt.Errorf("CRD in %s is missing metadata.name", source)
			}
			sources = append(sources, crdSource{obj: obj, source: source})
		}
		return nil
	}
	if ch != nil {
		for _, crd := range ch.CRDObjects() {
			if err := add(string(crd.File.Data), crd.Filename); err != nil {
				return nil, err
			}
		}
	}
	if err := add(manifest, "templates"); err != nil {
		return nil, err
	}
	return sources, nil
}

func crdCompare(cmd *cobra.Command, c *Clients, src crdSource) crdRow {
	row := crdRow{Name: src.obj.GetName(), Source: src.source, ChartVersions: crdVersions(src.obj)}
	live := c.Cluster.Get(cmd.Context(), crdResource, "", row.Name)
	row.Status, row.Error = live.Status, live.Error
	if live.Status != kube.StatusPresent {
		return row
	}
	row.ClusterVersions = crdVersions(live.Object)
	row.StoredVersions, _, _ = unstructured.NestedStringSlice(live.Object.Object, "status", "storedVersions")
	row.Conversion, _, _ = unstructured.NestedString(live.Object.Object, "spec", "conversion", "strategy")
	if row.Conversion == "" {
		row.Conversion = "None"
	}
	if row.Conversion == "Webhook" {
		cfg, _, _ := unstructured.NestedMap(live.Object.Object, "spec", "conversion", "webhook", "clientConfig")
		if url, _ := cfg["url"].(string); url != "" {
			row.WebhookService = url
		} else {
			ns, _, _ := unstructured.NestedString(cfg, "service", "namespace")
			name, _, _ := unstructured.NestedString(cfg, "service", "name")
			row.WebhookService = ns + "/" + name
		}
	}
	// Both sides are wrapped so that paths read /spec/..., as in drift.
	expected := map[string]any{"spec": runtime.DeepCopyJSONValue(src.obj.Object["spec"])}
	actual := map[string]any{"spec": runtime.DeepCopyJSONValue(live.Object.Object["spec"])}
	// Charts ship a placeholder caBundle that cert-manager's cainjector overwrites.
	for _, side := range []map[string]any{expected, actual} {
		unstructured.RemoveNestedField(side, "spec", "conversion", "webhook", "clientConfig", "caBundle")
	}
	row.Changes = kube.ChangedPaths(expected, actual)
	row.Status = "in-sync"
	if len(row.Changes) > 0 {
		row.Status = "outdated"
		want, got := kube.Project(expected, actual)
		var err error
		if row.Diff, err = yamlDiff(want, got); err != nil {
			row.Error = err.Error()
		}
	}
	row.Notes = crdNotes(row)
	return row
}

// crdNotes explains version changes that the spec diff alone does not make obvious.
func crdNotes(row crdRow) []string {
	var notes []string
	for _, v := range row.StoredVersions {
		if !slices.ContainsFunc(row.ChartVersions, func(cv crdVersion) bool { return cv.Name == v }) {
			notes = append(notes, fmt.Sprintf("storedVersions has %s, which the chart's CRD drops: the API server rejects the CRD until stored objects are migrated and %s is removed from status.storedVersions", v, v))
		}
	}
	if chartStorage, liveStorage := storageVersion(row.ChartVersions), storageVersion(row.ClusterVersions); chartStorage != liveStorage {
		notes = append(notes, fmt.Sprintf("storage version is %s in the cluster, %s in the chart", liveStorage, chartStorage))
	}
	return notes
}

func storageVersion(versions []crdVersion) string {
	for _, v := range versions {
		if v.Storage {
			return v.Name
		}
	}
	return ""
}

// crdVersions summarizes spec.versions in declared order.
func crdVersions(crd *unstructured.Unstructured) []crdVersion {
	list, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	versions := []crdVersion{}
	for _, item := range list {
		m, _ := item.(map[string]any)
		name, _ := m["name"].(string)
		served, _ := m["served"].(bool)
		storage, _ := m["storage"].(bool)
		versions = append(versions, crdVersion{Name: name, Served: served, Storage: storage})
	}
	return versions
}

// versionList renders versions as "(v1beta1),v1*": "*" marks the storage
// version, parentheses a version that is not served.
func versionList(versions []crdVersion) string {
	parts := make([]string, len(versions))
	for i, v := range versions {
		parts[i] = stripUnsafe(v.Name)
		if !v.Served {
			parts[i] = "(" + parts[i] + ")"
		}
		if v.Storage {
			parts[i] += "*"
		}
	}
	return strings.Join(parts, ",")
}

func printCrds(o *Options, w io.Writer, result *crdsResult) {
	if len(result.CRDs) == 0 {
		fmt.Fprintf(w, "Release %s/%s revision %d ships no CRDs\n", result.Namespace, result.Release, result.Revision)
		return
	}
	fmt.Fprintf(w, "Release %s/%s revision %d\n\n", result.Namespace, result.Release, result.Revision)
	t := newTable(w)
	t.add("NAME", "SOURCE", "STATUS", "CHART VERSIONS", "CLUSTER VERSIONS", "STORED", "CONVERSION")
	for _, row := range result.CRDs {
		conversion := row.Conversion
		if row.WebhookService != "" {
			conversion += " " + stripUnsafe(row.WebhookService)
		}
		t.add(o.paint(stripUnsafe(row.Name), name), stripUnsafe(row.Source), o.paint(row.Status, statusColor(row.Status)),
			versionList(row.ChartVersions), versionList(row.ClusterVersions), stripUnsafe(strings.Join(row.StoredVersions, ",")), conversion)
	}
	t.flush()
	fmt.Fprint(w, "\n* storage version, (v) not served\n")
	for _, row := range result.CRDs {
		if row.Diff != "" {
			fmt.Fprintf(w, "\n--- CustomResourceDefinition %s (%s)\n%s", o.paint(stripUnsafe(row.Name), name), o.paint(row.Status, statusColor(row.Status)), o.paintDiff(row.Diff))
		}
	}
	for _, row := range result.CRDs {
		for _, note := range row.Notes {
			fmt.Fprintf(w, "\nnote: %s: %s\n", stripUnsafe(row.Name), stripUnsafe(note))
		}
	}
	for _, row := range result.CRDs {
		if row.Error != "" {
			fmt.Fprintf(w, "\nerror: %s: %s\n", stripUnsafe(row.Name), cleanLine(row.Error))
		}
	}
}
