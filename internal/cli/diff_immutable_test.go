package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/release/common"
)

const immutableManifestV1 = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
  namespace: prod
spec:
  serviceName: db
  replicas: 1
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      resources:
        requests:
          storage: 1Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  replicas: 2
---
apiVersion: v1
kind: Service
metadata:
  name: db
  namespace: prod
spec:
  ports:
  - port: 5432
---
apiVersion: batch/v1
kind: Job
metadata:
  name: migrate
  namespace: prod
spec:
  template:
    spec:
      containers:
      - name: migrate
        image: migrate:v1
`

const immutableManifestV2 = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: db
  namespace: prod
spec:
  serviceName: db-headless
  replicas: 1
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      resources:
        requests:
          storage: 5Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  replicas: 3
---
apiVersion: v1
kind: Service
metadata:
  name: db
  namespace: prod
spec:
  clusterIP: None
  ports:
  - port: 5432
---
apiVersion: batch/v1
kind: Job
metadata:
  name: migrate
  namespace: prod
spec:
  template:
    spec:
      containers:
      - name: migrate
        image: migrate:v2
`

func immutableDiffCluster(t *testing.T) *cluster {
	t.Helper()
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusSuperseded, immutableManifestV1)
	c.release("api", "prod", 2, common.StatusDeployed, immutableManifestV2)
	return c
}

func TestDiffImmutableJSON(t *testing.T) {
	out, err := run(t, immutableDiffCluster(t), "prod", "diff", "api", "1", "2", "-o", "json")
	if err != nil {
		t.Fatalf("immutable changes must exit 0, got %v", err)
	}
	type immutable struct {
		Fields    []string `json:"fields"`
		Reference string   `json:"reference"`
		Command   string   `json:"command"`
		Note      string   `json:"note"`
	}
	var result struct {
		Manifest []struct {
			Kind      string     `json:"kind"`
			Name      string     `json:"name"`
			Status    string     `json:"status"`
			Immutable *immutable `json:"immutable"`
		} `json:"manifest"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		status  string
		fields  []string
		command string
	}{
		"StatefulSet/db": {"immutable", []string{"/spec/serviceName", "/spec/volumeClaimTemplates"}, "kubectl -n prod delete statefulset/db --cascade=orphan"},
		"Deployment/api": {"changed", nil, ""},
		"Service/db":     {"immutable", []string{"/spec/clusterIP"}, "kubectl -n prod delete service/db"},
		"Job/migrate":    {"immutable", []string{"/spec/template"}, "kubectl -n prod delete job/migrate"},
	}
	if len(result.Manifest) != len(want) {
		t.Fatalf("manifest rows = %d, want %d: %s", len(result.Manifest), len(want), out)
	}
	for _, row := range result.Manifest {
		key := row.Kind + "/" + row.Name
		w, ok := want[key]
		if !ok {
			t.Fatalf("unexpected row %s: %s", key, out)
		}
		if row.Status != w.status {
			t.Errorf("%s status = %q, want %q", key, row.Status, w.status)
		}
		if w.fields == nil {
			if row.Immutable != nil {
				t.Errorf("%s immutable = %+v, want none", key, row.Immutable)
			}
			continue
		}
		if row.Immutable == nil {
			t.Errorf("%s has no immutable block: %s", key, out)
			continue
		}
		if strings.Join(row.Immutable.Fields, ",") != strings.Join(w.fields, ",") {
			t.Errorf("%s immutable.fields = %q, want %q", key, row.Immutable.Fields, w.fields)
		}
		if row.Immutable.Command != w.command {
			t.Errorf("%s immutable.command = %q, want %q", key, row.Immutable.Command, w.command)
		}
		if row.Immutable.Reference == "" {
			t.Errorf("%s immutable.reference is empty", key)
		}
	}
	if strings.Count(out, `"immutable":`) != 3 {
		t.Errorf("want exactly 3 immutable keys (omitempty on the rest): %s", out)
	}
}

func TestDiffImmutableTable(t *testing.T) {
	out, err := run(t, immutableDiffCluster(t), "prod", "diff", "api", "1", "2")
	if err != nil {
		t.Fatalf("immutable changes must exit 0, got %v", err)
	}
	golden(t, out)
}

func TestDiffImmutableFallsBackToReleaseNamespace(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusSuperseded, "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: db\nspec:\n  serviceName: db\n")
	c.release("api", "prod", 2, common.StatusDeployed, "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: db\nspec:\n  serviceName: db-headless\n")
	out, err := run(t, c, "prod", "diff", "api", "1", "2", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kubectl -n prod delete statefulset/db --cascade=orphan") {
		t.Fatalf("command must use the release namespace: %s", out)
	}
}

func TestDiffImmutableIdenticalRevisions(t *testing.T) {
	out, err := run(t, immutableDiffCluster(t), "prod", "diff", "api", "2", "2")
	if err != nil {
		t.Fatalf("identical revisions must exit 0: %v", err)
	}
	if !strings.Contains(out, "No differences") || strings.Contains(out, "Immutable fields") {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestImmutableStatusColor(t *testing.T) {
	if got := statusColor("immutable"); got != red {
		t.Errorf("statusColor(immutable) = %q, want red", got)
	}
}
