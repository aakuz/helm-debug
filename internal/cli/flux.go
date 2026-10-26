package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/aakuz/helm-debug/internal/helm"
)

var helmReleaseKind = schema.GroupKind{Group: "helm.toolkit.fluxcd.io", Kind: "HelmRelease"}

// maxReleaseNameLen is Helm's limit on release names; helm-controller shortens longer generated names to fit it.
const maxReleaseNameLen = 53

type fluxCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type fluxStatusResult struct {
	HelmRelease      string          `json:"helmRelease"`
	ReleaseName      string          `json:"releaseName"`
	ReleaseNamespace string          `json:"releaseNamespace"`
	StorageNamespace string          `json:"storageNamespace"`
	Conditions       []fluxCondition `json:"conditions"`
	Revisions        []historyRow    `json:"revisions"`
	Fix              string          `json:"fix,omitempty"`
	Warning          string          `json:"warning,omitempty"`
}

func newFluxCommand(o *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "flux",
		Short: "Inspect Flux-managed releases",
	}
	cmd.AddCommand(newFluxStatusCommand(o))
	return cmd
}

func newFluxStatusCommand(o *Options) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "status NAME",
		Short: "Show a Flux HelmRelease's conditions next to the history of the Helm release it manages",
		Long: `Resolve a Flux HelmRelease (helm.toolkit.fluxcd.io) to its Helm release and show the HelmRelease conditions
together with the release revisions, in the same format and order as history. -n is the namespace of the HelmRelease.

Exits with code 2 when the Ready condition is missing or not True, the latest revision is failed or pending,
or no release records exist.`,
		Example: `  helm-debug flux status podinfo -n flux-system`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			mapping, err := c.Cluster.Mapper.RESTMapping(helmReleaseKind)
			if err != nil {
				if meta.IsNoMatchError(err) {
					return fmt.Errorf("the HelmRelease CRD (%s) is not installed in the cluster", helmReleaseKind.Group)
				}
				return err
			}
			hr, err := c.Cluster.Dynamic.Resource(mapping.Resource).Namespace(c.Namespace).Get(cmd.Context(), args[0], metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("helmrelease %s/%s not found", c.Namespace, args[0])
			}
			if err != nil {
				return err
			}
			relName, relNs, storageNs := resolveHelmRelease(hr)
			res := fluxStatusResult{
				HelmRelease: c.Namespace + "/" + args[0], ReleaseName: relName, ReleaseNamespace: relNs, StorageNamespace: storageNs,
				Conditions: fluxConditions(hr), Revisions: []historyRow{},
			}
			problems := !fluxReady(res.Conditions)
			rels, err := c.Store.History(relName, storageNs)
			switch {
			case errors.Is(err, helm.ErrReleaseNotFound):
				res.Warning = fmt.Sprintf("no Helm release records found for %q in namespace %s", relName, storageNs)
				problems = true
			case err != nil:
				return err
			default:
				// rels is newest first: --max keeps the latest revisions, which are then printed oldest first like helm history.
				for i, r := range rels {
					if limit > 0 && i >= limit {
						break
					}
					res.Revisions = append([]historyRow{toHistoryRow(r)}, res.Revisions...)
				}
				if st := rels[0].Info.Status; st.IsPending() || st == "failed" {
					problems = true
					if st.IsPending() {
						res.Fix = unlockCommand(c.Store, rels[0].Name, storageNs, rels[0].Version)
					}
				}
			}
			if err := o.print(res, func(w io.Writer) { printFluxStatus(o, w, res) }); err != nil {
				return err
			}
			if problems {
				return ErrProblems
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "max", 0, "Maximum number of revisions to show (0 = all)")
	return cmd
}

// resolveHelmRelease mirrors how helm-controller names the release and picks the namespaces holding it and its records.
func resolveHelmRelease(hr *unstructured.Unstructured) (name, namespace, storageNamespace string) {
	str := func(k string) string { s, _, _ := unstructured.NestedString(hr.Object, "spec", k); return s }
	namespace, storageNamespace = hr.GetNamespace(), hr.GetNamespace()
	target := str("targetNamespace")
	if target != "" {
		namespace = target
	}
	if s := str("storageNamespace"); s != "" {
		storageNamespace = s
	}
	if name = str("releaseName"); name != "" {
		return name, namespace, storageNamespace
	}
	name = hr.GetName()
	if target != "" {
		name = target + "-" + name
	}
	if len(name) > maxReleaseNameLen {
		sum := sha256.Sum256([]byte(name))
		name = name[:40] + "-" + hex.EncodeToString(sum[:])[:12]
	}
	return name, namespace, storageNamespace
}

// fluxConditions puts Ready and Released first, since they answer "is it healthy", then the rest in stored order.
func fluxConditions(hr *unstructured.Unstructured) []fluxCondition {
	items, _, _ := unstructured.NestedSlice(hr.Object, "status", "conditions")
	var first, rest []fluxCondition
	for _, it := range items {
		m, _ := it.(map[string]any)
		var c fluxCondition
		c.Type, _, _ = unstructured.NestedString(m, "type")
		c.Status, _, _ = unstructured.NestedString(m, "status")
		c.Reason, _, _ = unstructured.NestedString(m, "reason")
		c.Message, _, _ = unstructured.NestedString(m, "message")
		switch c.Type {
		case "Ready":
			first = append([]fluxCondition{c}, first...)
		case "Released":
			first = append(first, c)
		default:
			rest = append(rest, c)
		}
	}
	return append(append([]fluxCondition{}, first...), rest...)
}

func fluxReady(conds []fluxCondition) bool {
	for _, c := range conds {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

func printFluxStatus(o *Options, w io.Writer, res fluxStatusResult) {
	fmt.Fprintf(w, "HelmRelease:       %s\nRelease:           %s\nRelease namespace: %s\nStorage namespace: %s\n\n",
		res.HelmRelease, res.ReleaseName, res.ReleaseNamespace, res.StorageNamespace)
	t := newTable(w)
	t.add("TYPE", "STATUS", "REASON", "MESSAGE")
	for _, c := range res.Conditions {
		color := ""
		switch c.Status {
		case "True":
			color = green
		case "False":
			color = red
		}
		t.add(c.Type, o.paint(c.Status, color), c.Reason, c.Message)
	}
	t.flush()
	if res.Warning != "" {
		fmt.Fprintf(w, "\n%s %s\n", o.paint("warning:", yellow), res.Warning)
		return
	}
	fmt.Fprintln(w)
	printHistory(o, w, historyResult{Revisions: res.Revisions})
	if res.Fix != "" {
		fmt.Fprintf(w, "\nThe latest revision is pending. Fix: %s\n", res.Fix)
	}
}
