package kube

import (
	"reflect"
	"testing"
)

const (
	refDeployment  = "https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/deployment-v1/#DeploymentSpec"
	refReplicaSet  = "https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/replica-set-v1/#ReplicaSetSpec"
	refDaemonSet   = "https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/daemon-set-v1/#DaemonSetSpec"
	refStatefulSet = "https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/stateful-set-v1/#StatefulSetSpec"
	refJob         = "https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/job-v1/#JobSpec"
	refService     = "https://kubernetes.io/docs/reference/kubernetes-api/service-resources/service-v1/#ServiceSpec"
	refPVC         = "https://kubernetes.io/docs/reference/kubernetes-api/config-and-storage-resources/persistent-volume-claim-v1/#PersistentVolumeClaimSpec"
	refStorageCls  = "https://kubernetes.io/docs/reference/kubernetes-api/config-and-storage-resources/storage-class-v1/"
	refRoleBinding = "https://kubernetes.io/docs/reference/kubernetes-api/authorization-resources/role-binding-v1/"
	refCRB         = "https://kubernetes.io/docs/reference/kubernetes-api/authorization-resources/cluster-role-binding-v1/"

	noteDefault     = "the next upgrade or rollback recreates it"
	noteStatefulSet = "keeps the Pods and PersistentVolumeClaims; the next upgrade or rollback recreates the StatefulSet and it adopts them"
	notePVC         = "also deletes the volume and its data when the reclaim policy is Delete: back up first; the next upgrade or rollback recreates the claim"
)

func TestImmutableChangesFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		apiVersion string
		kind       string
		changed    []string
		wantFields []string
		wantRef    string
	}{
		{"deployment selector", "apps/v1", "Deployment", []string{"/spec/selector"}, []string{"/spec/selector"}, refDeployment},
		{"deployment selector nested", "apps/v1", "Deployment", []string{"/spec/selector/matchLabels/app"}, []string{"/spec/selector"}, refDeployment},
		{"deployment other version", "apps/v1beta2", "Deployment", []string{"/spec/selector/matchLabels/app"}, []string{"/spec/selector"}, refDeployment},
		{"replicaset selector", "apps/v1", "ReplicaSet", []string{"/spec/selector/matchLabels/app"}, []string{"/spec/selector"}, refReplicaSet},
		{"daemonset selector", "apps/v1", "DaemonSet", []string{"/spec/selector"}, []string{"/spec/selector"}, refDaemonSet},
		{"statefulset all paths", "apps/v1", "StatefulSet",
			[]string{"/spec/volumeClaimTemplates/0/spec/resources/requests/storage", "/spec/serviceName", "/spec/selector/matchLabels/app", "/spec/podManagementPolicy"},
			[]string{"/spec/podManagementPolicy", "/spec/selector", "/spec/serviceName", "/spec/volumeClaimTemplates"}, refStatefulSet},
		{"statefulset dedup in table order", "apps/v1", "StatefulSet",
			[]string{"/spec/volumeClaimTemplates/0/metadata/name", "/spec/serviceName", "/spec/volumeClaimTemplates/1", "/spec/replicas"},
			[]string{"/spec/serviceName", "/spec/volumeClaimTemplates"}, refStatefulSet},
		{"job template nested", "batch/v1", "Job", []string{"/spec/template/spec/containers/0/image"}, []string{"/spec/template"}, refJob},
		{"job completion mode", "batch/v1", "Job", []string{"/spec/template/metadata/labels/a", "/spec/completionMode", "/spec/backoffLimit"}, []string{"/spec/completionMode", "/spec/template"}, refJob},
		{"service clusterIP", "v1", "Service", []string{"/spec/clusterIP"}, []string{"/spec/clusterIP"}, refService},
		{"service clusterIPs", "v1", "Service", []string{"/spec/clusterIPs/0", "/spec/clusterIP"}, []string{"/spec/clusterIP", "/spec/clusterIPs"}, refService},
		{"pvc several", "v1", "PersistentVolumeClaim", []string{"/spec/volumeName", "/spec/storageClassName", "/spec/accessModes/0"}, []string{"/spec/accessModes", "/spec/storageClassName", "/spec/volumeName"}, refPVC},
		{"pvc dataSource volumeMode selector", "v1", "PersistentVolumeClaim", []string{"/spec/volumeMode", "/spec/selector/matchLabels/a", "/spec/dataSource/name"}, []string{"/spec/dataSource", "/spec/selector", "/spec/volumeMode"}, refPVC},
		{"storageclass", "storage.k8s.io/v1", "StorageClass", []string{"/volumeBindingMode", "/parameters/type", "/reclaimPolicy", "/provisioner"}, []string{"/parameters", "/provisioner", "/reclaimPolicy", "/volumeBindingMode"}, refStorageCls},
		{"rolebinding roleRef", "rbac.authorization.k8s.io/v1", "RoleBinding", []string{"/roleRef/name"}, []string{"/roleRef"}, refRoleBinding},
		{"clusterrolebinding roleRef", "rbac.authorization.k8s.io/v1", "ClusterRoleBinding", []string{"/subjects/0/name", "/roleRef"}, []string{"/roleRef"}, refCRB},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ImmutableChanges(tt.apiVersion, tt.kind, "prod", "x", tt.changed)
			if got == nil {
				t.Fatalf("ImmutableChanges(%q, %q, %v) = nil, want fields %v", tt.apiVersion, tt.kind, tt.changed, tt.wantFields)
			}
			if !reflect.DeepEqual(got.Fields, tt.wantFields) {
				t.Errorf("Fields = %q, want %q", got.Fields, tt.wantFields)
			}
			if got.Reference != tt.wantRef {
				t.Errorf("Reference = %q, want %q", got.Reference, tt.wantRef)
			}
		})
	}
}

func TestImmutableChangesNoMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		apiVersion string
		kind       string
		changed    []string
	}{
		{"sibling prefix", "apps/v1", "Deployment", []string{"/spec/selectorX"}},
		{"sibling prefix nested", "apps/v1", "Deployment", []string{"/spec/selectorX/a"}},
		{"ancestor", "apps/v1", "Deployment", []string{"/spec"}},
		{"root", "apps/v1", "Deployment", []string{""}},
		{"mutable field", "apps/v1", "Deployment", []string{"/spec/replicas", "/spec/template/spec/containers/0/image"}},
		{"statefulset replicas", "apps/v1", "StatefulSet", []string{"/spec/replicas"}},
		{"service ports", "v1", "Service", []string{"/spec/ports/0/port", "/spec/clusterIPX"}},
		{"unknown kind", "v1", "ConfigMap", []string{"/data/a"}},
		{"unknown kind with table path", "v1", "ConfigMap", []string{"/spec/selector"}},
		{"same kind wrong group", "example.com/v1", "Deployment", []string{"/spec/selector"}},
		{"core kind in apps group", "apps/v1", "Service", []string{"/spec/clusterIP"}},
		{"apps kind in core group", "v1", "StatefulSet", []string{"/spec/serviceName"}},
		{"empty changed", "apps/v1", "Deployment", nil},
		{"empty slice", "apps/v1", "StatefulSet", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ImmutableChanges(tt.apiVersion, tt.kind, "prod", "x", tt.changed); got != nil {
				t.Errorf("ImmutableChanges(%q, %q, %q) = %+v, want nil", tt.apiVersion, tt.kind, tt.changed, got)
			}
		})
	}
}

func TestImmutableChangesCommandAndNote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                        string
		apiVersion, kind, ns, obj   string
		changed                     []string
		wantCommand, wantNote, wRef string
	}{
		{"statefulset", "apps/v1", "StatefulSet", "prod", "db", []string{"/spec/serviceName"},
			"kubectl -n prod delete statefulset/db --cascade=orphan", noteStatefulSet, refStatefulSet},
		{"statefulset no namespace", "apps/v1", "StatefulSet", "", "db", []string{"/spec/serviceName"},
			"kubectl delete statefulset/db --cascade=orphan", noteStatefulSet, refStatefulSet},
		{"pvc", "v1", "PersistentVolumeClaim", "prod", "data", []string{"/spec/storageClassName"},
			"kubectl -n prod delete persistentvolumeclaim/data", notePVC, refPVC},
		{"namespaced default", "apps/v1", "Deployment", "prod", "api", []string{"/spec/selector/matchLabels/app"},
			"kubectl -n prod delete deployment/api", noteDefault, refDeployment},
		{"job default", "batch/v1", "Job", "prod", "migrate", []string{"/spec/template/spec/containers/0/image"},
			"kubectl -n prod delete job/migrate", noteDefault, refJob},
		{"rolebinding default", "rbac.authorization.k8s.io/v1", "RoleBinding", "prod", "reader", []string{"/roleRef"},
			"kubectl -n prod delete rolebinding/reader", noteDefault, refRoleBinding},
		{"namespaced empty namespace", "apps/v1", "Deployment", "", "api", []string{"/spec/selector"},
			"kubectl delete deployment/api", noteDefault, refDeployment},
		{"cluster-scoped ignores namespace", "storage.k8s.io/v1", "StorageClass", "prod", "fast", []string{"/provisioner"},
			"kubectl delete storageclass/fast", noteDefault, refStorageCls},
		{"cluster-scoped binding ignores namespace", "rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "prod", "admins", []string{"/roleRef/name"},
			"kubectl delete clusterrolebinding/admins", noteDefault, refCRB},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ImmutableChanges(tt.apiVersion, tt.kind, tt.ns, tt.obj, tt.changed)
			if got == nil {
				t.Fatal("ImmutableChanges = nil")
			}
			if got.Command != tt.wantCommand {
				t.Errorf("Command = %q, want %q", got.Command, tt.wantCommand)
			}
			if got.Note != tt.wantNote {
				t.Errorf("Note = %q, want %q", got.Note, tt.wantNote)
			}
			if got.Reference != tt.wRef {
				t.Errorf("Reference = %q, want %q", got.Reference, tt.wRef)
			}
		})
	}
}
