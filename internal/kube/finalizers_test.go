package kube

import (
	"context"
	"errors"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestExplainFinalizer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want Finalizer
	}{
		{"kubernetes.io/pvc-protection", Finalizer{Name: "kubernetes.io/pvc-protection", WaitsFor: "no Pod to use the PersistentVolumeClaim any more"}},
		{"kubernetes.io/pv-protection", Finalizer{Name: "kubernetes.io/pv-protection", WaitsFor: "no PersistentVolumeClaim to be bound to the PersistentVolume any more"}},
		{"foregroundDeletion", Finalizer{Name: "foregroundDeletion", WaitsFor: "the garbage collector to delete the dependents that block owner deletion"}},
		{"orphan", Finalizer{Name: "orphan", WaitsFor: "the garbage collector to remove this object from the ownerReferences of its dependents"}},
		{"customresourcecleanup.apiextensions.k8s.io", Finalizer{Name: "customresourcecleanup.apiextensions.k8s.io", WaitsFor: "every custom resource of this CRD to be deleted"}},
		{"helm.toolkit.fluxcd.io/finalizer", Finalizer{Name: "helm.toolkit.fluxcd.io/finalizer", Controller: "helm.toolkit.fluxcd.io"}},
		{"example.com/cleanup", Finalizer{Name: "example.com/cleanup", Controller: "example.com"}},
		{"resources-finalizer.argocd.argoproj.io", Finalizer{Name: "resources-finalizer.argocd.argoproj.io", Controller: "resources-finalizer.argocd.argoproj.io"}},
		{"finalizer.cert-manager.io", Finalizer{Name: "finalizer.cert-manager.io", Controller: "finalizer.cert-manager.io"}},
		{"a.io/b/c", Finalizer{Name: "a.io/b/c", Controller: "a.io"}},
		{"kubernetes.io/other", Finalizer{Name: "kubernetes.io/other", Controller: "kubernetes.io"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ExplainFinalizer(tt.name); got != tt.want {
				t.Errorf("ExplainFinalizer(%q) = %+v, want %+v", tt.name, got, tt.want)
			}
		})
	}
}

func TestControllerHint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		controller, want string
	}{
		{"helm.toolkit.fluxcd.io", "helm"},
		{"resources-finalizer.argocd.argoproj.io", "argocd"},
		{"finalizer.cert-manager.io", "cert-manager"},
		{"finalizers.cert-manager.io", "cert-manager"},
		{"example.com", "example"},
		{"kustomize", "kustomize"},
		{"", ""},
		{"finalizer", ""},
		{"finalizers.foo-finalizer", ""},
	}
	for _, tt := range tests {
		t.Run(tt.controller, func(t *testing.T) {
			t.Parallel()
			if got := ControllerHint(tt.controller); got != tt.want {
				t.Errorf("ControllerHint(%q) = %q, want %q", tt.controller, got, tt.want)
			}
		})
	}
}

func TestRemoveFinalizersCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, kind, namespace, obj, want string
	}{
		{"namespaced", "PersistentVolumeClaim", "prod", "data", `kubectl -n prod patch PersistentVolumeClaim/data --type=merge -p '{"metadata":{"finalizers":null}}'`},
		{"cluster-scoped", "Widget", "", "backend-widget", `kubectl patch Widget/backend-widget --type=merge -p '{"metadata":{"finalizers":null}}'`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := RemoveFinalizersCommand(tt.kind, tt.namespace, tt.obj); got != tt.want {
				t.Errorf("RemoveFinalizersCommand = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListDeployments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		forbid  bool
		want    []string
		wantErr bool
	}{
		{name: "all namespaces", want: []string{"flux-system/helm-controller", "prod/api"}},
		{name: "forbidden", forbid: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newManagedCluster(t,
				partial("apps/v1", "Deployment", "prod", "api", nil, nil),
				partial("apps/v1", "Deployment", "flux-system", "helm-controller", nil, nil),
				partial("v1", "ConfigMap", "prod", "not-a-deployment", nil, nil),
			)
			if tt.forbid {
				c.Metadata.(*metadatafake.FakeMetadataClient).PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "", errors.New("no list"))
				})
			}
			got, err := c.ListDeployments(context.Background())
			if tt.wantErr {
				if !apierrors.IsForbidden(err) {
					t.Fatalf("want a forbidden error, got %v (result %v)", err, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("ListDeployments = %v, want %v", got, tt.want)
			}
		})
	}
}
