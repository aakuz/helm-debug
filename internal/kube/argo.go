package kube

import "strings"

// Argo CD tracking metadata keys.
const (
	AnnotationArgoTrackingID = "argocd.argoproj.io/tracking-id"
	LabelArgoInstance        = "app.kubernetes.io/instance"
)

// ArgoTracking checks whether a live object is tracked by the Argo CD Application app in appNamespace.
// It returns the verdict and, on mismatch, the foreign owner. The tracking-id annotation wins over the
// instance label, as in Argo CD's annotation tracking mode; the label is the legacy fallback.
func ArgoTracking(app, appNamespace string, annotations, labels map[string]string) (verdict, owner string) {
	if id := annotations[AnnotationArgoTrackingID]; id != "" {
		// The id is <app-id>:<group>/<kind>:<namespace>/<name>; the app id itself never contains a colon.
		appID, _, _ := strings.Cut(id, ":")
		if appID == app || appID == appNamespace+"_"+app {
			return OwnershipOK, ""
		}
		return OwnershipMismatch, appID
	}
	if v := labels[LabelArgoInstance]; v != "" {
		if v == app {
			return OwnershipOK, ""
		}
		return OwnershipMismatch, v
	}
	return OwnershipMissing, ""
}
