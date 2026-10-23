package kube

import (
	"fmt"
	"strconv"
	"strings"
)

// Statuses of an apiVersion against a target Kubernetes version.
const (
	StatusDeprecated = "deprecated"
	StatusRemoved    = "removed"
)

// Version is a Kubernetes minor version.
type Version struct{ Major, Minor int }

func (v Version) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// AtLeast reports whether v is the same as or newer than o.
func (v Version) AtLeast(o Version) bool {
	return v.Major > o.Major || (v.Major == o.Major && v.Minor >= o.Minor)
}

// ParseVersion reads "1.25", "v1.25.3", "1.30+" or "v1.28.4-eks-abc123".
func ParseVersion(s string) (Version, error) {
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".", 3)
	if len(parts) < 2 {
		return Version{}, fmt.Errorf("invalid Kubernetes version %q (want e.g. 1.31)", s)
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(strings.TrimRight(leadingDigits(parts[1]), "+"))
	if err1 != nil || err2 != nil {
		return Version{}, fmt.Errorf("invalid Kubernetes version %q (want e.g. 1.31)", s)
	}
	return Version{major, minor}, nil
}

func leadingDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// Removal describes an apiVersion+kind that Kubernetes deprecated and later stopped serving.
type Removal struct {
	APIVersion   string
	Kind         string
	DeprecatedIn Version
	RemovedIn    Version
	// Replacement is the apiVersion to migrate to; empty when the kind has no successor.
	Replacement string
}

// StatusAt is StatusRemoved or StatusDeprecated for a cluster at the target version, or "" when still current.
func (r Removal) StatusAt(target Version) string {
	switch {
	case target.AtLeast(r.RemovedIn):
		return StatusRemoved
	case target.AtLeast(r.DeprecatedIn):
		return StatusDeprecated
	}
	return ""
}

// LookupRemoval finds the removal entry for an apiVersion and kind.
func LookupRemoval(apiVersion, kind string) (Removal, bool) {
	for _, r := range removals {
		if r.APIVersion == apiVersion && r.Kind == kind {
			return r, true
		}
	}
	return Removal{}, false
}

// IsBuiltinAPIVersion reports whether the apiVersion belongs to a group shipped with Kubernetes,
// as opposed to a CRD group whose absence only means the CRD is not installed.
func IsBuiltinAPIVersion(apiVersion string) bool {
	group, _, ok := strings.Cut(apiVersion, "/")
	return !ok || !strings.Contains(group, ".") || strings.HasSuffix(group, ".k8s.io")
}

func v(minor int) Version { return Version{1, minor} }

// removals follows the Kubernetes deprecated API migration guide
// (https://kubernetes.io/docs/reference/using-api/deprecation-guide/).
var removals = []Removal{
	{"extensions/v1beta1", "Deployment", v(9), v(16), "apps/v1"},
	{"extensions/v1beta1", "DaemonSet", v(9), v(16), "apps/v1"},
	{"extensions/v1beta1", "ReplicaSet", v(9), v(16), "apps/v1"},
	{"extensions/v1beta1", "NetworkPolicy", v(9), v(16), "networking.k8s.io/v1"},
	{"extensions/v1beta1", "PodSecurityPolicy", v(10), v(16), "policy/v1beta1"},
	{"apps/v1beta1", "Deployment", v(9), v(16), "apps/v1"},
	{"apps/v1beta1", "StatefulSet", v(9), v(16), "apps/v1"},
	{"apps/v1beta2", "Deployment", v(9), v(16), "apps/v1"},
	{"apps/v1beta2", "StatefulSet", v(9), v(16), "apps/v1"},
	{"apps/v1beta2", "DaemonSet", v(9), v(16), "apps/v1"},
	{"apps/v1beta2", "ReplicaSet", v(9), v(16), "apps/v1"},

	{"extensions/v1beta1", "Ingress", v(14), v(22), "networking.k8s.io/v1"},
	{"networking.k8s.io/v1beta1", "Ingress", v(19), v(22), "networking.k8s.io/v1"},
	{"networking.k8s.io/v1beta1", "IngressClass", v(19), v(22), "networking.k8s.io/v1"},
	{"rbac.authorization.k8s.io/v1beta1", "Role", v(17), v(22), "rbac.authorization.k8s.io/v1"},
	{"rbac.authorization.k8s.io/v1beta1", "RoleBinding", v(17), v(22), "rbac.authorization.k8s.io/v1"},
	{"rbac.authorization.k8s.io/v1beta1", "ClusterRole", v(17), v(22), "rbac.authorization.k8s.io/v1"},
	{"rbac.authorization.k8s.io/v1beta1", "ClusterRoleBinding", v(17), v(22), "rbac.authorization.k8s.io/v1"},
	{"admissionregistration.k8s.io/v1beta1", "MutatingWebhookConfiguration", v(16), v(22), "admissionregistration.k8s.io/v1"},
	{"admissionregistration.k8s.io/v1beta1", "ValidatingWebhookConfiguration", v(16), v(22), "admissionregistration.k8s.io/v1"},
	{"apiextensions.k8s.io/v1beta1", "CustomResourceDefinition", v(16), v(22), "apiextensions.k8s.io/v1"},
	{"apiregistration.k8s.io/v1beta1", "APIService", v(19), v(22), "apiregistration.k8s.io/v1"},
	{"certificates.k8s.io/v1beta1", "CertificateSigningRequest", v(19), v(22), "certificates.k8s.io/v1"},
	{"coordination.k8s.io/v1beta1", "Lease", v(19), v(22), "coordination.k8s.io/v1"},
	{"scheduling.k8s.io/v1beta1", "PriorityClass", v(19), v(22), "scheduling.k8s.io/v1"},
	{"storage.k8s.io/v1beta1", "CSIDriver", v(19), v(22), "storage.k8s.io/v1"},
	{"storage.k8s.io/v1beta1", "CSINode", v(17), v(22), "storage.k8s.io/v1"},
	{"storage.k8s.io/v1beta1", "StorageClass", v(19), v(22), "storage.k8s.io/v1"},
	{"storage.k8s.io/v1beta1", "VolumeAttachment", v(19), v(22), "storage.k8s.io/v1"},
	{"authentication.k8s.io/v1beta1", "TokenReview", v(19), v(22), "authentication.k8s.io/v1"},
	{"authorization.k8s.io/v1beta1", "SubjectAccessReview", v(19), v(22), "authorization.k8s.io/v1"},
	{"authorization.k8s.io/v1beta1", "LocalSubjectAccessReview", v(19), v(22), "authorization.k8s.io/v1"},
	{"authorization.k8s.io/v1beta1", "SelfSubjectAccessReview", v(19), v(22), "authorization.k8s.io/v1"},
	{"authorization.k8s.io/v1beta1", "SelfSubjectRulesReview", v(19), v(22), "authorization.k8s.io/v1"},

	{"batch/v1beta1", "CronJob", v(21), v(25), "batch/v1"},
	{"discovery.k8s.io/v1beta1", "EndpointSlice", v(21), v(25), "discovery.k8s.io/v1"},
	{"events.k8s.io/v1beta1", "Event", v(19), v(25), "events.k8s.io/v1"},
	{"autoscaling/v2beta1", "HorizontalPodAutoscaler", v(22), v(25), "autoscaling/v2"},
	{"policy/v1beta1", "PodDisruptionBudget", v(21), v(25), "policy/v1"},
	{"policy/v1beta1", "PodSecurityPolicy", v(21), v(25), ""},
	{"node.k8s.io/v1beta1", "RuntimeClass", v(22), v(25), "node.k8s.io/v1"},

	{"autoscaling/v2beta2", "HorizontalPodAutoscaler", v(23), v(26), "autoscaling/v2"},
	{"storage.k8s.io/v1beta1", "CSIStorageCapacity", v(24), v(27), "storage.k8s.io/v1"},

	{"flowcontrol.apiserver.k8s.io/v1beta1", "FlowSchema", v(23), v(26), "flowcontrol.apiserver.k8s.io/v1"},
	{"flowcontrol.apiserver.k8s.io/v1beta1", "PriorityLevelConfiguration", v(23), v(26), "flowcontrol.apiserver.k8s.io/v1"},
	{"flowcontrol.apiserver.k8s.io/v1beta2", "FlowSchema", v(26), v(29), "flowcontrol.apiserver.k8s.io/v1"},
	{"flowcontrol.apiserver.k8s.io/v1beta2", "PriorityLevelConfiguration", v(26), v(29), "flowcontrol.apiserver.k8s.io/v1"},
	{"flowcontrol.apiserver.k8s.io/v1beta3", "FlowSchema", v(29), v(32), "flowcontrol.apiserver.k8s.io/v1"},
	{"flowcontrol.apiserver.k8s.io/v1beta3", "PriorityLevelConfiguration", v(29), v(32), "flowcontrol.apiserver.k8s.io/v1"},
}
