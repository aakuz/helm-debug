package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
)

var (
	pvcKind    = kindInfo{schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaim"}, "persistentvolumeclaims", true}
	widgetKind = kindInfo{schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}, "widgets", false}
)

// terminate marks the last live object as being deleted ago before now, held by finalizers.
func (c *cluster) terminate(ago time.Duration, finalizers ...string) {
	u := c.live[len(c.live)-1].(*unstructured.Unstructured)
	ts := metav1.NewTime(now.Add(-ago))
	u.SetDeletionTimestamp(&ts)
	u.SetFinalizers(finalizers)
}

const terminatingManifest = `---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
---
apiVersion: v1
kind: Service
metadata:
  name: web
---
apiVersion: example.com/v1
kind: Widget
metadata:
  name: backend-widget
`

// terminatingCluster has a release whose objects are deleted but held: a PVC by pvc-protection, a Service by a Flux
// finalizer whose controller runs, a Widget by a finalizer whose controller is gone, and a Deployment by nothing.
func terminatingCluster(t *testing.T) *cluster {
	c := newCluster(t)
	c.extraKinds = []kindInfo{pvcKind, widgetKind}
	c.release("app", "prod", 1, common.StatusDeployed, terminatingManifest)
	c.object("v1", "ConfigMap", "prod", "config", "app", "prod")
	c.object("apps/v1", "Deployment", "prod", "api", "app", "prod")
	c.terminate(10 * time.Minute)
	c.object("v1", "PersistentVolumeClaim", "prod", "data", "app", "prod")
	c.terminate(3*time.Hour, "kubernetes.io/pvc-protection")
	c.object("v1", "Service", "prod", "web", "app", "prod")
	c.terminate(5*time.Minute, "helm.toolkit.fluxcd.io/finalizer")
	c.object("example.com/v1", "Widget", "", "backend-widget", "app", "prod")
	c.terminate(50*time.Hour, "example.com/cleanup")
	// The Flux helm-controller, not part of the release.
	c.object("apps/v1", "Deployment", "flux-system", "helm-controller", "", "")
	return c
}

// runWithMetadata is run with a hook that can add reactors to the fake metadata client.
func runWithMetadata(t *testing.T, c *cluster, hook func(*metadatafake.FakeMetadataClient), args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut, o.Now = &out, &errOut, func() time.Time { return now }
	o.NewClients = func(*Options) (*Clients, error) {
		cl := c.clients("prod")
		hook(cl.Cluster.Metadata.(*metadatafake.FakeMetadataClient))
		return cl, nil
	}
	cmd := NewRootCommand(o)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// countDeploymentLists counts lists of Deployments through the metadata client without changing their result.
func countDeploymentLists(n *atomic.Int32) func(*metadatafake.FakeMetadataClient) {
	return func(m *metadatafake.FakeMetadataClient) {
		m.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
			n.Add(1)
			return false, nil, nil
		})
	}
}

func forbidDeploymentLists(m *metadatafake.FakeMetadataClient) {
	m.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "", errors.New("no list"))
	})
}

type terminatingJSON struct {
	Resources []struct {
		Kind       string     `json:"kind"`
		Namespace  string     `json:"namespace"`
		Name       string     `json:"name"`
		Status     string     `json:"status"`
		Ownership  string     `json:"ownership"`
		Deleted    *time.Time `json:"deletionTimestamp"`
		Finalizers []struct {
			Name        string   `json:"name"`
			WaitsFor    string   `json:"waitsFor"`
			Controller  string   `json:"controller"`
			Deployments []string `json:"deployments"`
		} `json:"finalizers"`
	} `json:"resources"`
	FinalizerFixes   []string `json:"finalizerFixes"`
	DeploymentsError string   `json:"deploymentsError"`
}

func decodeTerminating(t *testing.T, out string) terminatingJSON {
	t.Helper()
	var res terminatingJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	return res
}

func TestResourcesTerminating(t *testing.T) {
	t.Parallel()
	out, err := run(t, terminatingCluster(t), "prod", "resources", "app")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestResourcesTerminatingJSON(t *testing.T) {
	t.Parallel()
	out, err := run(t, terminatingCluster(t), "prod", "resources", "app", "-o", "json")
	wantProblems(t, err, true)
	if strings.Contains(out, "deploymentsError") {
		t.Errorf("deploymentsError set although the list succeeded:\n%s", out)
	}
	res := decodeTerminating(t, out)
	if len(res.Resources) != 5 {
		t.Fatalf("resources = %+v", res.Resources)
	}
	byName := map[string]int{}
	for i, r := range res.Resources {
		byName[r.Kind+"/"+r.Name] = i
	}

	cm := res.Resources[byName["ConfigMap/config"]]
	if cm.Status != "present" || cm.Deleted != nil || len(cm.Finalizers) != 0 {
		t.Errorf("ConfigMap row = %+v", cm)
	}

	dep := res.Resources[byName["Deployment/api"]]
	if dep.Status != "terminating" || dep.Deleted == nil || !dep.Deleted.Equal(now.Add(-10*time.Minute)) || len(dep.Finalizers) != 0 {
		t.Errorf("Deployment row = %+v", dep)
	}
	if dep.Ownership != "ok" {
		t.Errorf("ownership of a terminating object = %q, want ok", dep.Ownership)
	}

	pvc := res.Resources[byName["PersistentVolumeClaim/data"]]
	if pvc.Status != "terminating" || pvc.Deleted == nil || !pvc.Deleted.Equal(now.Add(-3*time.Hour)) || len(pvc.Finalizers) != 1 {
		t.Fatalf("PVC row = %+v", pvc)
	}
	if f := pvc.Finalizers[0]; f.Name != "kubernetes.io/pvc-protection" || f.WaitsFor != "no Pod to use the PersistentVolumeClaim any more" || f.Controller != "" || len(f.Deployments) != 0 {
		t.Errorf("PVC finalizer = %+v", f)
	}

	svc := res.Resources[byName["Service/web"]]
	if svc.Status != "terminating" || svc.Deleted == nil || !svc.Deleted.Equal(now.Add(-5*time.Minute)) || len(svc.Finalizers) != 1 {
		t.Fatalf("Service row = %+v", svc)
	}
	if f := svc.Finalizers[0]; f.Name != "helm.toolkit.fluxcd.io/finalizer" || f.WaitsFor != "" || f.Controller != "helm.toolkit.fluxcd.io" || !slices.Equal(f.Deployments, []string{"flux-system/helm-controller"}) {
		t.Errorf("Service finalizer = %+v", f)
	}

	w := res.Resources[byName["Widget/backend-widget"]]
	if w.Status != "terminating" || w.Namespace != "" || w.Deleted == nil || !w.Deleted.Equal(now.Add(-50*time.Hour)) || len(w.Finalizers) != 1 {
		t.Fatalf("Widget row = %+v", w)
	}
	if f := w.Finalizers[0]; f.Name != "example.com/cleanup" || f.Controller != "example.com" || len(f.Deployments) != 0 {
		t.Errorf("Widget finalizer = %+v", f)
	}

	// The PVC is held only by a well-known finalizer, so it gets no patch command.
	wantFixes := []string{
		`kubectl -n prod patch Service/web --type=merge -p '{"metadata":{"finalizers":null}}'`,
		`kubectl patch Widget/backend-widget --type=merge -p '{"metadata":{"finalizers":null}}'`,
	}
	if !slices.Equal(res.FinalizerFixes, wantFixes) {
		t.Errorf("finalizerFixes = %q, want %q", res.FinalizerFixes, wantFixes)
	}
}

func TestResourcesTerminatingHook(t *testing.T) {
	t.Parallel()
	hookCluster := func(t *testing.T, finalizers ...string) *cluster {
		hook := &rspb.Hook{
			Name: "migrate", Kind: "Job", Path: "api/templates/migrate.yaml",
			Events:   []rspb.HookEvent{rspb.HookPreUpgrade},
			Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: migrate\n",
		}
		c := newCluster(t)
		c.release("api", "prod", 1, common.StatusDeployed, "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n", hook)
		c.object("apps/v1", "Deployment", "prod", "api", "api", "prod")
		c.object("batch/v1", "Job", "prod", "migrate", "", "")
		c.terminate(time.Hour, finalizers...)
		return c
	}

	t.Run("controller finalizer", func(t *testing.T) {
		t.Parallel()
		out, err := run(t, hookCluster(t, "example.com/cleanup"), "prod", "resources", "api")
		wantProblems(t, err, false)
		for _, want := range []string{
			"terminating",
			"Terminating objects:\n  Job prod/migrate: deletion requested 1h ago\n    example.com/cleanup: set by controller example.com; Deployments named example*: none - the controller may be uninstalled\n",
			`  kubectl -n prod patch Job/migrate --type=merge -p '{"metadata":{"finalizers":null}}'`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("only well-known finalizers", func(t *testing.T) {
		t.Parallel()
		out, err := run(t, hookCluster(t, "foregroundDeletion"), "prod", "resources", "api")
		wantProblems(t, err, false)
		want := "Terminating objects:\n  Job prod/migrate: deletion requested 1h ago\n    foregroundDeletion: waits for: the garbage collector to delete the dependents that block owner deletion\n"
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
		if strings.Contains(out, "Clearing finalizers") || strings.Contains(out, "kubectl") {
			t.Errorf("a row held only by well-known finalizers gets no patch command:\n%s", out)
		}
	})
}

func TestResourcesOnlyTerminating(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n")
	c.object("apps/v1", "Deployment", "prod", "api", "api", "prod")
	c.terminate(2 * time.Minute)

	out, err := run(t, c, "prod", "resources", "api")
	wantProblems(t, err, true)
	if strings.Contains(out, "none of the declared objects exist") {
		t.Errorf("a terminating object counts as existing:\n%s", out)
	}
	if !strings.Contains(out, "  Deployment prod/api: deletion requested 2m ago\n    (no finalizers - waiting for the API server or the garbage collector)\n") {
		t.Errorf("output lacks the terminating block:\n%s", out)
	}
	if strings.Contains(out, "Clearing finalizers") {
		t.Errorf("no finalizers to clear:\n%s", out)
	}
}

func TestResourcesUninstalling(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusUninstalling, "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n---\napiVersion: v1\nkind: Service\nmetadata:\n  name: api\n")
	c.object("apps/v1", "Deployment", "prod", "api", "api", "prod")
	c.terminate(time.Minute, "foregroundDeletion")

	out, err := run(t, c, "prod", "resources", "api", "-o", "json")
	wantProblems(t, err, true)
	res := decodeTerminating(t, out)
	if len(res.Resources) != 2 {
		t.Fatalf("resources = %+v", res.Resources)
	}
	for _, r := range res.Resources {
		want := map[string]string{"Deployment": "terminating", "Service": "absent"}[r.Kind]
		if r.Status != want {
			t.Errorf("%s status = %q, want %q", r.Kind, r.Status, want)
		}
	}
}

func TestResourcesDeploymentLookups(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		finalizers []string
		wantLists  int32
	}{
		{"well-known finalizers need no controller", []string{"kubernetes.io/pvc-protection"}, 0},
		{"no finalizers need no controller", nil, 0},
		{"controller finalizer with no hint", []string{"finalizer/x"}, 0},
		{"several controllers share one list", []string{"example.com/cleanup", "helm.toolkit.fluxcd.io/finalizer"}, 1},
		{"built-in kubernetes domain needs no controller", []string{"service.kubernetes.io/load-balancer-cleanup"}, 0},
		{"built-in k8s.io domain needs no controller", []string{"foo.k8s.io/cleanup"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newCluster(t)
			c.extraKinds = []kindInfo{pvcKind}
			c.release("app", "prod", 1, common.StatusDeployed, "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: data\n---\napiVersion: v1\nkind: Service\nmetadata:\n  name: web\n")
			c.object("v1", "PersistentVolumeClaim", "prod", "data", "app", "prod")
			c.terminate(time.Hour, tt.finalizers...)
			c.object("v1", "Service", "prod", "web", "app", "prod")
			c.terminate(time.Hour, tt.finalizers...)

			var lists atomic.Int32
			_, err := runWithMetadata(t, c, countDeploymentLists(&lists), "resources", "app")
			wantProblems(t, err, true)
			if got := lists.Load(); got != tt.wantLists {
				t.Errorf("Deployments listed %d times, want %d", got, tt.wantLists)
			}
		})
	}
}

func TestResourcesDeploymentsForbidden(t *testing.T) {
	t.Parallel()
	t.Run("text", func(t *testing.T) {
		t.Parallel()
		out, err := runWithMetadata(t, terminatingCluster(t), forbidDeploymentLists, "resources", "app")
		wantProblems(t, err, true)
		for _, want := range []string{
			"Terminating objects:\n",
			"    example.com/cleanup: set by controller example.com; Deployments named example*: not checked (",
			"    helm.toolkit.fluxcd.io/finalizer: set by controller helm.toolkit.fluxcd.io; Deployments named helm*: not checked (",
			"    kubernetes.io/pvc-protection: waits for: no Pod to use the PersistentVolumeClaim any more\n",
			"Clearing finalizers (last resort",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if !strings.Contains(out, "forbidden") {
			t.Errorf("output does not name the error:\n%s", out)
		}
	})
	t.Run("json", func(t *testing.T) {
		t.Parallel()
		out, err := runWithMetadata(t, terminatingCluster(t), forbidDeploymentLists, "resources", "app", "-o", "json")
		wantProblems(t, err, true)
		res := decodeTerminating(t, out)
		if !strings.Contains(res.DeploymentsError, "forbidden") {
			t.Errorf("deploymentsError = %q", res.DeploymentsError)
		}
		for _, r := range res.Resources {
			for _, f := range r.Finalizers {
				if len(f.Deployments) != 0 {
					t.Errorf("%s/%s finalizer %s has deployments %v although the list failed", r.Kind, r.Name, f.Name, f.Deployments)
				}
			}
		}
		if len(res.FinalizerFixes) != 2 {
			t.Errorf("finalizerFixes = %q", res.FinalizerFixes)
		}
	})
}

func TestResourcesBuiltinControllerFinalizer(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	c.release("app", "prod", 1, common.StatusDeployed, "apiVersion: v1\nkind: Service\nmetadata:\n  name: web\n")
	c.object("v1", "Service", "prod", "web", "app", "prod")
	c.terminate(time.Hour, "service.kubernetes.io/load-balancer-cleanup")
	c.object("apps/v1", "Deployment", "kube-system", "service-controller", "", "")

	out, err := run(t, c, "prod", "resources", "app")
	wantProblems(t, err, true)
	for _, want := range []string{
		"    service.kubernetes.io/load-balancer-cleanup: set by controller service.kubernetes.io\n",
		`  kubectl -n prod patch Service/web --type=merge -p '{"metadata":{"finalizers":null}}'`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Deployments named") {
		t.Errorf("a built-in controller is not looked up among Deployments:\n%s", out)
	}
}

func TestResourcesDeploymentMatching(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		deployments []string // "namespace/name" of live Deployments besides the release
		want        []string
	}{
		{"hint followed by a dash", []string{"flux-system/helm-controller", "ci/helmfile-runner"}, []string{"flux-system/helm-controller"}},
		{"name equal to the hint", []string{"tools/helm", "ci/helmfile-runner"}, []string{"tools/helm"}},
		{"only a longer word", []string{"ci/helmfile-runner"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newCluster(t)
			c.release("app", "prod", 1, common.StatusDeployed, "apiVersion: v1\nkind: Service\nmetadata:\n  name: web\n")
			c.object("v1", "Service", "prod", "web", "app", "prod")
			c.terminate(time.Hour, "helm.toolkit.fluxcd.io/finalizer")
			for _, d := range tt.deployments {
				ns, name, _ := strings.Cut(d, "/")
				c.object("apps/v1", "Deployment", ns, name, "", "")
			}

			out, err := run(t, c, "prod", "resources", "app", "-o", "json")
			wantProblems(t, err, true)
			res := decodeTerminating(t, out)
			if len(res.Resources) != 1 || len(res.Resources[0].Finalizers) != 1 {
				t.Fatalf("resources = %+v", res.Resources)
			}
			got := res.Resources[0].Finalizers[0].Deployments
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("deployments = %v, want %v", got, tt.want)
			}
		})
	}
}
