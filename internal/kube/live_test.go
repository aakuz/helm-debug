package kube

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestOwnership(t *testing.T) {
	tests := []struct {
		name string
		live Live
		want string
	}{
		{"absent", Live{Status: StatusAbsent, ReleaseName: "r", ReleaseNamespace: "ns", ManagedBy: "Helm"}, ""},
		{"error", Live{Status: StatusError}, ""},
		{"unknown kind", Live{Status: StatusUnknownKind}, ""},
		{"ok", Live{Status: StatusPresent, ReleaseName: "r", ReleaseNamespace: "ns", ManagedBy: "Helm"}, OwnershipOK},
		{"missing", Live{Status: StatusPresent}, OwnershipMissing},
		{"missing even with managed-by", Live{Status: StatusPresent, ManagedBy: "Helm"}, OwnershipMissing},
		{"mismatch name", Live{Status: StatusPresent, ReleaseName: "other", ReleaseNamespace: "ns", ManagedBy: "Helm"}, OwnershipMismatch},
		{"mismatch namespace", Live{Status: StatusPresent, ReleaseName: "r", ReleaseNamespace: "other", ManagedBy: "Helm"}, OwnershipMismatch},
		{"mismatch managed-by", Live{Status: StatusPresent, ReleaseName: "r", ReleaseNamespace: "ns", ManagedBy: "kustomize"}, OwnershipMismatch},
		{"mismatch no managed-by", Live{Status: StatusPresent, ReleaseName: "r", ReleaseNamespace: "ns"}, OwnershipMismatch},
		{"only name set", Live{Status: StatusPresent, ReleaseName: "r", ManagedBy: "Helm"}, OwnershipMismatch},
		{"only namespace set", Live{Status: StatusPresent, ReleaseNamespace: "ns", ManagedBy: "Helm"}, OwnershipMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.live.Ownership("r", "ns"); got != tt.want {
				t.Fatalf("Ownership = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAdoptCommands(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		want      []string
	}{
		{"namespaced", "prod", []string{
			"kubectl -n prod annotate Deployment/web meta.helm.sh/release-name=rel meta.helm.sh/release-namespace=relns --overwrite",
			"kubectl -n prod label Deployment/web app.kubernetes.io/managed-by=Helm --overwrite",
		}},
		{"cluster-scoped", "", []string{
			"kubectl annotate Deployment/web meta.helm.sh/release-name=rel meta.helm.sh/release-namespace=relns --overwrite",
			"kubectl label Deployment/web app.kubernetes.io/managed-by=Helm --overwrite",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AdoptCommands("Deployment", tt.namespace, "web", "rel", "relns")
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func newObj(apiVersion, kind, namespace, name string, annotations, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"name": name}}}
	if namespace != "" {
		u.SetNamespace(namespace)
	}
	if annotations != nil {
		u.SetAnnotations(annotations)
	}
	if labels != nil {
		u.SetLabels(labels)
	}
	return u
}

func TestLiveFrom(t *testing.T) {
	t.Run("with ownership metadata", func(t *testing.T) {
		u := newObj("v1", "ConfigMap", "ns", "a",
			map[string]string{AnnotationReleaseName: "r", AnnotationReleaseNamespace: "rns"},
			map[string]string{LabelManagedBy: "Helm"})
		l := liveFrom(u)
		want := Live{Status: StatusPresent, Object: u, ReleaseName: "r", ReleaseNamespace: "rns", ManagedBy: "Helm"}
		if !reflect.DeepEqual(l, want) {
			t.Fatalf("got %+v, want %+v", l, want)
		}
	})
	t.Run("without metadata", func(t *testing.T) {
		u := newObj("v1", "ConfigMap", "ns", "a", nil, nil)
		l := liveFrom(u)
		if l.Status != StatusPresent || l.Object != u || l.ReleaseName != "" || l.ReleaseNamespace != "" || l.ManagedBy != "" {
			t.Fatalf("got %+v", l)
		}
	})
}

var (
	cmGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	nsGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
)

func fakeDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		cmGVR: "ConfigMapList",
		nsGVR: "NamespaceList",
	}, objs...)
}

func TestGet(t *testing.T) {
	ctx := context.Background()
	cm := newObj("v1", "ConfigMap", "ns", "a",
		map[string]string{AnnotationReleaseName: "r", AnnotationReleaseNamespace: "ns"},
		map[string]string{LabelManagedBy: "Helm"})
	clusterObj := newObj("v1", "Namespace", "", "team", nil, nil)
	cmRes := Resolved{GVR: cmGVR, Kind: "ConfigMap", Namespaced: true}
	nsRes := Resolved{GVR: nsGVR, Kind: "Namespace"}

	t.Run("present namespaced", func(t *testing.T) {
		l := (&Cluster{Dynamic: fakeDynamic(cm)}).Get(ctx, cmRes, "ns", "a")
		if l.Status != StatusPresent || l.Object == nil || l.Object.GetName() != "a" {
			t.Fatalf("got %+v", l)
		}
		if l.ReleaseName != "r" || l.ReleaseNamespace != "ns" || l.ManagedBy != "Helm" {
			t.Fatalf("ownership not extracted: %+v", l)
		}
		if l.Ownership("r", "ns") != OwnershipOK {
			t.Fatal("expected ownership ok")
		}
	})
	t.Run("present cluster-scoped ignores namespace", func(t *testing.T) {
		l := (&Cluster{Dynamic: fakeDynamic(clusterObj)}).Get(ctx, nsRes, "ignored", "team")
		if l.Status != StatusPresent || l.Object.GetName() != "team" {
			t.Fatalf("got %+v", l)
		}
	})
	t.Run("absent: wrong name", func(t *testing.T) {
		l := (&Cluster{Dynamic: fakeDynamic(cm)}).Get(ctx, cmRes, "ns", "missing")
		if l.Status != StatusAbsent || l.Object != nil || l.Error != "" {
			t.Fatalf("got %+v", l)
		}
	})
	t.Run("absent: wrong namespace", func(t *testing.T) {
		l := (&Cluster{Dynamic: fakeDynamic(cm)}).Get(ctx, cmRes, "other", "a")
		if l.Status != StatusAbsent {
			t.Fatalf("got %+v", l)
		}
	})
	t.Run("other error becomes StatusError", func(t *testing.T) {
		dc := fakeDynamic()
		dc.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(cmGVR.GroupResource(), "a", errors.New("no access"))
		})
		l := (&Cluster{Dynamic: dc}).Get(ctx, cmRes, "ns", "a")
		if l.Status != StatusError || l.Object != nil || !strings.Contains(l.Error, "forbidden") {
			t.Fatalf("got %+v", l)
		}
	})
	t.Run("plain error becomes StatusError", func(t *testing.T) {
		dc := fakeDynamic()
		dc.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("boom")
		})
		l := (&Cluster{Dynamic: dc}).Get(ctx, cmRes, "ns", "a")
		if l.Status != StatusError || l.Error != "boom" {
			t.Fatalf("got %+v", l)
		}
	})
	t.Run("reactor NotFound is absent", func(t *testing.T) {
		dc := fakeDynamic()
		dc.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(cmGVR.GroupResource(), "a")
		})
		l := (&Cluster{Dynamic: dc}).Get(ctx, cmRes, "ns", "a")
		if l.Status != StatusAbsent {
			t.Fatalf("got %+v", l)
		}
	})
	t.Run("request targets namespace and name", func(t *testing.T) {
		dc := fakeDynamic(cm)
		(&Cluster{Dynamic: dc}).Get(ctx, cmRes, "ns", "a")
		acts := dc.Actions()
		if len(acts) != 1 {
			t.Fatalf("actions = %v", acts)
		}
		get, ok := acts[0].(clienttesting.GetAction)
		if !ok || get.GetName() != "a" || get.GetNamespace() != "ns" || get.GetResource() != cmGVR {
			t.Fatalf("action = %#v", acts[0])
		}
	})
}

func testMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	m.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, meta.RESTScopeRoot)
	return m
}

func TestResolve(t *testing.T) {
	c := &Cluster{Mapper: testMapper()}
	tests := []struct {
		name, apiVersion, kind string
		want                   Resolved
		wantErr                bool
	}{
		{"namespaced core", "v1", "ConfigMap", Resolved{GVR: cmGVR, Kind: "ConfigMap", Namespaced: true}, false},
		{"namespaced grouped", "apps/v1", "Deployment", Resolved{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, Kind: "Deployment", Namespaced: true}, false},
		{"cluster-scoped core", "v1", "Namespace", Resolved{GVR: nsGVR, Kind: "Namespace"}, false},
		{"cluster-scoped grouped", "rbac.authorization.k8s.io/v1", "ClusterRole", Resolved{GVR: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, Kind: "ClusterRole"}, false},
		{"unknown kind", "v1", "Widget", Resolved{}, true},
		{"unknown group", "example.com/v1", "Deployment", Resolved{}, true},
		{"unknown version", "apps/v9", "Deployment", Resolved{}, true},
		{"bad apiVersion", "a/b/c", "Deployment", Resolved{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.Resolve(tt.apiVersion, tt.kind)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
	t.Run("unknown kind is a no-match error", func(t *testing.T) {
		_, err := c.Resolve("v1", "Widget")
		if !meta.IsNoMatchError(err) {
			t.Fatalf("err = %v, want NoMatch", err)
		}
	})
}

func TestResolveArg(t *testing.T) {
	c := &Cluster{Mapper: testMapper()}
	deploy := Resolved{GVR: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, Kind: "Deployment", Namespaced: true}
	cm := Resolved{GVR: cmGVR, Kind: "ConfigMap", Namespaced: true}
	tests := []struct {
		arg     string
		want    Resolved
		wantErr bool
	}{
		{"deployments", deploy, false},
		{"deployment", deploy, false},
		{"Deployment", deploy, false},
		{"deployments.apps", deploy, false},
		{"deployment.apps", deploy, false},
		{"configmaps", cm, false},
		{"configmap", cm, false},
		{"namespaces", Resolved{GVR: nsGVR, Kind: "Namespace"}, false},
		{"clusterroles.rbac.authorization.k8s.io", Resolved{GVR: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, Kind: "ClusterRole"}, false},
		{"clusterrole", Resolved{GVR: schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, Kind: "ClusterRole"}, false},
		{"widgets", Resolved{}, true},
		{"deployments.wrong", Resolved{}, true},
		{"", Resolved{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			got, err := c.ResolveArg(tt.arg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
