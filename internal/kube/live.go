// Package kube looks up live cluster objects and their Helm ownership metadata.
package kube

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Helm ownership metadata keys.
const (
	AnnotationReleaseName      = "meta.helm.sh/release-name"
	AnnotationReleaseNamespace = "meta.helm.sh/release-namespace"
	LabelManagedBy             = "app.kubernetes.io/managed-by"
)

// Status of a live object lookup.
const (
	StatusPresent     = "present"
	StatusAbsent      = "absent"
	StatusUnknownKind = "unknown-kind"
	StatusError       = "error"
)

// Ownership verdicts.
const (
	OwnershipOK       = "ok"
	OwnershipMissing  = "missing"
	OwnershipMismatch = "mismatch"
)

// Cluster resolves kinds and fetches live objects.
type Cluster struct {
	Dynamic dynamic.Interface
	Mapper  meta.RESTMapper
}

// Resolved is a kind mapped to its API resource.
type Resolved struct {
	GVR        schema.GroupVersionResource
	Kind       string
	Namespaced bool
}

// Resolve maps apiVersion+kind to a resource.
func (c *Cluster) Resolve(apiVersion, kind string) (Resolved, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return Resolved{}, err
	}
	m, err := c.Mapper.RESTMapping(gv.WithKind(kind).GroupKind(), gv.Version)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{GVR: m.Resource, Kind: m.GroupVersionKind.Kind, Namespaced: m.Scope.Name() == meta.RESTScopeNameNamespace}, nil
}

// ResolveArg maps a user-supplied resource name (kind, plural or short name, optionally with group) to a resource.
func (c *Cluster) ResolveArg(arg string) (Resolved, error) {
	gvr, err := c.Mapper.ResourceFor(schema.ParseGroupResource(arg).WithVersion(""))
	if err != nil {
		return Resolved{}, err
	}
	gvk, err := c.Mapper.KindFor(gvr)
	if err != nil {
		return Resolved{}, err
	}
	m, err := c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{GVR: m.Resource, Kind: gvk.Kind, Namespaced: m.Scope.Name() == meta.RESTScopeNameNamespace}, nil
}

// Live describes the live state of an object.
type Live struct {
	Status           string `json:"status"`
	Error            string `json:"error,omitempty"`
	ReleaseName      string `json:"releaseName,omitempty"`
	ReleaseNamespace string `json:"releaseNamespace,omitempty"`
	ManagedBy        string `json:"managedBy,omitempty"`
}

// Get fetches an object and extracts its Helm ownership metadata.
func (c *Cluster) Get(ctx context.Context, r Resolved, namespace, name string) Live {
	var ri dynamic.ResourceInterface = c.Dynamic.Resource(r.GVR)
	if r.Namespaced {
		ri = c.Dynamic.Resource(r.GVR).Namespace(namespace)
	}
	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Live{Status: StatusAbsent}
	case err != nil:
		return Live{Status: StatusError, Error: err.Error()}
	}
	return liveFrom(obj)
}

func liveFrom(obj *unstructured.Unstructured) Live {
	ann := obj.GetAnnotations()
	return Live{
		Status:           StatusPresent,
		ReleaseName:      ann[AnnotationReleaseName],
		ReleaseNamespace: ann[AnnotationReleaseNamespace],
		ManagedBy:        obj.GetLabels()[LabelManagedBy],
	}
}

// Ownership checks whether a live object carries the metadata Helm requires to manage it as part of release/namespace.
func (l Live) Ownership(release, namespace string) string {
	if l.Status != StatusPresent {
		return ""
	}
	if l.ReleaseName == "" && l.ReleaseNamespace == "" {
		return OwnershipMissing
	}
	if l.ReleaseName != release || l.ReleaseNamespace != namespace || l.ManagedBy != "Helm" {
		return OwnershipMismatch
	}
	return OwnershipOK
}

// AdoptCommands prints kubectl commands that make the object adoptable by the release.
func AdoptCommands(kind, namespace, name, release, releaseNamespace string) []string {
	target := fmt.Sprintf("%s/%s", kind, name)
	nsFlag := ""
	if namespace != "" {
		nsFlag = " -n " + namespace
	}
	return []string{
		fmt.Sprintf("kubectl%s annotate %s %s=%s %s=%s --overwrite", nsFlag, target, AnnotationReleaseName, release, AnnotationReleaseNamespace, releaseNamespace),
		fmt.Sprintf("kubectl%s label %s %s=Helm --overwrite", nsFlag, target, LabelManagedBy),
	}
}
