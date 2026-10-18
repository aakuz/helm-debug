package cli

import (
	"fmt"
	"io"
	"log/slog"
	"sort"

	"github.com/spf13/cobra"

	"github.com/aakuz/helm-debug/internal/helm"
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
release they claim. Namespaced kinds are checked in the current namespace, or in
all of them with -A; cluster-scoped objects are attributed to the namespace of the
release they claim and are listed only for that namespace. Nothing is changed.

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
			// A record lives in the storage namespace, which can differ from the namespace of the release it holds
			// (helm -n infra list shows cert-manager/cert-manager for a record stored in infra), so records are
			// matched by name first and the release namespace is read from the record only when needed.
			latest := helm.LatestOnly(refs)
			byName := map[string][]helm.Ref{}
			for _, r := range latest {
				byName[r.Release] = append(byName[r.Release], r)
			}
			exists := map[string]bool{}
			undecodable := 0
			releaseExists := func(relNs, name string) bool {
				key := relNs + "/" + name
				if v, ok := exists[key]; ok {
					return v
				}
				found := false
				for _, r := range byName[name] {
					// A cluster-scoped object without the namespace annotation can only be matched by name.
					if relNs == "" || r.Namespace == relNs {
						found = true
						break
					}
					rel, err := c.Store.Load(r)
					if err != nil {
						slog.Debug("cannot decode release record", "namespace", r.Namespace, "object", r.Object, "error", err)
						undecodable++
						found = true // cannot tell, so do not call it lost
						break
					}
					if rel.Namespace == relNs {
						found = true
						break
					}
				}
				exists[key] = found
				return found
			}
			progress, eraseProgress := o.progressFunc("resource kinds")
			objs, warnings, err := c.Cluster.ListHelmManaged(cmd.Context(), ns, progress)
			eraseProgress()
			if err != nil {
				return err
			}
			res := lostResult{Releases: []lostRelease{}, Warnings: warnings}
			byRelease := map[string]int{}
			for _, m := range objs {
				if releaseExists(m.ReleaseNamespace, m.ReleaseName) {
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
			if undecodable > 0 {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%d release record(s) could not be decoded; objects of releases with those names are not reported", undecodable))
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
