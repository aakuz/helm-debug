package kube

import "testing"

func TestParseVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    Version
		wantErr bool
	}{
		{"1.25", Version{1, 25}, false},
		{"v1.25.3", Version{1, 25}, false},
		{"v1.28.4-eks-abc123", Version{1, 28}, false},
		{"1.30+", Version{1, 30}, false},
		{"", Version{}, true},
		{"banana", Version{}, true},
		{"1", Version{}, true},
		{"1.x", Version{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseVersion(tt.in)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("ParseVersion(%q) = %v, %v", tt.in, got, err)
			}
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	t.Parallel()
	if !(Version{1, 25}).AtLeast(Version{1, 25}) || (Version{1, 24}).AtLeast(Version{1, 25}) || !(Version{2, 0}).AtLeast(Version{1, 99}) {
		t.Fatal("AtLeast is wrong")
	}
}

func TestLookupRemoval(t *testing.T) {
	t.Parallel()
	r, ok := LookupRemoval("policy/v1beta1", "PodDisruptionBudget")
	if !ok || r.RemovedIn != (Version{1, 25}) || r.Replacement != "policy/v1" {
		t.Fatalf("got %+v, %v", r, ok)
	}
	if _, ok := LookupRemoval("batch/v2alpha1", "CronJob"); !ok {
		t.Fatal("batch/v2alpha1 CronJob is missing")
	}
	if _, ok := LookupRemoval("apps/v1", "Deployment"); ok {
		t.Fatal("apps/v1 Deployment is current")
	}
	if _, ok := LookupRemoval("policy/v1beta1", "Widget"); ok {
		t.Fatal("kind must match")
	}
}

func TestRemovalStatusAt(t *testing.T) {
	t.Parallel()
	r := Removal{DeprecatedIn: Version{1, 21}, RemovedIn: Version{1, 25}}
	for _, tt := range []struct {
		target Version
		want   string
	}{
		{Version{1, 20}, ""}, {Version{1, 21}, StatusDeprecated}, {Version{1, 24}, StatusDeprecated}, {Version{1, 25}, StatusRemoved}, {Version{1, 31}, StatusRemoved},
	} {
		if got := r.StatusAt(tt.target); got != tt.want {
			t.Errorf("StatusAt(%v) = %q, want %q", tt.target, got, tt.want)
		}
	}
}

func TestRemovalsTableIsConsistent(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, r := range removals {
		key := r.APIVersion + "/" + r.Kind
		if seen[key] {
			t.Errorf("duplicate %s", key)
		}
		seen[key] = true
		if !r.RemovedIn.AtLeast(r.DeprecatedIn) {
			t.Errorf("%s: removed before deprecated", key)
		}
		if r.Replacement == r.APIVersion {
			t.Errorf("%s: replacement equals itself", key)
		}
	}
}

func TestIsBuiltinAPIVersion(t *testing.T) {
	t.Parallel()
	for apiVersion, want := range map[string]bool{
		"v1": true, "apps/v1": true, "extensions/v1beta1": true, "batch/v1": true,
		"networking.k8s.io/v1": true, "cert-manager.io/v1": false, "monitoring.coreos.com/v1": false,
		"gateway.networking.k8s.io/v1": false, "snapshot.storage.k8s.io/v1": false, "metrics.k8s.io/v1beta1": false,
		"rbac.authorization.k8s.io/v1": true, "flowcontrol.apiserver.k8s.io/v1": true,
	} {
		if got := IsBuiltinAPIVersion(apiVersion); got != want {
			t.Errorf("IsBuiltinAPIVersion(%q) = %v, want %v", apiVersion, got, want)
		}
	}
}
