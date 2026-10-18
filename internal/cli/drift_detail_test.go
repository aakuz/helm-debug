package cli

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/release/common"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/aakuz/helm-debug/internal/helm"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func driftDeployment(t *testing.T, replicas int64) *cluster {
	t.Helper()
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 2
  template:
    spec:
      containers:
      - name: api
        image: api:v1
        resources:
          requests:
            cpu: "0.1"
`)
	c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "api", "namespace": "prod", "uid": "x"},
		"status":   map[string]any{"replicas": int64(1)},
		"spec": map[string]any{
			"replicas": replicas, "strategy": map[string]any{"type": "RollingUpdate"},
			"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{
				"name": "api", "image": "api:v1", "imagePullPolicy": "Always",
				"resources": map[string]any{"requests": map[string]any{"cpu": "100m"}},
			}}}},
		},
	}})
	return c
}

func TestDriftTableShowsDiff(t *testing.T) {
	out, err := run(t, driftDeployment(t, 3), "prod", "drift", "api")
	wantProblems(t, err, true)
	for _, want := range []string{"--- Deployment prod/api (drifted)", "-  replicas: 2", "+  replicas: 3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	if !strings.Contains(out, "--- ") {
		t.Fatalf("no diff: %s", out)
	}
	diff := strings.SplitN(out, "--- ", 2)[1]
	for _, unwanted := range []string{"strategy", "imagePullPolicy", "cpu", "uid", "status"} {
		if strings.Contains(diff, unwanted) {
			t.Fatalf("diff contains live-only or normalized %q: %s", unwanted, out)
		}
	}
	if strings.Index(out, "STATUS") > strings.Index(out, "--- ") {
		t.Fatalf("table must come first: %s", out)
	}
}

func TestDriftJSONDiffField(t *testing.T) {
	out, err := run(t, driftDeployment(t, 3), "prod", "drift", "api", "-o", "json")
	wantProblems(t, err, true)
	var result struct {
		Resources []struct {
			Changes []string `json:"changes"`
			Diff    string   `json:"diff"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	r := result.Resources[0]
	if len(r.Changes) != 1 || !strings.Contains(r.Diff, "-  replicas: 2\n+  replicas: 3\n") {
		t.Fatalf("unexpected row: %s", out)
	}
}

func TestDriftInSyncHasNoDiff(t *testing.T) {
	out, err := run(t, driftDeployment(t, 2), "prod", "drift", "api", "-o", "json")
	wantProblems(t, err, false)
	if strings.Contains(out, `"diff"`) {
		t.Fatalf("unexpected diff: %s", out)
	}
}

func TestDriftDiffKeepsAddedPayloadKeys(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\ndata:\n  a: one\n")
	c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "settings", "namespace": "prod"},
		"data":     map[string]any{"a": "one", "added": "manual"},
	}})
	out, err := run(t, c, "prod", "drift", "api")
	wantProblems(t, err, true)
	if !strings.Contains(out, "+  added: manual") {
		t.Fatalf("unexpected report: %s", out)
	}
}

func driftSecret(t *testing.T) *cluster {
	t.Helper()
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, `apiVersion: v1
kind: Secret
metadata:
  name: credentials
stringData:
  password: old-password
  same: unchanged
`)
	c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "credentials", "namespace": "prod"},
		"data":     map[string]any{"password": b64("live-password"), "same": b64("unchanged")},
	}})
	return c
}

func TestDriftMasksSecretsByDefault(t *testing.T) {
	for _, format := range []string{"table", "json"} {
		out, err := run(t, driftSecret(t), "prod", "drift", "api", "-o", format)
		wantProblems(t, err, true)
		text := out
		if format == "json" {
			// The JSON encoder escapes < and >.
			text = strings.NewReplacer("\\u003c", "<", "\\u003e", ">").Replace(out)
		}
		for _, want := range []string{"<masked:old>", "<masked:new>"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s: missing %q: %s", format, want, out)
			}
		}
		// The unchanged key has no changed path, so only the changed one is a diff line.
		for _, unwanted := range []string{"old-password", "live-password", b64("live-password"), b64("old-password")} {
			if strings.Contains(out, unwanted) {
				t.Fatalf("%s: leaked %q: %s", format, unwanted, out)
			}
		}
	}
}

func TestDriftShowSecrets(t *testing.T) {
	out, err := run(t, driftSecret(t), "prod", "drift", "api", "--show-secrets")
	wantProblems(t, err, true)
	for _, want := range []string{b64("live-password"), b64("old-password")} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "<masked") {
		t.Fatalf("unexpected masking: %s", out)
	}
}

func TestDriftValuesMode(t *testing.T) {
	c := driftDeployment(t, 3)
	c.release("api", "prod", 2, common.StatusDeployed, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 2
  paused: true
  template:
    spec:
      containers:
      - name: api
        image: api:v2
`)
	out, err := run(t, c, "prod", "drift", "api", "--values")
	wantProblems(t, err, true)
	want := `# Deployment prod/api
# removed: /spec/paused
spec:
  replicas: 3
  template:
    spec:
      containers:
      - image: api:v1
        imagePullPolicy: Always
        name: api
        resources:
          requests:
            cpu: 100m
`
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestDriftValuesModeSecretsAndSeparator(t *testing.T) {
	c := driftSecret(t)
	c.release("api", "prod", 2, common.StatusDeployed, `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
data:
  k: v
---
apiVersion: v1
kind: Secret
metadata:
  name: credentials
stringData:
  password: old-password
`)
	c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "a", "namespace": "prod"},
		"data":     map[string]any{"k": "changed"},
	}})
	out, err := run(t, c, "prod", "drift", "api", "--values")
	wantProblems(t, err, true)
	want := "# ConfigMap prod/a\ndata:\n  k: changed\n---\n# Secret prod/credentials\ndata:\n  password: <masked>\n  same: <masked>\n"
	if out != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	out, _ = run(t, c, "prod", "drift", "api", "--values", "--show-secrets")
	if !strings.Contains(out, b64("live-password")) {
		t.Fatalf("secret not shown: %s", out)
	}
}

func TestDriftValuesIgnoredForStructuredOutput(t *testing.T) {
	out, err := run(t, driftDeployment(t, 3), "prod", "drift", "api", "--values", "-o", "json")
	wantProblems(t, err, true)
	if !json.Valid([]byte(out)) {
		t.Fatalf("not json: %s", out)
	}
}

func TestDriftDiffColorHighlightsName(t *testing.T) {
	on := true
	o := NewOptions()
	o.Color = &on
	var b strings.Builder
	printDrift(o, &b, &driftResult{Release: "api", Namespace: "prod", Revision: 1, Resources: []driftRow{{
		Object: helm.Object{Kind: "Deployment", Namespace: "prod", Name: "api"}, Status: "drifted",
		Diff: "@@ -1,1 +1,1 @@\n-a\n+b\n",
	}}})
	for _, want := range []string{"--- Deployment prod/" + name + "api" + reset, green + "+b" + reset, red + "-a" + reset} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q: %q", want, b.String())
		}
	}
}

func TestDiffShowSecrets(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusSuperseded, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\ndata:\n  k: "+b64("one")+"\n")
	c.release("api", "prod", 2, common.StatusDeployed, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\ndata:\n  k: "+b64("two")+"\n")
	out, err := run(t, c, "prod", "diff", "api", "1", "2")
	if err != nil || strings.Contains(out, b64("two")) || !strings.Contains(out, "<masked:new>") {
		t.Fatalf("err=%v out=%s", err, out)
	}
	out, err = run(t, c, "prod", "diff", "api", "1", "2", "--show-secrets")
	if err != nil || !strings.Contains(out, b64("two")) || strings.Contains(out, "<masked") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}
