package cli

import (
	"bytes"
	"encoding/json"
	"errors"
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

func runLostFoundProgress(t *testing.T, progress bool) (stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut, o.Progress = &out, &errOut, &progress
	c := lostCluster(t)
	o.NewClients = func(*Options) (*Clients, error) { return c.clients("prod"), nil }
	cmd := NewRootCommand(o)
	cmd.SetArgs([]string{"lost-found", "-o", "json"})
	if err := cmd.Execute(); !errors.Is(err, ErrProblems) {
		t.Fatal(err)
	}
	return out.String(), errOut.String()
}

func TestLostFoundShowsProgress(t *testing.T) {
	stdout, stderr := runLostFoundProgress(t, true)
	if !strings.Contains(stderr, "Scanning 6/6 resource kinds") {
		t.Fatalf("progress missing on stderr: %q", stderr)
	}
	if !strings.HasSuffix(stderr, "\r\033[K") {
		t.Fatalf("progress line not erased at the end: %q", stderr)
	}
	if strings.Contains(stdout, "Scanning") {
		t.Fatalf("progress leaked into stdout: %q", stdout)
	}
}

func TestLostFoundNoProgressWhenDisabled(t *testing.T) {
	if _, stderr := runLostFoundProgress(t, false); stderr != "" {
		t.Fatalf("unexpected stderr output: %q", stderr)
	}
}

func TestLostFoundReleaseStoredInAnotherNamespace(t *testing.T) {
	c := newCluster(t)
	// helm -n infra list shows cert-manager/cert-manager: the records live in infra, the release belongs to cert-manager
	c.releaseStoredIn("infra", "cert-manager", "cert-manager", 1, common.StatusSuperseded, simpleManifest, nil)
	c.releaseStoredIn("infra", "cert-manager", "cert-manager", 2, common.StatusDeployed, simpleManifest, nil)
	c.object("apps/v1", "Deployment", "cert-manager", "cert-manager", "cert-manager", "cert-manager")
	c.object("rbac.authorization.k8s.io/v1", "ClusterRole", "", "cert-manager-controller", "cert-manager", "cert-manager")
	// same release name, but the record belongs to another namespace
	c.object("v1", "ConfigMap", "dev", "stray", "cert-manager", "dev")

	out, err := run(t, c, "", "lost-found", "-A")
	wantProblems(t, err, true)
	if got := strings.Join(strings.Fields(out), " "); got != "RELEASE NAMESPACE RELEASE KIND NAMESPACE NAME dev cert-manager ConfigMap dev stray" {
		t.Fatalf("unexpected output:\n%s", out)
	}
}
