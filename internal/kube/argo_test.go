package kube

import "testing"

func TestArgoTracking(t *testing.T) {
	const id = "argocd.argoproj.io/tracking-id"
	const inst = "app.kubernetes.io/instance"
	tests := []struct {
		name      string
		ann, lbl  map[string]string
		wantState string
		wantOwner string
	}{
		{"annotation plain id", map[string]string{id: "web:apps/Deployment:prod/web"}, nil, OwnershipOK, ""},
		{"annotation namespaced id", map[string]string{id: "argocd_web:apps/Deployment:prod/web"}, nil, OwnershipOK, ""},
		{"annotation other app", map[string]string{id: "other:apps/Deployment:prod/web"}, nil, OwnershipMismatch, "other"},
		{"annotation other namespace", map[string]string{id: "dev_web:apps/Deployment:prod/web"}, nil, OwnershipMismatch, "dev_web"},
		{"annotation wins over label", map[string]string{id: "other:apps/Deployment:prod/web"}, map[string]string{inst: "web"}, OwnershipMismatch, "other"},
		{"label ok", nil, map[string]string{inst: "web"}, OwnershipOK, ""},
		{"label other app", nil, map[string]string{inst: "other"}, OwnershipMismatch, "other"},
		{"empty annotation falls back to label", map[string]string{id: ""}, map[string]string{inst: "web"}, OwnershipOK, ""},
		{"neither", nil, nil, OwnershipMissing, ""},
		{"unrelated metadata", map[string]string{"a": "b"}, map[string]string{"c": "d"}, OwnershipMissing, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state, owner := ArgoTracking("web", "argocd", tt.ann, tt.lbl)
			if state != tt.wantState || owner != tt.wantOwner {
				t.Fatalf("ArgoTracking = %q, %q; want %q, %q", state, owner, tt.wantState, tt.wantOwner)
			}
		})
	}
}
