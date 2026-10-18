package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	releaseutil "helm.sh/helm/v4/pkg/release/v1/util"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/aakuz/helm-debug/internal/helm"
	"github.com/aakuz/helm-debug/internal/kube"
)

type driftRow struct {
	helm.Object
	Status  string   `json:"status"`
	Changes []string `json:"changes,omitempty"`
	Diff    string   `json:"diff,omitempty"`
	Error   string   `json:"error,omitempty"`

	// values and removed feed `drift --values`; they are not part of the structured output.
	values  map[string]any
	removed []string
}

type driftResult struct {
	Release   string     `json:"release"`
	Namespace string     `json:"namespace"`
	Revision  int        `json:"revision"`
	Resources []driftRow `json:"resources"`
}

func newDriftCommand(o *Options) *cobra.Command {
	var values bool
	cmd := &cobra.Command{
		Use:   "drift RELEASE",
		Short: "Compare the latest stored manifest with live objects",
		Long: `Compare fields declared by the latest release manifest with live objects.
Server metadata, status and live-only fields (defaults and controller additions)
are ignored. Lists are compared in order. Hooks are excluded.
Each drifted object is followed by a unified diff of the declared (expected) and
live fields. Secret data values are masked unless --show-secrets is set.
With --values the table is replaced by a values-like YAML document of the live
values at the drifted paths, one per drifted object. It is not mapped to chart
values keys: the manifest is already rendered, so map the fields by hand. Fields
that were removed from the live object are listed in "# removed:" comments.
--values is ignored with -o json or -o yaml. Exits with code 2 for drift.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			result, err := runDrift(cmd, o, c, args[0])
			if err != nil {
				return err
			}
			if err := o.print(result, func(w io.Writer) {
				if values {
					printDriftValues(w, result)
					return
				}
				printDrift(o, w, result)
			}); err != nil {
				return err
			}
			var lookupErr error
			drifted := false
			for _, row := range result.Resources {
				if row.Error != "" && lookupErr == nil {
					lookupErr = fmt.Errorf("%s/%s: %s", row.Kind, row.Name, row.Error)
				}
				drifted = drifted || row.Status == "drifted" || row.Status == kube.StatusAbsent
			}
			if lookupErr != nil {
				return lookupErr
			}
			if drifted {
				return ErrProblems
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&values, "values", false, "Print the live values at drifted paths as YAML instead of the table (table output only)")
	return cmd
}

func runDrift(cmd *cobra.Command, o *Options, c *Clients, name string) (*driftResult, error) {
	rel, err := c.Store.Revision(name, c.Namespace, 0)
	if err != nil {
		return nil, err
	}
	var objects []*unstructured.Unstructured
	for _, doc := range releaseutil.SplitManifests(rel.Manifest) {
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			return nil, fmt.Errorf("parsing release manifest: %w", err)
		}
		if obj.GetAPIVersion() == "" || obj.GetKind() == "" || obj.GetName() == "" {
			return nil, fmt.Errorf("release manifest object is missing apiVersion, kind or metadata.name")
		}
		objects = append(objects, obj)
	}
	sort.Slice(objects, func(i, j int) bool {
		a, b := objects[i], objects[j]
		if a.GetKind() != b.GetKind() {
			return a.GetKind() < b.GetKind()
		}
		if a.GetNamespace() != b.GetNamespace() {
			return a.GetNamespace() < b.GetNamespace()
		}
		return a.GetName() < b.GetName()
	})
	rows := make([]driftRow, len(objects))
	parallel(len(objects), o.Concurrency, func(i int) {
		obj := objects[i]
		row := driftRow{Object: helm.Object{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName()}}
		resolved, err := c.Cluster.Resolve(row.APIVersion, row.Kind)
		if err != nil {
			row.Status, row.Error = kube.StatusUnknownKind, err.Error()
			rows[i] = row
			return
		}
		if !resolved.Namespaced {
			row.Namespace = ""
		} else if row.Namespace == "" {
			row.Namespace = rel.Namespace
		}
		obj.SetNamespace(row.Namespace)
		live := c.Cluster.Get(cmd.Context(), resolved, row.Namespace, row.Name)
		row.Status, row.Error = live.Status, live.Error
		if live.Status == kube.StatusPresent {
			row.Changes = kube.ChangedPaths(obj.Object, live.Object.Object)
			row.Status = "in-sync"
			if len(row.Changes) > 0 {
				row.Status = "drifted"
				driftDetails(o, &row, obj.Object, live.Object.Object)
			}
		}
		rows[i] = row
	})
	return &driftResult{Release: rel.Name, Namespace: rel.Namespace, Revision: rel.Version, Resources: rows}, nil
}

func printDrift(o *Options, w io.Writer, result *driftResult) {
	fmt.Fprintf(w, "Release %s/%s revision %d\n\n", result.Namespace, result.Release, result.Revision)
	t := newTable(w)
	t.add("KIND", "NAMESPACE", "NAME", "STATUS", "CHANGED FIELDS")
	for _, row := range result.Resources {
		t.add(row.Kind, row.Namespace, o.paint(row.Name, name), o.paint(row.Status, statusColor(row.Status)), strings.Join(row.Changes, ", "))
	}
	t.flush()
	for _, row := range result.Resources {
		if row.Diff == "" {
			continue
		}
		ns := ""
		if row.Namespace != "" {
			ns = row.Namespace + "/"
		}
		fmt.Fprintf(w, "\n--- %s %s%s (%s)\n%s", row.Kind, ns, o.paint(row.Name, name), o.paint(row.Status, statusColor(row.Status)), o.paintDiff(row.Diff))
	}
	for _, row := range result.Resources {
		if row.Error != "" {
			fmt.Fprintf(w, "\nerror: %s/%s: %s\n", row.Kind, row.Name, row.Error)
		}
	}
}

// driftDetails fills the diff and the live values of a drifted row. Secret data
// is masked unless --show-secrets is set.
func driftDetails(o *Options, row *driftRow, expected, live map[string]any) {
	want, got := kube.Project(expected, live)
	secret := row.Kind == "Secret" && !o.ShowSecrets
	row.values, row.removed = liveValues(kube.Normalize(live), row.Changes)
	if data, ok := row.values["data"].(map[string]any); ok && secret {
		for k := range data {
			data[k] = "<masked>"
		}
	}
	if secret {
		maskSecret(want, got)
	}
	wantText, err := yaml.Marshal(want)
	if err == nil {
		var gotText []byte
		if gotText, err = yaml.Marshal(got); err == nil {
			row.Diff = unifiedDiff(string(wantText), string(gotText))
		}
	}
	if err != nil {
		row.Error = err.Error()
	}
}

// liveValues builds nested maps holding the live value at every changed JSON
// pointer. A path through a list yields the whole list. Paths that no longer
// exist in the live object are returned separately.
func liveValues(live map[string]any, paths []string) (values map[string]any, removed []string) {
	values = map[string]any{}
	for _, path := range paths {
		node, dst := live, values
		tokens := strings.Split(strings.TrimPrefix(path, "/"), "/")
		for i, token := range tokens {
			key := strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
			child, exists := node[key]
			if !exists {
				removed = append(removed, path)
				break
			}
			next, isMap := child.(map[string]any)
			if i == len(tokens)-1 || !isMap {
				dst[key] = child
				break
			}
			sub, ok := dst[key].(map[string]any)
			if !ok {
				sub = map[string]any{}
				dst[key] = sub
			}
			node, dst = next, sub
		}
	}
	return values, removed
}

func printDriftValues(w io.Writer, result *driftResult) {
	first := true
	for _, row := range result.Resources {
		if row.Status != "drifted" {
			continue
		}
		if !first {
			fmt.Fprintln(w, "---")
		}
		first = false
		ref := row.Kind + " " + row.Name
		if row.Namespace != "" {
			ref = row.Kind + " " + row.Namespace + "/" + row.Name
		}
		fmt.Fprintf(w, "# %s\n", ref)
		for _, path := range row.removed {
			fmt.Fprintf(w, "# removed: %s\n", path)
		}
		if len(row.values) > 0 {
			b, _ := yaml.Marshal(row.values)
			fmt.Fprint(w, string(b))
		}
	}
	if first {
		fmt.Fprintln(w, "# No drifted objects")
	}
}
