package kube

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	discoveryfake "k8s.io/client-go/discovery/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
)

func partial(apiVersion, kind, ns, name string, labels, annotations map[string]string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiVersion, Kind: kind},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels, Annotations: annotations},
	}
}

func helmOwned(release, releaseNs string) (map[string]string, map[string]string) {
	return map[string]string{LabelManagedBy: "Helm"},
		map[string]string{AnnotationReleaseName: release, AnnotationReleaseNamespace: releaseNs}
}

func newManagedCluster(t *testing.T, objs ...runtime.Object) *Cluster {
	t.Helper()
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	verbs := metav1.Verbs{"get", "list"}
	disc := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: verbs},
			{Name: "services", Kind: "Service", Namespaced: true, Verbs: verbs},
			{Name: "services/status", Kind: "Service", Namespaced: true, Verbs: verbs},
			{Name: "bindings", Kind: "Binding", Namespaced: true, Verbs: metav1.Verbs{"create"}},
		}},
		{GroupVersion: "rbac.authorization.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "clusterroles", Kind: "ClusterRole", Verbs: verbs},
		}},
	}}}
	return &Cluster{Discovery: disc, Metadata: metadatafake.NewSimpleMetadataClient(scheme, objs...)}
}

func TestListHelmManaged(t *testing.T) {
	l, a := helmOwned("web", "prod")
	cl, ca := helmOwned("rbac", "ops")
	c := newManagedCluster(t,
		partial("v1", "ConfigMap", "prod", "web-config", l, a),
		partial("v1", "Service", "prod", "web", l, a),
		partial("v1", "Service", "dev", "web", l, a),
		partial("rbac.authorization.k8s.io/v1", "ClusterRole", "", "rbac-reader", cl, ca),
		// managed by Helm but with no release annotation: the release cannot be named
		partial("v1", "ConfigMap", "prod", "no-annotations", l, nil),
		// not managed by Helm
		partial("v1", "ConfigMap", "prod", "plain", nil, a),
	)

	got, warnings, err := c.ListHelmManaged(context.Background(), "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	want := []Managed{
		{Kind: "ClusterRole", Name: "rbac-reader", ReleaseName: "rbac", ReleaseNamespace: "ops"},
		{Kind: "ConfigMap", Namespace: "prod", Name: "web-config", ReleaseName: "web", ReleaseNamespace: "prod"},
		{Kind: "Service", Namespace: "prod", Name: "web", ReleaseName: "web", ReleaseNamespace: "prod"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListHelmManaged(prod) = %+v, want %+v", got, want)
	}

	all, _, err := c.ListHelmManaged(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("ListHelmManaged(all namespaces) returned %d objects, want 4: %+v", len(all), all)
	}
}

func TestListHelmManagedWarnsWhenKindCannotBeListed(t *testing.T) {
	l, a := helmOwned("web", "prod")
	c := newManagedCluster(t, partial("v1", "ConfigMap", "prod", "web-config", l, a))
	c.Metadata.(*metadatafake.FakeMetadataClient).PrependReactor("list", "services", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})

	got, warnings, err := c.ListHelmManaged(context.Background(), "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "web-config" {
		t.Fatalf("got %+v, want only web-config", got)
	}
	if len(warnings) != 1 {
		t.Fatalf("got warnings %v, want one about services", warnings)
	}
}

func TestListHelmManagedReportsProgress(t *testing.T) {
	c := newManagedCluster(t)
	type step struct {
		done, total int
		resource    string
	}
	var got []step
	_, _, err := c.ListHelmManaged(context.Background(), "", func(done, total int, resource string) {
		got = append(got, step{done, total, resource})
	})
	if err != nil {
		t.Fatal(err)
	}
	// subresources and kinds that cannot be listed are not counted
	want := []step{{1, 3, "configmaps"}, {2, 3, "services"}, {3, 3, "clusterroles"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("progress = %+v, want %+v", got, want)
	}
}
