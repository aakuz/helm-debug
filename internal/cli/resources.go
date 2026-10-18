package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/aakuz/helm-debug/internal/helm"
	"github.com/aakuz/helm-debug/internal/kube"
)

type resourceRow struct {
	helm.Object
	Status    string `json:"status"`
	Ownership string `json:"ownership,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Error     string `json:"error,omitempty"`
}

type resourcesResult struct {
	Release   string        `json:"release"`
	Namespace string        `json:"namespace"`
	Revision  int           `json:"revision"`
	Status    string        `json:"status"`
	Resources []resourceRow `json:"resources"`
	Fixes     []string      `json:"fixes,omitempty"`
}

func newResourcesCommand(o *Options) *cobra.Command {
	var revision int
	cmd := &cobra.Command{
		Use:   "resources RELEASE",
		Short: "List objects declared by a release revision and check them against the cluster",
		Long: `List every object declared by a release revision (manifest and hooks) and check
that it exists in the cluster and carries Helm ownership metadata
(meta.helm.sh/release-name, meta.helm.sh/release-namespace, app.kubernetes.io/managed-by=Helm).

Exits with code 2 when a non-hook object is absent or its ownership is wrong.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			res, err := runResources(cmd, o, c, args[0], revision)
			if err != nil {
				return err
			}
			if err := o.print(res, func(w io.Writer) { printResources(o, w, res) }); err != nil {
				return err
			}
			for _, r := range res.Resources {
				if r.problem() {
					return ErrProblems
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&revision, "revision", "r", 0, "Release revision to inspect (default: latest)")
	return cmd
}

func (r resourceRow) problem() bool {
	if len(r.Hook) > 0 {
		return false
	}
	return r.Status != kube.StatusPresent || r.Ownership != kube.OwnershipOK
}

func runResources(cmd *cobra.Command, o *Options, c *Clients, name string, revision int) (*resourcesResult, error) {
	rel, err := c.Store.Revision(name, c.Namespace, revision)
	if err != nil {
		return nil, err
	}
	objs := helm.Objects(rel)
	rows := make([]resourceRow, len(objs))
	parallel(len(objs), o.Concurrency, func(i int) {
		obj := objs[i]
		row := resourceRow{Object: obj}
		res, err := c.Cluster.Resolve(obj.APIVersion, obj.Kind)
		if err != nil {
			row.Status, row.Error = kube.StatusUnknownKind, err.Error()
			rows[i] = row
			return
		}
		switch {
		case !res.Namespaced:
			row.Namespace = ""
		case row.Namespace == "":
			row.Namespace = rel.Namespace
		}
		live := c.Cluster.Get(cmd.Context(), res, row.Namespace, obj.Name)
		row.Status, row.Error = live.Status, live.Error
		if len(obj.Hook) == 0 {
			row.Ownership = live.Ownership(rel.Name, rel.Namespace)
			if row.Ownership == kube.OwnershipMismatch {
				row.Owner = fmt.Sprintf("%s/%s managed-by=%s", live.ReleaseNamespace, live.ReleaseName, live.ManagedBy)
			}
		}
		rows[i] = row
	})

	res := &resourcesResult{
		Release:   rel.Name,
		Namespace: rel.Namespace,
		Revision:  rel.Version,
		Status:    string(rel.Info.Status),
		Resources: rows,
	}
	for _, r := range rows {
		if r.Ownership == kube.OwnershipMissing || r.Ownership == kube.OwnershipMismatch {
			res.Fixes = append(res.Fixes, kube.AdoptCommands(r.Kind, r.Namespace, r.Name, rel.Name, rel.Namespace)...)
		}
	}
	return res, nil
}

func printResources(o *Options, w io.Writer, res *resourcesResult) {
	fmt.Fprintf(w, "Release %s/%s revision %d (%s)\n\n", res.Namespace, res.Release, res.Revision, o.paint(res.Status, statusColor(res.Status)))
	t := newTable(w)
	t.add("KIND", "NAMESPACE", "NAME", "HOOK", "STATUS", "OWNERSHIP")
	present := 0
	for _, r := range res.Resources {
		if r.Status == kube.StatusPresent {
			present++
		}
		own := r.Ownership
		if r.Owner != "" {
			own += " (" + r.Owner + ")"
		}
		t.add(r.Kind, r.Namespace, r.Name, r.HookEvents(), o.paint(r.Status, statusColor(r.Status)), o.paint(own, statusColor(r.Ownership)))
	}
	t.flush()
	sep := "\n"
	for _, r := range res.Resources {
		if r.Error != "" {
			fmt.Fprintf(w, "%s%s %s/%s: %s\n", sep, o.paint("error:", red), r.Kind, r.Name, r.Error)
			sep = ""
		}
	}
	if present == 0 && len(res.Resources) > 0 {
		fmt.Fprintf(w, "\n%s none of the declared objects exist - the release may have been uninstalled or its objects deleted\n", o.paint("warning:", yellow))
	}
	if len(res.Fixes) > 0 {
		fmt.Fprintln(w, "\nSuggested fixes (let Helm adopt the objects):")
		for _, f := range res.Fixes {
			fmt.Fprintln(w, "  "+f)
		}
	}
}
