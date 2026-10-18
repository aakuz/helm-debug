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
// progress, when not nil, is called after each kind with the number of kinds done, the total and the kind's resource name.
func (c *Cluster) ListHelmManaged(ctx context.Context, namespace string, progress func(done, total int, resource string)) ([]Managed, []string, error) {
	lists, err := discovery.ServerPreferredResources(c.Discovery)
	var warnings []string
	if err != nil {
		if len(lists) == 0 {
			return nil, nil, fmt.Errorf("discovering API resources: %w", err)
		}
		warnings = append(warnings, fmt.Sprintf("some API groups could not be discovered, their objects are not checked: %v", err))
	}
	type target struct {
		gvr        schema.GroupVersionResource
		kind       string
		namespaced bool
	}
	var targets []target
	for _, list := range lists {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			if strings.Contains(r.Name, "/") || !slices.Contains(r.Verbs, "list") {
				continue
			}
			targets = append(targets, target{gv.WithResource(r.Name), r.Kind, r.Namespaced})
		}
	}
	selector := LabelManagedBy + "=Helm"
	var out []Managed
	for i, t := range targets {
		ns := ""
		if t.namespaced {
			ns = namespace
		}
		opts := metav1.ListOptions{LabelSelector: selector, Limit: 500}
		for {
			items, err := c.Metadata.Resource(t.gvr).Namespace(ns).List(ctx, opts)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("cannot list %s: %v", t.gvr.Resource, err))
				break
			}
			for _, it := range items.Items {
				name := it.Annotations[AnnotationReleaseName]
				if name == "" {
					continue
				}
				// Helm installs into the release namespace, so that is where an unannotated namespaced object belongs.
				relNs := it.Annotations[AnnotationReleaseNamespace]
				if relNs == "" {
					relNs = it.Namespace
				}
				out = append(out, Managed{Kind: t.kind, Namespace: it.Namespace, Name: it.Name, ReleaseName: name, ReleaseNamespace: relNs})
			}
			if items.Continue == "" {
				break
			}
			opts.Continue = items.Continue
		}
		if progress != nil {
			progress(i+1, len(targets), t.gvr.Resource)
		}
	}
	slices.SortFunc(out, func(a, b Managed) int {
		return strings.Compare(a.Kind+"\x00"+a.Namespace+"\x00"+a.Name, b.Kind+"\x00"+b.Namespace+"\x00"+b.Name)
	})
	return out, warnings, nil
}
