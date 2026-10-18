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
	Error   string   `json:"error,omitempty"`
}

type driftResult struct {
	Release   string     `json:"release"`
	Namespace string     `json:"namespace"`
	Revision  int        `json:"revision"`
	Resources []driftRow `json:"resources"`
}

func newDriftCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "drift RELEASE",
		Short: "Compare the latest stored manifest with live objects",
		Long: `Compare fields declared by the latest release manifest with live objects.
Server metadata, status and live-only fields (defaults and controller additions)
are ignored. Lists are compared in order. Hooks are excluded.
Only field paths are printed, never values. Exits with code 2 for drift.`,
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
			if err := o.print(result, func(w io.Writer) { printDrift(w, result) }); err != nil {
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
			}
		}
		rows[i] = row
	})
	return &driftResult{Release: rel.Name, Namespace: rel.Namespace, Revision: rel.Version, Resources: rows}, nil
}

func printDrift(w io.Writer, result *driftResult) {
	fmt.Fprintf(w, "Release %s/%s revision %d\n\n", result.Namespace, result.Release, result.Revision)
	t := newTable(w)
	t.add("KIND", "NAMESPACE", "NAME", "STATUS", "CHANGED FIELDS")
	for _, row := range result.Resources {
		t.add(row.Kind, row.Namespace, row.Name, row.Status, strings.Join(row.Changes, ", "))
	}
	t.flush()
	for _, row := range result.Resources {
		if row.Error != "" {
			fmt.Fprintf(w, "\nerror: %s/%s: %s\n", row.Kind, row.Name, row.Error)
		}
	}
}
