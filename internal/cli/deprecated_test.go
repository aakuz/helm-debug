package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const ingressManifest = `---
apiVersion: extensions/v1beta1
kind: Ingress
metadata:
  name: web
`

const currentManifest = `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
`

type deprecatedFinding struct {
	Kind, Name, APIVersion, Status, RemovedIn, Replacement string
	Hook                                                   []string
}

type deprecatedJSON struct {
	TargetVersion string
	Scanned       int
	Findings      []deprecatedFinding
}

func deprecatedRun(t *testing.T, c *cluster, args ...string) (deprecatedJSON, error) {
	t.Helper()
	out, err := run(t, c, "prod", append([]string{"deprecated", "-o", "json"}, args...)...)
	var res deprecatedJSON
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("bad json: %v\n%s", jerr, out)
	}
	return res, err
}

func TestDeprecatedRemovedAPIVersion(t *testing.T) {
	c := newCluster(t)
	c.release("web", "prod", 1, common.StatusDeployed, ingressManifest+currentManifest)
	res, err := deprecatedRun(t, c, "web", "--target-version", "1.30")
	wantProblems(t, err, true)
	if res.TargetVersion != "1.30" || res.Scanned != 2 || len(res.Findings) != 1 {
		t.Fatalf("res = %+v", res)
	}
	f := res.Findings[0]
	if f.Kind != "Ingress" || f.Name != "web" || f.APIVersion != "extensions/v1beta1" || f.Status != "removed" || f.RemovedIn != "1.22" || f.Replacement != "networking.k8s.io/v1" {
		t.Fatalf("finding = %+v", f)
	}
}

func TestDeprecatedTable(t *testing.T) {
	c := newCluster(t)
	c.release("web", "prod", 1, common.StatusDeployed, ingressManifest)
	out, err := run(t, c, "prod", "deprecated", "web", "--target-version", "1.30")
	wantProblems(t, err, true)
	for _, want := range []string{"Ingress", "web", "extensions/v1beta1", "removed", "1.22", "networking.k8s.io/v1", "1.30"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestDeprecatedCurrentIsClean(t *testing.T) {
	c := newCluster(t)
	c.release("web", "prod", 1, common.StatusDeployed, currentManifest)
	out, err := run(t, c, "prod", "deprecated", "web", "--target-version", "1.30")
	wantProblems(t, err, false)
	if !strings.Contains(out, "No deprecated apiVersions") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestDeprecatedBeforeDeprecationIsClean(t *testing.T) {
	c := newCluster(t)
	c.extraKinds = []kindInfo{{schema.GroupVersionKind{Group: "extensions", Version: "v1beta1", Kind: "Ingress"}, "ingresses", true}}
	c.release("web", "prod", 1, common.StatusDeployed, ingressManifest)
	_, err := deprecatedRun(t, c, "web", "--target-version", "1.13")
	wantProblems(t, err, false)
}

func TestDeprecatedStillServedIsDeprecated(t *testing.T) {
	c := newCluster(t)
	c.extraKinds = []kindInfo{{schema.GroupVersionKind{Group: "policy", Version: "v1beta1", Kind: "PodDisruptionBudget"}, "poddisruptionbudgets", true}}
	c.release("web", "prod", 1, common.StatusDeployed, "apiVersion: policy/v1beta1\nkind: PodDisruptionBudget\nmetadata:\n  name: web\n")
	res, err := deprecatedRun(t, c, "web", "--target-version", "1.23")
	wantProblems(t, err, true)
	if len(res.Findings) != 1 || res.Findings[0].Status != "deprecated" || res.Findings[0].RemovedIn != "1.25" {
		t.Fatalf("res = %+v", res)
	}
}

func TestDeprecatedTargetDefaultsToServerVersion(t *testing.T) {
	c := newCluster(t)
	c.serverVersion = "v1.30.2-eks-abc"
	c.release("web", "prod", 1, common.StatusDeployed, ingressManifest)
	res, err := deprecatedRun(t, c, "web")
	wantProblems(t, err, true)
	if res.TargetVersion != "1.30" || res.Findings[0].Status != "removed" {
		t.Fatalf("res = %+v", res)
	}
}

func TestDeprecatedNoTargetNoServerVersion(t *testing.T) {
	c := newCluster(t)
	c.release("web", "prod", 1, common.StatusDeployed, ingressManifest)
	_, err := run(t, c, "prod", "deprecated", "web")
	if err == nil || !strings.Contains(err.Error(), "--target-version") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeprecatedInvalidTargetVersion(t *testing.T) {
	c := newCluster(t)
	c.release("web", "prod", 1, common.StatusDeployed, ingressManifest)
	_, err := run(t, c, "prod", "deprecated", "web", "--target-version", "banana")
	if err == nil || errors.Is(err, ErrProblems) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeprecatedNotServedByCluster(t *testing.T) {
	c := newCluster(t)
	c.serverVersion = "v1.30.0"
	// Not in the removals table, but the cluster no longer serves it; the CRD kind is ignored.
	c.release("web", "prod", 1, common.StatusDeployed,
		"apiVersion: apps/v1beta9\nkind: Deployment\nmetadata:\n  name: web\n---\napiVersion: cert-manager.io/v1\nkind: Certificate\nmetadata:\n  name: cert\n")
	res, err := deprecatedRun(t, c, "web")
	wantProblems(t, err, true)
	if len(res.Findings) != 1 || res.Findings[0].Status != "not-served" || res.Findings[0].APIVersion != "apps/v1beta9" {
		t.Fatalf("res = %+v", res)
	}
}

func TestDeprecatedIncludesHooks(t *testing.T) {
	c := newCluster(t)
	hook := &rspb.Hook{
		Name: "cleanup", Kind: "CronJob", Events: []rspb.HookEvent{rspb.HookPreUpgrade},
		Manifest: "apiVersion: batch/v1beta1\nkind: CronJob\nmetadata:\n  name: cleanup\n",
	}
	c.release("web", "prod", 1, common.StatusDeployed, currentManifest, hook)
	res, err := deprecatedRun(t, c, "web", "--target-version", "1.31")
	wantProblems(t, err, true)
	if len(res.Findings) != 1 || res.Findings[0].Kind != "CronJob" || strings.Join(res.Findings[0].Hook, ",") != "pre-upgrade" || res.Findings[0].Replacement != "batch/v1" {
		t.Fatalf("res = %+v", res)
	}
}

func TestDeprecatedRevision(t *testing.T) {
	c := newCluster(t)
	c.release("web", "prod", 1, common.StatusSuperseded, ingressManifest)
	c.release("web", "prod", 2, common.StatusDeployed, currentManifest)
	res, err := deprecatedRun(t, c, "web", "--target-version", "1.30")
	wantProblems(t, err, false)
	if len(res.Findings) != 0 {
		t.Fatalf("latest: %+v", res)
	}
	res, err = deprecatedRun(t, c, "web", "--target-version", "1.30", "--revision", "1")
	wantProblems(t, err, true)
	if len(res.Findings) != 1 {
		t.Fatalf("rev 1: %+v", res)
	}
}

func TestDeprecatedUnknownRelease(t *testing.T) {
	if _, err := run(t, newCluster(t), "prod", "deprecated", "nope", "--target-version", "1.30"); err == nil {
		t.Fatal("want error")
	}
}
