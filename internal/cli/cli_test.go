package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/aakuz/helm-debug/internal/helm"
	"github.com/aakuz/helm-debug/internal/kube"
)

var update = flag.Bool("update", false, "rewrite golden files")

var now = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

type kindInfo struct {
	gvk        schema.GroupVersionKind
	resource   string
	namespaced bool
}

var kinds = []kindInfo{
	{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, "deployments", true},
	{schema.GroupVersionKind{Version: "v1", Kind: "Service"}, "services", true},
	{schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "configmaps", true},
	{schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, "secrets", true},
	{schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}, "jobs", true},
	{schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, "clusterroles", false},
}

// cluster is a fake cluster: release records go to the storage, live objects to the dynamic client.
type cluster struct {
	t    *testing.T
	kube *kubefake.Clientset
	live []runtime.Object
	// logs maps "pod/container" (suffixed "!previous" for a previous run) to log text.
	logs     map[string]string
	logCalls []string
	logTail  int64
	// logTails maps a log key to the number of lines asked for in its last request.
	logTails map[string]int64
	// logErrors maps a log key to an error text returned by the fake reader.
	logErrors map[string]string
	logMu     sync.Mutex
	// serverVersion is the Kubernetes version the fake discovery client reports; empty means it reports none.
	serverVersion string
	// extraKinds are served by the fake API server in addition to kinds.
	extraKinds []kindInfo
}

func newCluster(t *testing.T) *cluster {
	return &cluster{t: t, kube: kubefake.NewClientset()}
}

func (c *cluster) release(name, ns string, version int, status common.Status, manifest string, hooks ...*rspb.Hook) {
	c.t.Helper()
	c.releaseWithValues(name, ns, version, status, manifest, nil, hooks...)
}

func (c *cluster) releaseWithValues(name, ns string, version int, status common.Status, manifest string, values map[string]any, hooks ...*rspb.Hook) {
	c.t.Helper()
	c.releaseStoredIn(ns, name, ns, version, status, manifest, values, hooks...)
}

// releaseStoredIn keeps the record in storageNs while the release itself belongs to ns, as with `helm install --namespace ns`
// under a client whose storage namespace differs.
func (c *cluster) releaseStoredIn(storageNs, name, ns string, version int, status common.Status, manifest string, values map[string]any, hooks ...*rspb.Hook) {
	c.t.Helper()
	rel := &rspb.Release{
		Name: name, Namespace: ns, Version: version, Manifest: manifest, Hooks: hooks, Config: values,
		Info: &rspb.Info{
			Status:       status,
			LastDeployed: now.Add(-time.Duration(10-version) * time.Hour),
			Description:  map[common.Status]string{common.StatusFailed: "Upgrade \"" + name + "\" failed: timed out waiting for the condition"}[status],
		},
		Chart: &chart.Chart{Metadata: &chart.Metadata{Name: name, Version: fmt.Sprintf("1.%d.0", version), AppVersion: "2.0"}},
	}
	key := fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, version)
	if err := driver.NewSecrets(c.kube.CoreV1().Secrets(storageNs)).Create(key, rel); err != nil {
		c.t.Fatal(err)
	}
}

func (c *cluster) object(apiVersion, kind, ns, name, release, releaseNs string) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	if release != "" {
		u.SetAnnotations(map[string]string{kube.AnnotationReleaseName: release, kube.AnnotationReleaseNamespace: releaseNs})
		u.SetLabels(map[string]string{kube.LabelManagedBy: "Helm"})
	}
	c.live = append(c.live, u)
}

func (c *cluster) clients(namespace string) *Clients {
	c.t.Helper()
	secrets, err := c.kube.CoreV1().Secrets("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
	var metas []runtime.Object
	for _, s := range secrets.Items {
		metas = append(metas, &metav1.PartialObjectMetadata{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: s.ObjectMeta,
		})
	}
	for _, o := range c.live {
		u := o.(*unstructured.Unstructured)
		metas = append(metas, &metav1.PartialObjectMetadata{
			TypeMeta:   metav1.TypeMeta{APIVersion: u.GetAPIVersion(), Kind: u.GetKind()},
			ObjectMeta: metav1.ObjectMeta{Namespace: u.GetNamespace(), Name: u.GetName(), Labels: u.GetLabels(), Annotations: u.GetAnnotations()},
		})
	}
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		c.t.Fatal(err)
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	listKinds := map[schema.GroupVersionResource]string{}
	var resources []*metav1.APIResourceList
	for _, k := range append(slices.Clone(kinds), c.extraKinds...) {
		res := metav1.APIResource{Name: k.resource, Kind: k.gvk.Kind, Namespaced: k.namespaced, Verbs: metav1.Verbs{"get", "list"}}
		if i := slices.IndexFunc(resources, func(l *metav1.APIResourceList) bool { return l.GroupVersion == k.gvk.GroupVersion().String() }); i >= 0 {
			resources[i].APIResources = append(resources[i].APIResources, res)
		} else {
			resources = append(resources, &metav1.APIResourceList{GroupVersion: k.gvk.GroupVersion().String(), APIResources: []metav1.APIResource{res}})
		}
		scope := meta.RESTScopeRoot
		if k.namespaced {
			scope = meta.RESTScopeNamespace
		}
		mapper.AddSpecific(k.gvk, k.gvk.GroupVersion().WithResource(k.resource), k.gvk.GroupVersion().WithResource(strings.ToLower(k.gvk.Kind)), scope)
		listKinds[k.gvk.GroupVersion().WithResource(k.resource)] = k.gvk.Kind + "List"
	}
	listKinds[schema.GroupVersionResource{Version: "v1", Resource: "pods"}] = "PodList"
	listKinds[schema.GroupVersionResource{Version: "v1", Resource: "events"}] = "EventList"
	metaClient := metadatafake.NewSimpleMetadataClient(scheme, metas...)
	disc := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}
	if c.serverVersion != "" {
		disc.FakedServerVersion = &version.Info{GitVersion: c.serverVersion}
	}
	return &Clients{
		Store: &helm.Store{
			Kube:     c.kube,
			Metadata: metaClient,
			Driver:   helm.StorageSecret,
		},
		Cluster: &kube.Cluster{
			Dynamic:   dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, c.live...),
			Mapper:    mapper,
			Discovery: disc,
			Metadata:  metaClient,
		},
		Namespace: namespace,
		Logs:      c.readLog,
	}
}

// run executes the CLI against the fake cluster and returns stdout and the error.
func run(t *testing.T, c *cluster, namespace string, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut, o.Now = &out, &errOut, func() time.Time { return now }
	o.NewClients = func(*Options) (*Clients, error) { return c.clients(namespace), nil }
	cmd := NewRootCommand(o)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func golden(t *testing.T, got string) {
	t.Helper()
	path := filepath.Join("testdata", strings.ReplaceAll(t.Name(), "/", "_")+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if string(want) != got {
		t.Errorf("output mismatch for %s\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

func wantProblems(t *testing.T, err error, want bool) {
	t.Helper()
	if want && !errors.Is(err, ErrProblems) {
		t.Fatalf("want ErrProblems, got %v", err)
	}
	if !want && err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

const backendManifest = `---
# Source: backend/templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: backend
---
# Source: backend/templates/service.yaml
apiVersion: v1
kind: Service
metadata:
  name: backend
---
# Source: backend/templates/config.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: backend-config
  namespace: shared
---
# Source: backend/templates/clusterrole.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: backend-reader
---
# Source: backend/templates/crd-thing.yaml
apiVersion: example.com/v1
kind: Widget
metadata:
  name: backend-widget
`

var migrateHook = &rspb.Hook{
	Name: "backend-migrate", Kind: "Job", Path: "backend/templates/migrate.yaml",
	Events:   []rspb.HookEvent{rspb.HookPreUpgrade},
	Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: backend-migrate\n",
}

func backendCluster(t *testing.T) *cluster {
	c := newCluster(t)
	c.release("backend", "prod", 1, common.StatusSuperseded, backendManifest, migrateHook)
	c.release("backend", "prod", 2, common.StatusDeployed, backendManifest, migrateHook)
	c.object("apps/v1", "Deployment", "prod", "backend", "backend", "prod")
	c.object("v1", "Service", "prod", "backend", "", "")                        // no ownership metadata
	c.object("v1", "ConfigMap", "shared", "backend-config", "frontend", "prod") // owned by another release
	c.object("rbac.authorization.k8s.io/v1", "ClusterRole", "", "backend-reader", "backend", "prod")
	return c
}

func TestResources(t *testing.T) {
	c := backendCluster(t)
	out, err := run(t, c, "prod", "resources", "backend")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestResourcesJSON(t *testing.T) {
	c := backendCluster(t)
	out, err := run(t, c, "prod", "resources", "backend", "-r", "1", "-o", "json")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestResourcesHealthy(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n")
	c.object("apps/v1", "Deployment", "prod", "api", "api", "prod")
	out, err := run(t, c, "prod", "resources", "api")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestResourcesNotFound(t *testing.T) {
	_, err := run(t, newCluster(t), "prod", "resources", "nope")
	if !errors.Is(err, helm.ErrReleaseNotFound) {
		t.Fatalf("want ErrReleaseNotFound, got %v", err)
	}
}

func TestHistory(t *testing.T) {
	c := newCluster(t)
	c.release("backend", "prod", 1, common.StatusSuperseded, backendManifest)
	c.release("backend", "prod", 2, common.StatusDeployed, backendManifest)
	c.release("backend", "prod", 3, common.StatusFailed, backendManifest)
	out, err := run(t, c, "prod", "history", "backend")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestHistoryHealthy(t *testing.T) {
	c := newCluster(t)
	c.release("backend", "prod", 1, common.StatusDeployed, backendManifest)
	_, err := run(t, c, "prod", "history", "backend")
	wantProblems(t, err, false)
}

func TestFindByAnnotation(t *testing.T) {
	c := backendCluster(t)
	out, err := run(t, c, "prod", "find", "deployment/backend")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestFindByScan(t *testing.T) {
	c := backendCluster(t)
	c.release("legacy", "staging", 4, common.StatusDeployed, "apiVersion: v1\nkind: Service\nmetadata:\n  name: backend\n  namespace: prod\n")
	out, err := run(t, c, "prod", "find", "Service", "backend")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestFindAllRevisions(t *testing.T) {
	c := backendCluster(t)
	out, err := run(t, c, "default", "find", "clusterrole/backend-reader", "--all-revisions")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestFindHookUnknownKind(t *testing.T) {
	c := backendCluster(t)
	out, err := run(t, c, "prod", "find", "Widget/backend-widget", "-o", "yaml")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestFindNamespaceFilter(t *testing.T) {
	c := backendCluster(t)
	out, err := run(t, c, "staging", "find", "service/backend")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestFindAllNamespaces(t *testing.T) {
	c := backendCluster(t)
	out, err := run(t, c, "staging", "find", "job/backend-migrate", "-A")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestStuck(t *testing.T) {
	c := newCluster(t)
	c.release("backend", "prod", 1, common.StatusSuperseded, backendManifest)
	c.release("backend", "prod", 2, common.StatusDeployed, backendManifest)
	c.release("backend", "prod", 3, common.StatusPendingUpgrade, backendManifest)
	c.release("fresh", "dev", 1, common.StatusPendingInstall, backendManifest)
	c.release("ok", "prod", 1, common.StatusDeployed, backendManifest)
	out, err := run(t, c, "prod", "stuck", "-A")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestStuckRecentIgnored(t *testing.T) {
	c := newCluster(t)
	c.release("backend", "prod", 9, common.StatusPendingUpgrade, backendManifest) // pending for 1h
	out, err := run(t, c, "prod", "stuck", "--older-than", "2h")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestUnsupportedOutput(t *testing.T) {
	_, err := run(t, newCluster(t), "prod", "stuck", "-o", "xml")
	if err == nil || !strings.Contains(err.Error(), "unsupported output format") {
		t.Fatalf("want output format error, got %v", err)
	}
}
