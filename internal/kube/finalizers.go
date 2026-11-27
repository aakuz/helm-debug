package kube

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Finalizer explains one metadata.finalizers entry of a terminating object.
type Finalizer struct {
	Name       string `json:"name"`
	WaitsFor   string `json:"waitsFor,omitempty"`   // set for well-known finalizers only
	Controller string `json:"controller,omitempty"` // set for other finalizers: the name prefix
}

var wellKnownFinalizers = map[string]string{
	"kubernetes.io/pvc-protection":               "no Pod to use the PersistentVolumeClaim any more",
	"kubernetes.io/pv-protection":                "no PersistentVolumeClaim to be bound to the PersistentVolume any more",
	"foregroundDeletion":                         "the garbage collector to delete the dependents that block owner deletion",
	"orphan":                                     "the garbage collector to remove this object from the ownerReferences of its dependents",
	"customresourcecleanup.apiextensions.k8s.io": "every custom resource of this CRD to be deleted",
}

// ExplainFinalizer says what a finalizer waits for, or which controller owns it.
func ExplainFinalizer(name string) Finalizer {
	if w, ok := wellKnownFinalizers[name]; ok {
		return Finalizer{Name: name, WaitsFor: w}
	}
	controller, _, _ := strings.Cut(name, "/")
	return Finalizer{Name: name, Controller: controller}
}

// ControllerHint is the word a controller's Deployment name usually starts with: the first DNS label of the
// prefix that is not "finalizer"/"finalizers" and does not end in "-finalizer". It is "" for the built-in Kubernetes
// controllers (kubernetes.io, k8s.io and their subdomains): they run in the control plane, not in a Deployment.
func ControllerHint(controller string) string {
	for _, d := range []string{"kubernetes.io", "k8s.io"} {
		if controller == d || strings.HasSuffix(controller, "."+d) {
			return ""
		}
	}
	for _, l := range strings.Split(controller, ".") {
		if l != "" && l != "finalizer" && l != "finalizers" && !strings.HasSuffix(l, "-finalizer") {
			return l
		}
	}
	return ""
}

// ListDeployments returns "namespace/name" of every Deployment in the cluster, via the metadata client.
func (c *Cluster) ListDeployments(ctx context.Context) ([]string, error) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	var out []string
	opts := metav1.ListOptions{Limit: 500}
	for {
		items, err := c.Metadata.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return nil, err
		}
		for _, it := range items.Items {
			out = append(out, it.Namespace+"/"+it.Name)
		}
		if items.Continue == "" {
			return out, nil
		}
		opts.Continue = items.Continue
	}
}

// RemoveFinalizersCommand prints (never runs) the kubectl command that clears the finalizers of an object.
func RemoveFinalizersCommand(kind, namespace, name string) string {
	nsFlag := ""
	if namespace != "" {
		nsFlag = " -n " + namespace
	}
	return fmt.Sprintf(`kubectl%s patch %s/%s --type=merge -p '{"metadata":{"finalizers":null}}'`, nsFlag, kind, name)
}
