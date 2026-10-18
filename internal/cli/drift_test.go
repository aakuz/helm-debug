package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/release/common"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/aakuz/helm-debug/internal/helm"
)

func TestDriftChangedField(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 2
`)
	c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "api", "namespace": "prod"},
		"spec":     map[string]any{"replicas": int64(3)},
	}})
	out, err := run(t, c, "prod", "drift", "api", "-o", "json")
	wantProblems(t, err, true)
	var result struct {
		Revision  int `json:"revision"`
		Resources []struct {
			Status  string   `json:"status"`
			Changes []string `json:"changes"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.Revision != 1 || len(result.Resources) != 1 || result.Resources[0].Status != "drifted" || len(result.Resources[0].Changes) != 1 || result.Resources[0].Changes[0] != "/spec/replicas" {
		t.Fatalf("unexpected drift report: %s", out)
	}
}

func TestDriftIgnoresServerFieldsAndDefaults(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  resourceVersion: old
  managedFields: []
  creationTimestamp: null
status:
  replicas: 0
spec:
  template:
    spec:
      containers:
      - name: api
        image: api:v1
`)
	c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "api", "namespace": "prod", "resourceVersion": "123", "uid": "generated", "creationTimestamp": "2026-10-18T00:00:00Z", "managedFields": []any{map[string]any{"manager": "helm"}}, "annotations": map[string]any{"meta.helm.sh/release-name": "api"}},
		"status":   map[string]any{"replicas": int64(1)},
		"spec":     map[string]any{"replicas": int64(1), "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "api", "image": "api:v1", "imagePullPolicy": "IfNotPresent"}}, "restartPolicy": "Always"}}},
	}})
	out, err := run(t, c, "prod", "drift", "api")
	wantProblems(t, err, false)
	if !strings.Contains(out, "in-sync") {
		t.Fatalf("unexpected report: %s", out)
	}
}

func TestDriftSecretStringData(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, `apiVersion: v1
kind: Secret
metadata:
  name: credentials
stringData:
  password: private-password
`)
	c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "credentials", "namespace": "prod"},
		"data":     map[string]any{"password": "cHJpdmF0ZS1wYXNzd29yZA=="},
	}})
	out, err := run(t, c, "prod", "drift", "api")
	wantProblems(t, err, false)
	if !strings.Contains(out, "in-sync") {
		t.Fatalf("unexpected report: %s", out)
	}
}

func TestDriftFormatsAndLatestRevision(t *testing.T) {
	for _, format := range []string{"table", "json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			c := newCluster(t)
			const manifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
  namespace: shared
data:
  setting/key: expected
  removed: expected
`
			c.release("api", "prod", 1, common.StatusSuperseded, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: old\n")
			c.release("api", "prod", 2, common.StatusDeployed, manifest, migrateHook)
			c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "settings", "namespace": "shared"},
				"data":     map[string]any{"setting/key": "sensitive-live-value"},
			}})
			out, err := run(t, c, "prod", "drift", "api", "-o", format)
			wantProblems(t, err, true)
			for _, want := range []string{"settings", "shared", "/data/removed", "/data/setting~1key", "drifted"} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q: %s", want, out)
				}
			}
			if !strings.Contains(out, "sensitive-live-value") || !strings.Contains(out, "expected") {
				t.Fatalf("values missing: %s", out)
			}
			for _, unwanted := range []string{"backend-migrate", "old"} {
				if strings.Contains(out, unwanted) {
					t.Fatalf("unwanted %q: %s", unwanted, out)
				}
			}
			if format != "table" {
				var result map[string]any
				if err := yaml.Unmarshal([]byte(out), &result); err != nil {
					t.Fatal(err)
				}
				if result["revision"] != float64(2) {
					t.Fatalf("wrong revision: %s", out)
				}
			}
		})
	}
}

func TestDriftMissingObject(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: missing\n")
	out, err := run(t, c, "prod", "drift", "api")
	wantProblems(t, err, true)
	if !strings.Contains(out, "absent") {
		t.Fatalf("unexpected report: %s", out)
	}
}

func TestDriftLookupFailure(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, "apiVersion: example.com/v1\nkind: Widget\nmetadata:\n  name: unknown\n")
	out, err := run(t, c, "prod", "drift", "api", "-o", "json")
	if err == nil || errors.Is(err, ErrProblems) {
		t.Fatalf("want execution error, got %v", err)
	}
	if !strings.Contains(out, "unknown-kind") {
		t.Fatalf("unexpected report: %s", out)
	}
}

func TestDriftReleaseNotFound(t *testing.T) {
	_, err := run(t, newCluster(t), "prod", "drift", "missing")
	if !errors.Is(err, helm.ErrReleaseNotFound) {
		t.Fatalf("want release not found, got %v", err)
	}
}

func TestDriftNestedListAndClusterScope(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: reader
  namespace: ignored
rules:
- apiGroups: [""]
  resources: [pods]
  verbs: [get]
`)
	c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": "reader"},
		"rules":    []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"list"}}},
	}})
	out, err := run(t, c, "prod", "drift", "api", "-o", "json")
	wantProblems(t, err, true)
	if !strings.Contains(out, "/rules/0/verbs/0") || strings.Contains(out, "ignored") {
		t.Fatalf("unexpected report: %s", out)
	}
}

func TestDriftAddedPayloadKey(t *testing.T) {
	for _, kind := range []string{"ConfigMap", "Secret"} {
		t.Run(kind, func(t *testing.T) {
			c := newCluster(t)
			c.release("api", "prod", 1, common.StatusDeployed, "apiVersion: v1\nkind: "+kind+"\nmetadata:\n  name: settings\ndata:\n  original: dmFsdWU=\n")
			c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": kind,
				"metadata": map[string]any{"name": "settings", "namespace": "prod"},
				"data":     map[string]any{"original": "dmFsdWU=", "added": "bWFudWFs"},
			}})
			out, err := run(t, c, "prod", "drift", "api", "-o", "json")
			wantProblems(t, err, true)
			if !strings.Contains(out, "/data/added") {
				t.Fatalf("unexpected report: %s", out)
			}
		})
	}
}

func TestDriftEquivalentResourceQuantities(t *testing.T) {
	c := newCluster(t)
	const manifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
      - name: api
        image: api:v1
        resources:
          requests:
            cpu: "0.1"
            memory: 1024Mi
`
	c.release("api", "prod", 1, common.StatusDeployed, manifest)
	liveJSON, err := yaml.YAMLToJSON([]byte(strings.ReplaceAll(strings.ReplaceAll(manifest, `"0.1"`, "100m"), "1024Mi", "1Gi")))
	if err != nil {
		t.Fatal(err)
	}
	live := &unstructured.Unstructured{}
	if err := live.UnmarshalJSON(liveJSON); err != nil {
		t.Fatal(err)
	}
	live.SetNamespace("prod")
	c.live = append(c.live, live)
	out, err := run(t, c, "prod", "drift", "api")
	wantProblems(t, err, false)
	if !strings.Contains(out, "in-sync") {
		t.Fatalf("unexpected report: %s", out)
	}
}

func TestDriftQuantityLikeApplicationStrings(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\ndata:\n  value: \"0.1\"\n")
	c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "settings", "namespace": "prod"},
		"data":     map[string]any{"value": "100m"},
	}})
	out, err := run(t, c, "prod", "drift", "api", "-o", "json")
	wantProblems(t, err, true)
	if !strings.Contains(out, "/data/value") {
		t.Fatalf("unexpected report: %s", out)
	}
}
