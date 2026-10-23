package cli

import (
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	rspb "helm.sh/helm/v4/pkg/release/v1"

	"github.com/aakuz/helm-debug/internal/helm"
	"github.com/aakuz/helm-debug/internal/kube"
)

type findMatch struct {
	Release           string    `json:"release"`
	ReleaseNamespace  string    `json:"releaseNamespace"`
	Revision          int       `json:"revision"`
	Status            string    `json:"status"`
	Updated           time.Time `json:"updated"`
	ResourceNamespace string    `json:"resourceNamespace,omitempty"`
	Hook              []string  `json:"hook,omitempty"`
}

type findResult struct {
	Kind         string      `json:"kind"`
	Name         string      `json:"name"`
	Namespace    string      `json:"namespace,omitempty"`
	Live         *kube.Live  `json:"live,omitempty"`
	Source       string      `json:"source"`
	Scanned      int         `json:"scanned,omitempty"`
	DecodeErrors int         `json:"decodeErrors,omitempty"`
	Matches      []findMatch `json:"matches"`
	Warnings     []string    `json:"warnings,omitempty"`
}

type findQuery struct {
	kind       string // canonical kind if resolved, otherwise as typed
	name       string
	namespace  string // "" = any namespace
	namespaced bool
	resolved   *kube.Resolved
}

func newFindCommand(o *Options) *cobra.Command {
	var allNamespaces, allRevisions bool
	cmd := &cobra.Command{
		Use:   "find KIND/NAME | KIND NAME",
		Short: "Find which Helm release(s) declare an object",
		Long: `Find which Helm release(s) declare an object.

First the live object is checked for the meta.helm.sh/release-* annotations and
the annotated release is verified. If that does not answer the question, release
records in all namespaces are scanned (latest revision of every release, or every
revision with --all-revisions).

KIND accepts kinds, plural and short names (deploy, svc, cm), optionally with a
group (certificates.cert-manager.io).

Exits with code 2 when no release declares the object.`,
		Example: `  helm-debug find deployment/nginx -n prod
  helm-debug find svc api -A
  helm-debug find clusterrole/flux-ui --all-revisions`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, name, err := parseKindName(args)
			if err != nil {
				return err
			}
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			q := findQuery{kind: kind, name: name, namespaced: true}
			if r, err := c.Cluster.ResolveArg(kind); err == nil {
				q.kind, q.namespaced, q.resolved = r.Kind, r.Namespaced, &r
			} else {
				slog.Debug("kind not known to the cluster, matching it by name", "kind", kind, "error", err)
			}
			if q.namespaced && !allNamespaces {
				q.namespace = c.Namespace
			}
			res, err := runFind(cmd, o, c, q, allRevisions)
			if err != nil {
				return err
			}
			if err := o.print(res, func(w io.Writer) { printFind(o, w, res) }); err != nil {
				return err
			}
			if len(res.Matches) == 0 {
				return ErrProblems
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&allNamespaces, "all-namespaces", "A", false, "Match the object in any namespace")
	cmd.Flags().BoolVar(&allRevisions, "all-revisions", false, "Search every stored revision, not only the latest one")
	return cmd
}

func parseKindName(args []string) (string, string, error) {
	if len(args) == 2 {
		return args[0], args[1], nil
	}
	kind, name, ok := strings.Cut(args[0], "/")
	if !ok || kind == "" || name == "" {
		return "", "", fmt.Errorf("expected KIND/NAME or KIND NAME, got %q", args[0])
	}
	return kind, name, nil
}

func runFind(cmd *cobra.Command, o *Options, c *Clients, q findQuery, allRevisions bool) (*findResult, error) {
	res := &findResult{Kind: q.kind, Name: q.name, Namespace: q.namespace, Matches: []findMatch{}}

	// Fast path: trust but verify the ownership annotations of the live object.
	if q.resolved != nil && (!q.namespaced || q.namespace != "") {
		live := c.Cluster.Get(cmd.Context(), *q.resolved, q.namespace, q.name)
		res.Live = &live
		if live.ReleaseName != "" {
			rels, err := c.Store.History(live.ReleaseName, live.ReleaseNamespace)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("object is annotated with release %s/%s, but it cannot be read: %v", live.ReleaseNamespace, live.ReleaseName, err))
			} else {
				if !allRevisions {
					rels = rels[:1]
				}
				// History is newest first; matches are listed oldest first like helm history.
				for i := len(rels) - 1; i >= 0; i-- {
					res.Matches = append(res.Matches, matchRelease(rels[i], q)...)
				}
				if len(res.Matches) > 0 {
					res.Source = "annotation"
					return res, nil
				}
				res.Warnings = append(res.Warnings, fmt.Sprintf("object is annotated with release %s/%s, but that release does not declare it", live.ReleaseNamespace, live.ReleaseName))
			}
		}
	}

	res.Source = "scan"
	refs, err := c.Store.ListRefs(cmd.Context(), "")
	if err != nil {
		return nil, err
	}
	if !allRevisions {
		refs = helm.LatestOnly(refs)
	}
	matches := make([][]findMatch, len(refs))
	failed := make([]bool, len(refs))
	parallel(len(refs), o.Concurrency, func(i int) {
		rel, err := c.Store.Load(refs[i])
		if err != nil {
			slog.Debug("cannot decode release record", "namespace", refs[i].Namespace, "object", refs[i].Object, "error", err)
			failed[i] = true
			return
		}
		matches[i] = matchRelease(rel, q)
	})
	res.Scanned = len(refs)
	for i := range refs {
		res.Matches = append(res.Matches, matches[i]...)
		if failed[i] {
			res.DecodeErrors++
		}
	}
	sort.SliceStable(res.Matches, func(i, j int) bool {
		a, b := res.Matches[i], res.Matches[j]
		if a.ReleaseNamespace != b.ReleaseNamespace {
			return a.ReleaseNamespace < b.ReleaseNamespace
		}
		if a.Release != b.Release {
			return a.Release < b.Release
		}
		return a.Revision < b.Revision
	})
	if owners := distinctReleases(res.Matches); !allRevisions && len(owners) > 1 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("object is declared by %d releases (%s); only one of them can own it", len(owners), strings.Join(owners, ", ")))
	}
	if res.Live != nil && res.Live.Status == kube.StatusPresent && res.Live.ReleaseName == "" && len(res.Matches) > 0 {
		res.Warnings = append(res.Warnings, "live object has no meta.helm.sh/release-* annotations; Helm will refuse to adopt it (see `resources` for fix commands)")
	}
	return res, nil
}

func matchRelease(rel *rspb.Release, q findQuery) []findMatch {
	var out []findMatch
	for _, obj := range helm.Objects(rel) {
		if !strings.EqualFold(obj.Kind, q.kind) || obj.Name != q.name {
			continue
		}
		ns := obj.Namespace
		if q.namespaced && ns == "" {
			ns = rel.Namespace
		}
		if !q.namespaced {
			ns = ""
		}
		if q.namespace != "" && ns != q.namespace {
			continue
		}
		m := findMatch{Release: rel.Name, ReleaseNamespace: rel.Namespace, Revision: rel.Version, ResourceNamespace: ns, Hook: obj.Hook}
		if rel.Info != nil {
			m.Status, m.Updated = string(rel.Info.Status), rel.Info.LastDeployed
		}
		out = append(out, m)
	}
	return out
}

func distinctReleases(ms []findMatch) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range ms {
		k := m.ReleaseNamespace + "/" + m.Release
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func printFind(o *Options, w io.Writer, res *findResult) {
	target := res.Kind + "/" + res.Name
	if res.Namespace != "" {
		target = res.Namespace + "/" + target
	}
	if res.Live != nil {
		live := res.Live.Status
		if res.Live.ReleaseName != "" {
			live += fmt.Sprintf(", annotated with release %s/%s", res.Live.ReleaseNamespace, res.Live.ReleaseName)
		}
		fmt.Fprintf(w, "Live object %s: %s\n", target, live)
	}
	if res.Source == "scan" {
		fmt.Fprintf(w, "Scanned %d release record(s)", res.Scanned)
		if res.DecodeErrors > 0 {
			fmt.Fprintf(w, ", %s", o.paint(fmt.Sprintf("%d could not be decoded", res.DecodeErrors), yellow))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
	if len(res.Matches) == 0 {
		fmt.Fprintf(w, "%s is not declared by any Helm release\n", target)
	} else {
		t := newTable(w)
		t.add("RELEASE NAMESPACE", "RELEASE", "REVISION", "STATUS", "UPDATED", "OBJECT NAMESPACE", "HOOK")
		for _, m := range res.Matches {
			t.add(m.ReleaseNamespace, m.Release, fmt.Sprint(m.Revision), o.paint(m.Status, statusColor(m.Status)), formatTime(m.Updated), m.ResourceNamespace, strings.Join(m.Hook, ","))
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
