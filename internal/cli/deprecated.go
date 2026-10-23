package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/aakuz/helm-debug/internal/helm"
	"github.com/aakuz/helm-debug/internal/kube"
)

// statusNotServed marks a built-in apiVersion that the connected cluster does not serve.
const statusNotServed = "not-served"

type deprecatedRow struct {
	helm.Object
	Status      string `json:"status"`
	RemovedIn   string `json:"removedIn,omitempty"`
	Replacement string `json:"replacement,omitempty"`
}

type deprecatedResult struct {
	Release       string          `json:"release"`
	Namespace     string          `json:"namespace"`
	Revision      int             `json:"revision"`
	TargetVersion string          `json:"targetVersion"`
	Scanned       int             `json:"scanned"`
	Findings      []deprecatedRow `json:"findings"`
}

func newDeprecatedCommand(o *Options) *cobra.Command {
	var revision int
	var target string
	cmd := &cobra.Command{
		Use:   "deprecated RELEASE",
		Short: "Find apiVersions in a release that Kubernetes removed or is about to remove",
		Long: `Scan the stored manifest and hooks of a release revision for apiVersions that are
removed in the target Kubernetes version ("no matches for kind ... in version ...", the
usual reason an upgrade needs helm-mapkubeapis). Nothing is changed.

Each object is checked against a built-in table of Kubernetes API removals and against
the connected cluster: a built-in apiVersion that the cluster does not serve is reported
as not-served. Kinds from CRD groups are not checked against the cluster.

The target version defaults to the cluster's server version; pass --target-version to
check an upgrade before it happens.

Exits with code 2 when removed or deprecated apiVersions are found.`,
		Example: `  helm-debug deprecated backend --target-version 1.32
  helm-debug deprecated backend -n prod -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			targetVersion, err := resolveTargetVersion(c, target)
			if err != nil {
				return err
			}
			rel, err := c.Store.Revision(args[0], c.Namespace, revision)
			if err != nil {
				return err
			}
			objs := helm.Objects(rel)
			res := deprecatedResult{
				Release: rel.Name, Namespace: rel.Namespace, Revision: rel.Version,
				TargetVersion: targetVersion.String(), Scanned: len(objs), Findings: []deprecatedRow{},
			}
			for _, obj := range objs {
				if row, ok := checkAPIVersion(c, obj, targetVersion); ok {
					res.Findings = append(res.Findings, row)
				}
			}
			if err := o.print(res, func(w io.Writer) { printDeprecated(o, w, res) }); err != nil {
				return err
			}
			if len(res.Findings) > 0 {
				return ErrProblems
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&revision, "revision", "r", 0, "Release revision to inspect (default: latest)")
	cmd.Flags().StringVar(&target, "target-version", "", "Kubernetes version to check against, e.g. 1.32 (default: the cluster's server version)")
	return cmd
}

func resolveTargetVersion(c *Clients, flag string) (kube.Version, error) {
	if flag != "" {
		return kube.ParseVersion(flag)
	}
	info, err := c.Cluster.Discovery.ServerVersion()
	if err != nil {
		return kube.Version{}, fmt.Errorf("cannot read the cluster version, pass --target-version: %w", err)
	}
	v, err := kube.ParseVersion(info.GitVersion)
	if err != nil || v.Major == 0 {
		return kube.Version{}, errors.New("cannot determine the cluster version, pass --target-version")
	}
	return v, nil
}

// checkAPIVersion reports a finding when the object's apiVersion is removed in the target version,
// is not served by the cluster, or is deprecated but still served at the target version.
func checkAPIVersion(c *Clients, obj helm.Object, target kube.Version) (deprecatedRow, bool) {
	row := deprecatedRow{Object: obj}
	rem, known := kube.LookupRemoval(obj.APIVersion, obj.Kind)
	status := ""
	if known {
		status = rem.StatusAt(target)
		row.Replacement = rem.Replacement
	}
	switch {
	case status == kube.StatusRemoved:
		row.Status = kube.StatusRemoved
	case kube.IsBuiltinAPIVersion(obj.APIVersion) && notServed(c, obj):
		row.Status = statusNotServed
	case status == kube.StatusDeprecated:
		row.Status = kube.StatusDeprecated
	default:
		return deprecatedRow{}, false
	}
	// A removal that is still ahead of a not-served object would only confuse.
	if known && row.Status != statusNotServed {
		row.RemovedIn = rem.RemovedIn.String()
	}
	return row, true
}

func notServed(c *Clients, obj helm.Object) bool {
	_, err := c.Cluster.Resolve(obj.APIVersion, obj.Kind)
	return meta.IsNoMatchError(err)
}

func printDeprecated(o *Options, w io.Writer, res deprecatedResult) {
	fmt.Fprintf(w, "Release %s/%s revision %d, target Kubernetes %s\n\n", res.Namespace, res.Release, res.Revision, res.TargetVersion)
	if len(res.Findings) == 0 {
		fmt.Fprintf(w, "No deprecated apiVersions among %d objects\n", res.Scanned)
		return
	}
	t := newTable(w)
	t.add("KIND", "NAME", "APIVERSION", "HOOK", "STATUS", "REMOVED-IN", "REPLACEMENT")
	for _, f := range res.Findings {
		t.add(f.Kind, f.Name, f.APIVersion, f.HookEvents(), o.paint(f.Status, statusColor(f.Status)), f.RemovedIn, f.Replacement)
	}
	t.flush()
}
