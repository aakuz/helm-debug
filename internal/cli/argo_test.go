package cli

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/aakuz/helm-debug/internal/kube"
)

var applicationKind = kindInfo{schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}, "applications", true}

// argoRes builds one status.resources[] entry.
func argoRes(group, version, kind, ns, name, sync, health string, hook bool) map[string]any {
	r := map[string]any{"group": group, "version": version, "kind": kind, "namespace": ns, "name": name, "status": sync}
	if health != "" {
		r["health"] = map[string]any{"status": health}
	}
	if hook {
		r["hook"] = true
	}
	return r
}

// argoApp adds an Application with the given status and tracked resources to the fake cluster.
func (c *cluster) argoApp(name, ns, syncStatus, health string, spec map[string]any, resources ...map[string]any) {
	rs := make([]any, len(resources))
	for i, r := range resources {
		rs[i] = r
	}
	status := map[string]any{
		"sync":      map[string]any{"status": syncStatus, "revision": "abc1234"},
		"health":    map[string]any{"status": health},
		"resources": rs,
	}
	if health == "Degraded" {
		status["health"].(map[string]any)["message"] = "deployment web is not available"
		status["operationState"] = map[string]any{"phase": "Failed", "message": "one or more objects failed to apply"}
	}
	if spec == nil {
		spec = map[string]any{"source": map[string]any{"repoURL": "https://git.example.com/web.git", "path": "deploy", "targetRevision": "main"}}
	}
	c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     spec, "status": status,
	}})
}

// argoObject adds a tracked live object; annotations and labels are set as given.
func (c *cluster) argoObject(apiVersion, kind, ns, name string, annotations, labels map[string]string) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetAnnotations(annotations)
	u.SetLabels(labels)
	c.live = append(c.live, u)
}

func trackedBy(id string) map[string]string {
	return map[string]string{kube.AnnotationArgoTrackingID: id}
}

func argoCluster(t *testing.T) *cluster {
	c := newCluster(t)
	c.extraKinds = []kindInfo{applicationKind}
	return c
}

// healthyArgo is an in-sync Application whose two objects exist and are tracked by it.
func healthyArgo(t *testing.T) *cluster {
	c := argoCluster(t)
	c.argoApp("web", "argocd", "Synced", "Healthy", nil,
		argoRes("apps", "v1", "Deployment", "prod", "web", "Synced", "Healthy", false),
		argoRes("", "v1", "Service", "prod", "web", "Synced", "Healthy", false),
	)
	c.argoObject("apps/v1", "Deployment", "prod", "web", trackedBy("web:apps/Deployment:prod/web"), nil)
	c.argoObject("v1", "Service", "prod", "web", trackedBy("argocd_web:/Service:prod/web"), nil)
	return c
}

func TestArgoResourcesHealthy(t *testing.T) {
	out, err := run(t, healthyArgo(t), "argocd", "argo", "resources", "web")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestArgoResourcesJSON(t *testing.T) {
	out, err := run(t, healthyArgo(t), "argocd", "argo", "resources", "web", "-o", "json")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestArgoResourcesMultiSource(t *testing.T) {
	c := argoCluster(t)
	c.argoApp("web", "argocd", "Synced", "Healthy", map[string]any{"sources": []any{
		map[string]any{"repoURL": "https://charts.example.com", "chart": "web", "targetRevision": "1.2.3"},
		map[string]any{"repoURL": "https://git.example.com/values.git", "path": "envs/prod", "targetRevision": "main"},
	}})
	out, err := run(t, c, "argocd", "argo", "resources", "web")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestArgoResourcesProblems(t *testing.T) {
	tests := []struct {
		name  string
		setup func(c *cluster)
		want  bool
		show  string // text the table must contain
	}{
		{"out of sync app", func(c *cluster) {
			c.argoApp("web", "argocd", "OutOfSync", "Healthy", nil, argoRes("apps", "v1", "Deployment", "prod", "web", "Synced", "Healthy", false))
			c.argoObject("apps/v1", "Deployment", "prod", "web", trackedBy("web:apps/Deployment:prod/web"), nil)
		}, true, "OutOfSync"},
		{"degraded app", func(c *cluster) {
			c.argoApp("web", "argocd", "Synced", "Degraded", nil, argoRes("apps", "v1", "Deployment", "prod", "web", "Synced", "Degraded", false))
			c.argoObject("apps/v1", "Deployment", "prod", "web", trackedBy("web:apps/Deployment:prod/web"), nil)
		}, true, "deployment web is not available"},
		{"out of sync resource", func(c *cluster) {
			c.argoApp("web", "argocd", "Synced", "Healthy", nil, argoRes("apps", "v1", "Deployment", "prod", "web", "OutOfSync", "Healthy", false))
			c.argoObject("apps/v1", "Deployment", "prod", "web", trackedBy("web:apps/Deployment:prod/web"), nil)
		}, true, ""},
		{"missing health", func(c *cluster) {
			c.argoApp("web", "argocd", "Synced", "Healthy", nil, argoRes("apps", "v1", "Deployment", "prod", "web", "Synced", "Missing", false))
			c.argoObject("apps/v1", "Deployment", "prod", "web", trackedBy("web:apps/Deployment:prod/web"), nil)
		}, true, ""},
		{"absent object", func(c *cluster) {
			c.argoApp("web", "argocd", "Synced", "Healthy", nil, argoRes("apps", "v1", "Deployment", "prod", "web", "Synced", "Healthy", false))
		}, true, "absent"},
		{"tracking mismatch", func(c *cluster) {
			c.argoApp("web", "argocd", "Synced", "Healthy", nil, argoRes("apps", "v1", "Deployment", "prod", "web", "Synced", "Healthy", false))
			c.argoObject("apps/v1", "Deployment", "prod", "web", trackedBy("other:apps/Deployment:prod/web"), nil)
		}, true, "mismatch (other)"},
		{"label fallback", func(c *cluster) {
			c.argoApp("web", "argocd", "Synced", "Healthy", nil, argoRes("apps", "v1", "Deployment", "prod", "web", "Synced", "Healthy", false))
			c.argoObject("apps/v1", "Deployment", "prod", "web", nil, map[string]string{kube.LabelArgoInstance: "web"})
		}, false, ""},
		{"tracking missing", func(c *cluster) {
			c.argoApp("web", "argocd", "Synced", "Healthy", nil, argoRes("apps", "v1", "Deployment", "prod", "web", "Synced", "Healthy", false))
			c.argoObject("apps/v1", "Deployment", "prod", "web", nil, nil)
		}, true, "missing"},
		{"unknown kind", func(c *cluster) {
			c.argoApp("web", "argocd", "Synced", "Healthy", nil, argoRes("example.com", "v1", "Widget", "prod", "w", "Synced", "Healthy", false))
		}, true, "unknown-kind"},
		{"hook is never a problem", func(c *cluster) {
			c.argoApp("web", "argocd", "Synced", "Healthy", nil, argoRes("batch", "v1", "Job", "prod", "migrate", "OutOfSync", "Degraded", true))
		}, false, "migrate"},
		{"cluster-scoped", func(c *cluster) {
			c.argoApp("web", "argocd", "Synced", "Healthy", nil, argoRes("rbac.authorization.k8s.io", "v1", "ClusterRole", "", "web-reader", "Synced", "", false))
			c.argoObject("rbac.authorization.k8s.io/v1", "ClusterRole", "", "web-reader", trackedBy("web:rbac.authorization.k8s.io/ClusterRole:/web-reader"), nil)
		}, false, "ClusterRole"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := argoCluster(t)
			tt.setup(c)
			out, err := run(t, c, "argocd", "argo", "resources", "web")
			wantProblems(t, err, tt.want)
			if !strings.Contains(out, tt.show) {
				t.Fatalf("output lacks %q:\n%s", tt.show, out)
			}
		})
	}
}

func TestArgoResourcesCRDAbsent(t *testing.T) {
	_, err := run(t, newCluster(t), "argocd", "argo", "resources", "web")
	if err == nil || !strings.Contains(err.Error(), "Application CRD (argoproj.io) is not installed") {
		t.Fatalf("want CRD error, got %v", err)
	}
}

func TestArgoResourcesNotFound(t *testing.T) {
	_, err := run(t, argoCluster(t), "argocd", "argo", "resources", "web")
	if err == nil || err.Error() != "application argocd/web not found" {
		t.Fatalf("want not found error, got %v", err)
	}
}
