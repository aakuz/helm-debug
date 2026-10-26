package cli

import (
	"errors"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/release/common"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var helmReleaseInfo = kindInfo{schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}, "helmreleases", true}

func helmRelease(ns, name string, spec map[string]any, conds ...map[string]any) *unstructured.Unstructured {
	items := make([]any, len(conds))
	for i, c := range conds {
		items[i] = c
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec":     spec,
		"status":   map[string]any{"conditions": items},
	}}
}

func cond(typ, status, reason, msg string) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": msg}
}

func TestResolveHelmRelease(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 60)
	tests := []struct {
		name                     string
		spec                     map[string]any
		hrName                   string
		wantName, wantNs, wantSt string
	}{
		{"default", nil, "podinfo", "podinfo", "flux", "flux"},
		{"targetNamespace", map[string]any{"targetNamespace": "apps"}, "podinfo", "apps-podinfo", "apps", "flux"},
		{"releaseName", map[string]any{"releaseName": "custom", "targetNamespace": "apps"}, "podinfo", "custom", "apps", "flux"},
		{"storageNamespace", map[string]any{"storageNamespace": "store"}, "podinfo", "podinfo", "flux", "store"},
		// 40 chars + "-" + first 12 hex chars of sha256 of the full name
		{"long name", nil, long, strings.Repeat("a", 40) + "-11ee391211c6", "flux", "flux"},
		{"long releaseName is kept", map[string]any{"releaseName": strings.Repeat("r", 60)}, "x", strings.Repeat("r", 60), "flux", "flux"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			name, ns, st := resolveHelmRelease(helmRelease("flux", tt.hrName, tt.spec))
			if name != tt.wantName || ns != tt.wantNs || st != tt.wantSt {
				t.Errorf("got %q, %q, %q; want %q, %q, %q", name, ns, st, tt.wantName, tt.wantNs, tt.wantSt)
			}
		})
	}
}

func TestResolveHelmReleaseLongTargetNamespaceName(t *testing.T) {
	t.Parallel()
	name, _, _ := resolveHelmRelease(helmRelease("flux", strings.Repeat("a", 50), map[string]any{"targetNamespace": "apps"}))
	if len(name) != 53 || !strings.HasPrefix(name, "apps-"+strings.Repeat("a", 35)+"-") {
		t.Errorf("name = %q", name)
	}
}

func fluxCluster(t *testing.T, hr *unstructured.Unstructured) *cluster {
	c := newCluster(t)
	c.extraKinds = []kindInfo{helmReleaseInfo}
	if hr != nil {
		c.live = append(c.live, hr)
	}
	return c
}

func TestFluxStatusHealthy(t *testing.T) {
	c := fluxCluster(t, helmRelease("flux", "podinfo", map[string]any{"targetNamespace": "apps"},
		cond("Reconciling", "False", "ReconciliationSucceeded", "Helm upgrade succeeded"),
		cond("Released", "True", "UpgradeSucceeded", "Helm upgrade succeeded"),
		cond("Ready", "True", "ReconciliationSucceeded", "Helm upgrade succeeded")))
	// records live in the HelmRelease namespace unless spec.storageNamespace says otherwise
	c.releaseStoredIn("flux", "apps-podinfo", "apps", 1, common.StatusSuperseded, "", nil)
	c.releaseStoredIn("flux", "apps-podinfo", "apps", 2, common.StatusDeployed, "", nil)
	out, err := run(t, c, "flux", "flux", "status", "podinfo")
	wantProblems(t, err, false)
	golden(t, out)
}

func TestFluxStatusMax(t *testing.T) {
	c := fluxCluster(t, helmRelease("flux", "podinfo", nil, cond("Ready", "True", "ok", "")))
	for v := 1; v <= 3; v++ {
		c.releaseStoredIn("flux", "podinfo", "flux", v, common.StatusDeployed, "", nil)
	}
	out, err := run(t, c, "flux", "flux", "status", "podinfo", "--max", "2")
	wantProblems(t, err, false)
	if strings.Contains(out, "1.1.0") || !strings.Contains(out, "1.2.0") || !strings.Contains(out, "1.3.0") {
		t.Errorf("want only the latest 2 revisions:\n%s", out)
	}
}

func TestFluxStatusNotReady(t *testing.T) {
	c := fluxCluster(t, helmRelease("flux", "podinfo", nil,
		cond("Ready", "False", "UpgradeFailed", "Helm upgrade failed: timed out waiting for the condition")))
	c.releaseStoredIn("flux", "podinfo", "flux", 1, common.StatusDeployed, "", nil)
	c.releaseStoredIn("flux", "podinfo", "flux", 2, common.StatusFailed, "", nil)
	out, err := run(t, c, "flux", "flux", "status", "podinfo")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestFluxStatusReadyMissing(t *testing.T) {
	c := fluxCluster(t, helmRelease("flux", "podinfo", nil))
	c.releaseStoredIn("flux", "podinfo", "flux", 1, common.StatusDeployed, "", nil)
	_, err := run(t, c, "flux", "flux", "status", "podinfo")
	wantProblems(t, err, true)
}

func TestFluxStatusPending(t *testing.T) {
	c := fluxCluster(t, helmRelease("flux", "podinfo", nil, cond("Ready", "True", "ok", "")))
	c.releaseStoredIn("flux", "podinfo", "flux", 1, common.StatusDeployed, "", nil)
	c.releaseStoredIn("flux", "podinfo", "flux", 2, common.StatusPendingUpgrade, "", nil)
	out, err := run(t, c, "flux", "flux", "status", "podinfo")
	wantProblems(t, err, true)
	if !strings.Contains(out, "helm rollback podinfo 1 -n flux") {
		t.Errorf("want unlock command:\n%s", out)
	}
}

func TestFluxStatusNoRecords(t *testing.T) {
	c := fluxCluster(t, helmRelease("flux", "podinfo", map[string]any{"storageNamespace": "store"}, cond("Ready", "True", "ok", "")))
	out, err := run(t, c, "flux", "flux", "status", "podinfo")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestFluxStatusCRDAbsent(t *testing.T) {
	_, err := run(t, newCluster(t), "flux", "flux", "status", "podinfo")
	if err == nil || errors.Is(err, ErrProblems) || !strings.Contains(err.Error(), "HelmRelease CRD (helm.toolkit.fluxcd.io) is not installed") {
		t.Fatalf("got %v", err)
	}
}

func TestFluxStatusNotFound(t *testing.T) {
	_, err := run(t, fluxCluster(t, nil), "flux", "flux", "status", "podinfo")
	if err == nil || errors.Is(err, ErrProblems) || err.Error() != "helmrelease flux/podinfo not found" {
		t.Fatalf("got %v", err)
	}
}

func TestFluxStatusJSON(t *testing.T) {
	c := fluxCluster(t, helmRelease("flux", "podinfo", nil, cond("Ready", "True", "ok", "fine")))
	c.releaseStoredIn("flux", "podinfo", "flux", 1, common.StatusDeployed, "", nil)
	out, err := run(t, c, "flux", "flux", "status", "podinfo", "-o", "json")
	wantProblems(t, err, false)
	golden(t, out)
}
