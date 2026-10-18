package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
)

type lostResource struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

type lostRelease struct {
	Release   string         `json:"release"`
	Namespace string         `json:"namespace"`
	Resources []lostResource `json:"resources"`
}

type lostResult struct {
	Releases []lostRelease `json:"releases"`
	Warnings []string      `json:"warnings,omitempty"`
}

func newLostFoundCommand(o *Options) *cobra.Command {
	var allNamespaces bool
	cmd := &cobra.Command{
		Use:   "lost-found",
		Short: "Find resources that claim a Helm release which no longer exists",
		Long: `Find resources that claim a Helm release which no longer exists.

Every object labeled app.kubernetes.io/managed-by=Helm and annotated with
meta.helm.sh/release-name is matched against the release records in the storage
(all namespaces). Objects whose release has no record are listed, grouped by the
release they claim. Cluster-scoped kinds are always checked; namespaced kinds in
the current namespace, or in all of them with -A. Nothing is changed.

Exits with code 2 when lost resources are found.`,
		Example: `  helm-debug lost-found -A
  helm-debug lost-found -n prod -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			ns := c.Namespace
			if allNamespaces {
				ns = ""
			}
			refs, err := c.Store.ListRefs(cmd.Context(), "")
			if err != nil {
				return err
			}
			byNamespacedName := map[string]bool{}
			byName := map[string]bool{}
			for _, r := range refs {
				byNamespacedName[r.Namespace+"/"+r.Release] = true
				byName[r.Release] = true
			}
			objs, warnings, err := c.Cluster.ListHelmManaged(cmd.Context(), ns)
			if err != nil {
				return err
			}
			res := lostResult{Releases: []lostRelease{}, Warnings: warnings}
			byRelease := map[string]int{}
			for _, m := range objs {
				// A cluster-scoped object without the namespace annotation can only be matched by name.
				if (m.ReleaseNamespace == "" && byName[m.ReleaseName]) || byNamespacedName[m.ReleaseNamespace+"/"+m.ReleaseName] {
					continue
				}
				key := m.ReleaseNamespace + "/" + m.ReleaseName
				i, ok := byRelease[key]
				if !ok {
					i = len(res.Releases)
					byRelease[key] = i
					res.Releases = append(res.Releases, lostRelease{Release: m.ReleaseName, Namespace: m.ReleaseNamespace})
				}
				res.Releases[i].Resources = append(res.Releases[i].Resources, lostResource{Kind: m.Kind, Namespace: m.Namespace, Name: m.Name})
			}
			sort.Slice(res.Releases, func(i, j int) bool {
				a, b := res.Releases[i], res.Releases[j]
				if a.Namespace != b.Namespace {
					return a.Namespace < b.Namespace
				}
				return a.Release < b.Release
			})
			if err := o.print(res, func(w io.Writer) { printLost(o, w, res) }); err != nil {
				return err
			}
			if len(res.Releases) > 0 {
				return ErrProblems
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "Look for resources in all namespaces")
	return cmd
}

func printLost(o *Options, w io.Writer, res lostResult) {
	if len(res.Releases) == 0 {
		fmt.Fprintln(w, "No lost resources: every Helm-managed object belongs to an existing release")
	} else {
		t := newTable(w)
		t.add("RELEASE NAMESPACE", "RELEASE", "KIND", "NAMESPACE", "NAME")
		for _, r := range res.Releases {
			for _, x := range r.Resources {
				t.add(r.Namespace, o.paint(r.Release, red), x.Kind, x.Namespace, x.Name)
			}
		}
		t.flush()
	}
	for _, wr := range res.Warnings {
		fmt.Fprintf(w, "\n%s %s", o.paint("warning:", yellow), wr)
	}
	if len(res.Warnings) > 0 {
		fmt.Fprintln(w)
	}
}
