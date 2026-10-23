package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/release/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type auditRowJSON struct {
	Release, Namespace, Status string
	Revisions, LatestRevision  int
	LargestBytes, TotalBytes   int
	Findings                   []string
}

type auditJSON struct {
	Scanned      int
	DecodeErrors int
	Releases     []auditRowJSON
}

func auditRun(t *testing.T, c *cluster, ns string, args ...string) (auditJSON, error) {
	t.Helper()
	out, err := run(t, c, ns, append([]string{"audit", "-o", "json"}, args...)...)
	var res auditJSON
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("bad json: %v\n%s", jerr, out)
	}
	return res, err
}

func auditByName(res auditJSON, name string) auditRowJSON {
	for _, r := range res.Releases {
		if r.Release == name {
			return r
		}
	}
	return auditRowJSON{}
}

func healthyCluster(t *testing.T) *cluster {
	c := newCluster(t)
	c.release("web", "prod", 1, common.StatusSuperseded, simpleManifest)
	c.release("web", "prod", 2, common.StatusDeployed, simpleManifest)
	c.object("v1", "ConfigMap", "prod", "cfg", "web", "prod")
	return c
}

func TestAuditHealthy(t *testing.T) {
	res, err := auditRun(t, healthyCluster(t), "prod")
	wantProblems(t, err, false)
	r := auditByName(res, "web")
	if res.Scanned != 1 || r.Namespace != "prod" || r.Status != "deployed" || r.Revisions != 2 || r.LatestRevision != 2 ||
		r.LargestBytes <= 0 || r.TotalBytes < r.LargestBytes || len(r.Findings) != 0 {
		t.Fatalf("res = %+v", res)
	}
}

func TestAuditTable(t *testing.T) {
	out, err := run(t, healthyCluster(t), "prod", "audit")
	wantProblems(t, err, false)
	for _, want := range []string{"NAMESPACE", "RELEASE", "REVISIONS", "LARGEST", "TOTAL", "web", "deployed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestAuditNoReleases(t *testing.T) {
	out, err := run(t, newCluster(t), "prod", "audit")
	wantProblems(t, err, false)
	if !strings.Contains(out, "No releases") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestAuditTooManyRevisions(t *testing.T) {
	res, err := auditRun(t, healthyCluster(t), "prod", "--max-revisions", "1")
	wantProblems(t, err, true)
	if f := auditByName(res, "web").Findings; !slices.Equal(f, []string{"revisions"}) {
		t.Fatalf("findings = %v", f)
	}
	// At the limit is fine.
	_, err = auditRun(t, healthyCluster(t), "prod", "--max-revisions", "2")
	wantProblems(t, err, false)
}

func TestAuditNearSizeLimit(t *testing.T) {
	res, err := auditRun(t, healthyCluster(t), "prod", "--max-record-size", "1")
	wantProblems(t, err, true)
	if f := auditByName(res, "web").Findings; !slices.Equal(f, []string{"record-size"}) {
		t.Fatalf("findings = %v", f)
	}
}

func TestAuditRecordSizeQuantity(t *testing.T) {
	_, err := auditRun(t, healthyCluster(t), "prod", "--max-record-size", "800Ki")
	wantProblems(t, err, false)
	if _, err := run(t, healthyCluster(t), "prod", "audit", "--max-record-size", "lots"); err == nil || strings.Contains(err.Error(), "problems found") {
		t.Fatalf("err = %v", err)
	}
}

func TestAuditOrphaned(t *testing.T) {
	c := newCluster(t)
	c.release("gone", "prod", 1, common.StatusDeployed, simpleManifest)
	res, err := auditRun(t, c, "prod")
	wantProblems(t, err, true)
	if f := auditByName(res, "gone").Findings; !slices.Equal(f, []string{"orphaned"}) {
		t.Fatalf("findings = %v", f)
	}
}

func TestAuditNotOrphaned(t *testing.T) {
	const twoObjects = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: other\n"
	for name, tt := range map[string]struct {
		status   common.Status
		manifest string
		live     bool
	}{
		"one of two objects exists": {common.StatusDeployed, twoObjects, true},
		"uninstalled keeps history": {common.StatusUninstalled, simpleManifest, false},
		"declares no objects":       {common.StatusDeployed, "", false},
		"install in flight":         {common.StatusPendingInstall, simpleManifest, false},
		"upgrade in flight":         {common.StatusPendingUpgrade, simpleManifest, false},
		"unknown kind":              {common.StatusDeployed, "apiVersion: example.com/v1\nkind: Widget\nmetadata:\n  name: w\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t)
			c.release("web", "prod", 1, tt.status, tt.manifest)
			if tt.live {
				c.object("v1", "ConfigMap", "prod", "cfg", "web", "prod")
			}
			res, err := auditRun(t, c, "prod")
			wantProblems(t, err, false)
			if f := auditByName(res, "web").Findings; len(f) != 0 {
				t.Fatalf("findings = %v", f)
			}
		})
	}
}

func TestAuditMultipleFindings(t *testing.T) {
	c := newCluster(t)
	c.release("gone", "prod", 1, common.StatusSuperseded, simpleManifest)
	c.release("gone", "prod", 2, common.StatusDeployed, simpleManifest)
	res, err := auditRun(t, c, "prod", "--max-revisions", "1")
	wantProblems(t, err, true)
	if f := auditByName(res, "gone").Findings; !slices.Equal(f, []string{"revisions", "orphaned"}) {
		t.Fatalf("findings = %v", f)
	}
}

func TestAuditNamespaceScope(t *testing.T) {
	c := newCluster(t)
	c.release("a", "prod", 1, common.StatusDeployed, "")
	c.release("b", "dev", 1, common.StatusDeployed, "")
	res, _ := auditRun(t, c, "prod")
	if res.Scanned != 1 || res.Releases[0].Release != "a" {
		t.Fatalf("prod: %+v", res)
	}
	res, _ = auditRun(t, c, "prod", "-A")
	if res.Scanned != 2 || res.Releases[0].Namespace != "dev" || res.Releases[1].Namespace != "prod" {
		t.Fatalf("-A: %+v", res)
	}
}

func TestAuditUndecodableLatestRecord(t *testing.T) {
	c := newCluster(t)
	c.release("web", "prod", 1, common.StatusDeployed, "")
	_, err := c.kube.CoreV1().Secrets("prod").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sh.helm.release.v1.bad.v1", Namespace: "prod",
			Labels: map[string]string{"owner": "helm", "name": "bad", "version": "1"},
		},
		Data: map[string][]byte{"release": []byte("garbage")},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := auditRun(t, c, "prod")
	wantProblems(t, err, false)
	if res.DecodeErrors != 1 || res.Scanned != 2 || auditByName(res, "bad").Revisions != 1 || auditByName(res, "bad").Status != "" {
		t.Fatalf("res = %+v", res)
	}
}
