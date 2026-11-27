package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/release/common"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const driftImmutableHeader = "Immutable fields (the live value differs from the release and Kubernetes does not allow changing it in place):"

// driftSelectorCluster has a live Deployment whose selector no longer matches the release.
func driftSelectorCluster(t *testing.T) *cluster {
	t.Helper()
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 2
  selector:
    matchLabels:
      app: api
`)
	c.live = append(c.live, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "api", "namespace": "prod"},
		"spec": map[string]any{
			"replicas": int64(2),
			"selector": map[string]any{"matchLabels": map[string]any{"app": "api-old"}},
		},
	}})
	return c
}

func TestDriftImmutableSelectorJSON(t *testing.T) {
	out, err := run(t, driftSelectorCluster(t), "prod", "drift", "api", "-o", "json")
	wantProblems(t, err, true)
	var result struct {
		Resources []struct {
			Status    string `json:"status"`
			Immutable *struct {
				Fields    []string `json:"fields"`
				Reference string   `json:"reference"`
				Command   string   `json:"command"`
				Note      string   `json:"note"`
			} `json:"immutable"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 1 {
		t.Fatalf("want one resource: %s", out)
	}
	row := result.Resources[0]
	if row.Status != "drifted" {
		t.Errorf("status = %q, want drifted", row.Status)
	}
	if row.Immutable == nil {
		t.Fatalf("no immutable block: %s", out)
	}
	if len(row.Immutable.Fields) != 1 || row.Immutable.Fields[0] != "/spec/selector" {
		t.Errorf("immutable.fields = %q, want [/spec/selector]", row.Immutable.Fields)
	}
	if row.Immutable.Command != "kubectl -n prod delete deployment/api" {
		t.Errorf("immutable.command = %q", row.Immutable.Command)
	}
	if row.Immutable.Reference == "" {
		t.Error("immutable.reference is empty")
	}
}

func TestDriftImmutableSelectorTable(t *testing.T) {
	out, err := run(t, driftSelectorCluster(t), "prod", "drift", "api")
	wantProblems(t, err, true)
	for _, want := range []string{
		driftImmutableHeader,
		"  Deployment prod/api: /spec/selector\n",
		"    reference: https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/deployment-v1/#DeploymentSpec\n",
		"    to apply it (not run): kubectl -n prod delete deployment/api\n",
		"      also deletes its Pods, so the workload is down until the next upgrade or rollback recreates it\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	header, table, diff := strings.Index(out, driftImmutableHeader), strings.Index(out, "STATUS"), strings.Index(out, "--- Deployment prod/api")
	if diff < 0 || table >= header || header >= diff {
		t.Fatalf("immutable block must come after the table and before the diffs:\n%s", out)
	}
	if !strings.Contains(out, "\n\n"+driftImmutableHeader+"\n") {
		t.Fatalf("immutable block must be preceded by a blank line:\n%s", out)
	}
}

func TestDriftImmutableValuesUnchanged(t *testing.T) {
	out, err := run(t, driftSelectorCluster(t), "prod", "drift", "api", "--values")
	wantProblems(t, err, true)
	if strings.Contains(out, "Immutable fields") || strings.Contains(out, "kubectl") {
		t.Fatalf("--values output must not carry the immutable block:\n%s", out)
	}
}

func TestDriftMutableOnlyHasNoImmutable(t *testing.T) {
	out, err := run(t, driftDeployment(t, 3), "prod", "drift", "api", "-o", "json")
	wantProblems(t, err, true)
	var result struct {
		Resources []map[string]json.RawMessage `json:"resources"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Resources) != 1 || string(result.Resources[0]["status"]) != `"drifted"` {
		t.Fatalf("unexpected report: %s", out)
	}
	if _, ok := result.Resources[0]["immutable"]; ok {
		t.Fatalf("replicas-only drift must have no immutable key: %s", out)
	}
	text, err := run(t, driftDeployment(t, 3), "prod", "drift", "api")
	wantProblems(t, err, true)
	if strings.Contains(text, "Immutable fields") {
		t.Fatalf("replicas-only drift must have no immutable block:\n%s", text)
	}
}
