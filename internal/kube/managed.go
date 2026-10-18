package kube

import (
	"context"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// Managed is a live object that claims to belong to a Helm release.
type Managed struct {
	Kind             string
	Namespace        string
	Name             string
	ReleaseName      string
	ReleaseNamespace string
}

// ListHelmManaged lists the objects of every listable kind that carry the Helm
// managed-by label and a release-name annotation. Cluster-scoped kinds are always
// listed; namespaced kinds only in namespace, or everywhere when it is empty.
// Kinds that cannot be listed are reported as warnings instead of failing the scan.
// Only object metadata is read, so Secret data never leaves the API server.
func (c *Cluster) ListHelmManaged(ctx context.Context, namespace string) ([]Managed, []string, error) {
	lists, err := discovery.ServerPreferredResources(c.Discovery)
	var warnings []string
	if err != nil {
		if len(lists) == 0 {
			return nil, nil, fmt.Errorf("discovering API resources: %w", err)
		}
		warnings = append(warnings, fmt.Sprintf("some API groups could not be discovered, their objects are not checked: %v", err))
	}
	selector := LabelManagedBy + "=Helm"
	var out []Managed
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") {
				continue
			}
			ns := ""
			if r.Namespaced {
				ns = namespace
			}
			items, err := c.Metadata.Resource(gv.WithResource(r.Name)).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("cannot list %s: %v", r.Name, err))
				continue
			}
			for _, it := range items.Items {
				name := it.Annotations[AnnotationReleaseName]
				if name == "" {
					continue
				}
				out = append(out, Managed{
					Kind: r.Kind, Namespace: it.Namespace, Name: it.Name,
					ReleaseName: name, ReleaseNamespace: it.Annotations[AnnotationReleaseNamespace],
				})
			}
		}
	}
	slices.SortFunc(out, func(a, b Managed) int {
		return strings.Compare(a.Kind+"\x00"+a.Namespace+"\x00"+a.Name, b.Kind+"\x00"+b.Namespace+"\x00"+b.Name)
	})
	return out, warnings, nil
}
