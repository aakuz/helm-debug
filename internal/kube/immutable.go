package kube

import (
	"fmt"
	"strings"
)

// ImmutableChange lists the immutable fields that changed on one object and what applying the change takes.
type ImmutableChange struct {
	Fields    []string `json:"fields"`    // table paths, in table order, each once
	Reference string   `json:"reference"` // Kubernetes API reference URL
	Command   string   `json:"command"`   // suggestion, never run
	Note      string   `json:"note"`
}

type immutableKind struct {
	paths       []string
	reference   string
	clusterWide bool
	note        string
}

type groupKind struct{ group, kind string }

const (
	immNoteRecreate    = "the next upgrade or rollback recreates it"
	immNoteStatefulSet = "keeps the Pods and PersistentVolumeClaims; the next upgrade or rollback recreates the StatefulSet and it adopts them"
	immNotePVC         = "also deletes the volume and its data when the reclaim policy is Delete: back up first; the next upgrade or rollback recreates the claim"
)

// immutableKinds is keyed by API group and kind, not version: the rules do not change between versions.
var immutableKinds = map[groupKind]immutableKind{
	{"apps", "Deployment"}: {paths: []string{"/spec/selector"}, note: immNoteRecreate,
		reference: "https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/deployment-v1/#DeploymentSpec"},
	{"apps", "ReplicaSet"}: {paths: []string{"/spec/selector"}, note: immNoteRecreate,
		reference: "https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/replica-set-v1/#ReplicaSetSpec"},
	{"apps", "DaemonSet"}: {paths: []string{"/spec/selector"}, note: immNoteRecreate,
		reference: "https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/daemon-set-v1/#DaemonSetSpec"},
	{"apps", "StatefulSet"}: {paths: []string{"/spec/podManagementPolicy", "/spec/selector", "/spec/serviceName", "/spec/volumeClaimTemplates"}, note: immNoteStatefulSet,
		reference: "https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/stateful-set-v1/#StatefulSetSpec"},
	{"batch", "Job"}: {paths: []string{"/spec/completionMode", "/spec/selector", "/spec/template"}, note: immNoteRecreate,
		reference: "https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/job-v1/#JobSpec"},
	{"", "Service"}: {paths: []string{"/spec/clusterIP", "/spec/clusterIPs"}, note: immNoteRecreate,
		reference: "https://kubernetes.io/docs/reference/kubernetes-api/service-resources/service-v1/#ServiceSpec"},
	{"", "PersistentVolumeClaim"}: {paths: []string{"/spec/accessModes", "/spec/dataSource", "/spec/selector", "/spec/storageClassName", "/spec/volumeMode", "/spec/volumeName"}, note: immNotePVC,
		reference: "https://kubernetes.io/docs/reference/kubernetes-api/config-and-storage-resources/persistent-volume-claim-v1/#PersistentVolumeClaimSpec"},
	{"storage.k8s.io", "StorageClass"}: {paths: []string{"/parameters", "/provisioner", "/reclaimPolicy", "/volumeBindingMode"}, clusterWide: true, note: immNoteRecreate,
		reference: "https://kubernetes.io/docs/reference/kubernetes-api/config-and-storage-resources/storage-class-v1/"},
	{"rbac.authorization.k8s.io", "RoleBinding"}: {paths: []string{"/roleRef"}, note: immNoteRecreate,
		reference: "https://kubernetes.io/docs/reference/kubernetes-api/authorization-resources/role-binding-v1/"},
	{"rbac.authorization.k8s.io", "ClusterRoleBinding"}: {paths: []string{"/roleRef"}, clusterWide: true, note: immNoteRecreate,
		reference: "https://kubernetes.io/docs/reference/kubernetes-api/authorization-resources/cluster-role-binding-v1/"},
}

func lookupImmutable(apiVersion, kind string) (immutableKind, bool) {
	group, _, found := strings.Cut(apiVersion, "/")
	if !found {
		group = ""
	}
	k, ok := immutableKinds[groupKind{group, kind}]
	return k, ok
}

// ImmutableNamespace returns the namespace to show for an object of a kind in the table: "" for the
// cluster-scoped ones, the given namespace for everything else.
func ImmutableNamespace(apiVersion, kind, namespace string) string {
	if k, ok := lookupImmutable(apiVersion, kind); ok && k.clusterWide {
		return ""
	}
	return namespace
}

// ImmutableChanges returns nil unless the kind is in the table and some changed path equals a table path or
// lies under it (prefix + "/"). "/spec/selectorX" does not match "/spec/selector"; an ancestor such as "/spec"
// does not match either.
func ImmutableChanges(apiVersion, kind, namespace, name string, changed []string) *ImmutableChange {
	k, ok := lookupImmutable(apiVersion, kind)
	if !ok {
		return nil
	}
	var fields []string
	for _, p := range k.paths {
		for _, c := range changed {
			if c == p || strings.HasPrefix(c, p+"/") {
				fields = append(fields, p)
				break
			}
		}
	}
	if len(fields) == 0 {
		return nil
	}
	nsFlag := ""
	if namespace = ImmutableNamespace(apiVersion, kind, namespace); namespace != "" {
		nsFlag = " -n " + namespace
	}
	cmd := fmt.Sprintf("kubectl%s delete %s/%s", nsFlag, strings.ToLower(kind), name)
	if kind == "StatefulSet" {
		// Orphaning keeps the Pods and the claims, so the recreated StatefulSet adopts them.
		cmd += " --cascade=orphan"
	}
	return &ImmutableChange{Fields: fields, Reference: k.reference, Command: cmd, Note: k.note}
}
