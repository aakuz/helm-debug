package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/release/common"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/aakuz/helm-debug/internal/kube"
)

func lostCluster(t *testing.T) *cluster {
	t.Helper()
	c := newCluster(t)
	c.release("alive", "prod", 1, common.StatusDeployed, simpleManifest)
	c.object("apps/v1", "Deployment", "prod", "alive", "alive", "prod")
	// release records live in prod, the object is elsewhere: the release still exists
	c.object("v1", "ConfigMap", "dev", "alive-cfg", "alive", "prod")

	c.object("apps/v1", "Deployment", "prod", "gone", "gone", "prod")
	c.object("v1", "Service", "prod", "gone", "gone", "prod")
	c.object("rbac.authorization.k8s.io/v1", "ClusterRole", "", "gone-reader", "gone", "prod")
	c.object("v1", "ConfigMap", "dev", "orphan-cfg", "orphan", "dev")
	// same name as a live release but another namespace: not that release
	c.object("v1", "ConfigMap", "prod", "alive-twin", "alive", "dev")
	// not owned by Helm at all
	c.object("v1", "ConfigMap", "prod", "plain", "", "")
	return c
}

func TestLostFoundNothingLost(t *testing.T) {
	c := newCluster(t)
	c.release("alive", "prod", 1, common.StatusDeployed, simpleManifest)
	c.object("apps/v1", "Deployment", "prod", "alive", "alive", "prod")
	out, err := run(t, c, "prod", "lost-found")
	wantProblems(t, err, false)
	if want := "No lost resources: every Helm-managed object belongs to an existing release\n"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestLostFoundTable(t *testing.T) {
	out, err := run(t, lostCluster(t), "prod", "lost-found")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestLostFoundAllNamespaces(t *testing.T) {
	out, err := run(t, lostCluster(t), "prod", "lost-found", "-A")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestLostFoundJSON(t *testing.T) {
	out, err := run(t, lostCluster(t), "prod", "lost-found", "-n", "prod", "-o", "json")
	wantProblems(t, err, true)
	var got struct {
		Releases []struct {
			Release   string `json:"release"`
			Namespace string `json:"namespace"`
			Resources []struct {
				Kind, Name string
			} `json:"resources"`
		} `json:"releases"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(got.Releases) != 2 {
		t.Fatalf("want releases gone/prod and alive/dev, got %+v", got.Releases)
	}
	if r := got.Releases[1]; r.Release != "gone" || r.Namespace != "prod" || len(r.Resources) != 3 {
		t.Fatalf("release gone/prod wrong: %+v", r)
	}
	if strings.Contains(out, "plain") || strings.Contains(out, "alive-cfg") {
		t.Fatalf("object of a live release or without Helm metadata reported:\n%s", out)
	}
}

func TestLostFoundReleaseNamespaceDefaultsToObjectNamespace(t *testing.T) {
	c := newCluster(t)
	c.release("alive", "prod", 1, common.StatusDeployed, simpleManifest)
	c.object("v1", "ConfigMap", "prod", "kept", "alive", "")
	c.object("v1", "ConfigMap", "prod", "lost", "gone", "")
	for _, o := range c.live {
		u := o.(*unstructured.Unstructured)
		u.SetAnnotations(map[string]string{kube.AnnotationReleaseName: u.GetAnnotations()[kube.AnnotationReleaseName]})
	}
	out, err := run(t, c, "prod", "lost-found")
	wantProblems(t, err, true)
	if strings.Contains(out, "kept") || strings.Join(strings.Fields(out), " ") != "RELEASE NAMESPACE RELEASE KIND NAMESPACE NAME prod gone ConfigMap prod lost" {
		t.Fatalf("unexpected output:\n%s", out)
	}
}
