package helm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	metafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestNormalizeDriver(t *testing.T) {
	for in, want := range map[string]string{"": StorageSecret, "Secrets": StorageSecret, "configmap": StorageConfigMap, "ConfigMaps": StorageConfigMap} {
		if got, err := NormalizeDriver(in); err != nil || got != want {
			t.Errorf("NormalizeDriver(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormalizeDriver("sql"); err == nil {
		t.Error("NormalizeDriver(sql) should fail")
	}
}

func TestRecordVersion(t *testing.T) {
	cases := []struct {
		object, label string
		want          int
	}{
		{"sh.helm.release.v1.app.v7", "7", 7},
		{"sh.helm.release.v1.app.v12", "", 12},
		{"sh.helm.release.v1.my.app.v3", "", 3},
		{"garbage", "", 0},
	}
	for _, c := range cases {
		if got := recordVersion(c.object, c.label); got != c.want {
			t.Errorf("recordVersion(%q, %q) = %d, want %d", c.object, c.label, got, c.want)
		}
	}
}

func TestLatestOnly(t *testing.T) {
	refs := []Ref{
		{Namespace: "prod", Release: "b", Version: 1},
		{Namespace: "prod", Release: "b", Version: 3},
		{Namespace: "dev", Release: "b", Version: 2},
		{Namespace: "prod", Release: "a", Version: 1},
	}
	want := []Ref{
		{Namespace: "dev", Release: "b", Version: 2},
		{Namespace: "prod", Release: "a", Version: 1},
		{Namespace: "prod", Release: "b", Version: 3},
	}
	if got := LatestOnly(refs); !reflect.DeepEqual(got, want) {
		t.Errorf("LatestOnly = %+v, want %+v", got, want)
	}
}

func TestNormalizeDriverTable(t *testing.T) {
	for _, c := range []struct {
		in, want string
		wantErr  bool
	}{
		{"SECRET", StorageSecret, false},
		{"CONFIGMAPS", StorageConfigMap, false},
		{"ConfigMap", StorageConfigMap, false},
		{" secret", "", true},
	} {
		got, err := NormalizeDriver(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("NormalizeDriver(%q) = %q, %v; want %q (err=%v)", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestStoreGVR(t *testing.T) {
	for drv, want := range map[string]string{StorageSecret: "secrets", StorageConfigMap: "configmaps", "": "secrets"} {
		got := (&Store{Driver: drv}).gvr()
		if got != (schema.GroupVersionResource{Version: "v1", Resource: want}) {
			t.Errorf("gvr(%q) = %v, want v1/%s", drv, got, want)
		}
	}
}

func testRelease(name, ns string, version int) *rspb.Release {
	return &rspb.Release{
		Name: name, Namespace: ns, Version: version,
		Info:     &rspb.Info{Status: "deployed"},
		Manifest: "kind: x\n",
	}
}

// seed creates release records with the real Helm driver for the given storage.
func seed(t *testing.T, drv string, rels ...*rspb.Release) *Store {
	t.Helper()
	s := &Store{Kube: fake.NewClientset(), Driver: drv}
	for _, r := range rels {
		d := s.driver(r.Namespace)
		if err := d.Create(fmt.Sprintf("sh.helm.release.v1.%s.v%d", r.Name, r.Version), r); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

var drivers = []string{StorageSecret, StorageConfigMap}

func TestStoreHistoryAndRevision(t *testing.T) {
	for _, drv := range drivers {
		t.Run(drv, func(t *testing.T) {
			s := seed(t, drv,
				testRelease("app", "prod", 1), testRelease("app", "prod", 3), testRelease("app", "prod", 2),
				testRelease("app", "dev", 9), testRelease("other", "prod", 5))

			hist, err := s.History("app", "prod")
			if err != nil {
				t.Fatal(err)
			}
			var got []int
			for _, r := range hist {
				got = append(got, r.Version)
				if r.Name != "app" || r.Namespace != "prod" {
					t.Errorf("unexpected release %s/%s", r.Namespace, r.Name)
				}
			}
			if !reflect.DeepEqual(got, []int{3, 2, 1}) {
				t.Errorf("History versions = %v, want [3 2 1]", got)
			}

			for _, c := range []struct {
				rev, want int
			}{{0, 3}, {1, 1}, {2, 2}} {
				r, err := s.Revision("app", "prod", c.rev)
				if err != nil || r.Version != c.want {
					t.Errorf("Revision(%d) = %v, %v; want version %d", c.rev, r, err, c.want)
				}
			}

			if _, err := s.Revision("app", "prod", 7); !errors.Is(err, ErrReleaseNotFound) {
				t.Errorf("unknown revision err = %v, want ErrReleaseNotFound", err)
			}
			if _, err := s.Revision("nope", "prod", 0); !errors.Is(err, ErrReleaseNotFound) {
				t.Errorf("unknown release err = %v, want ErrReleaseNotFound", err)
			}
			if _, err := s.History("app", "staging"); !errors.Is(err, ErrReleaseNotFound) {
				t.Errorf("other namespace err = %v, want ErrReleaseNotFound", err)
			}
		})
	}
}

func TestStoreDriverIsolation(t *testing.T) {
	s := seed(t, StorageSecret, testRelease("app", "prod", 1))
	s.Driver = StorageConfigMap
	if _, err := s.History("app", "prod"); !errors.Is(err, ErrReleaseNotFound) {
		t.Errorf("configmap driver should not see secret records, err = %v", err)
	}
}

func corruptObject(drv, ns, release, version string) runtime.Object {
	meta := metav1.ObjectMeta{Namespace: ns, Name: "sh.helm.release.v1." + release + ".v" + version,
		Labels: map[string]string{"owner": "helm", "name": release, "version": version, "status": "deployed"}}
	if drv == StorageConfigMap {
		return &corev1.ConfigMap{ObjectMeta: meta, Data: map[string]string{"release": "%%%not-base64%%%"}}
	}
	return &corev1.Secret{ObjectMeta: meta, Data: map[string][]byte{"release": []byte("%%%not-base64%%%")}}
}

func TestStoreCorruptPayload(t *testing.T) {
	for _, drv := range drivers {
		t.Run(drv, func(t *testing.T) {
			obj := corruptObject(drv, "prod", "bad", "1")
			s := &Store{Kube: fake.NewClientset(obj), Driver: drv}

			// Undecodable records are skipped by History and reported as not found.
			_, err := s.History("bad", "prod")
			if !errors.Is(err, ErrReleaseNotFound) {
				t.Errorf("History err = %v, want ErrReleaseNotFound", err)
			}

			// Load surfaces the decode error.
			rel, err := s.Load(Ref{Namespace: "prod", Object: "sh.helm.release.v1.bad.v1", Release: "bad", Version: 1})
			if err == nil || rel != nil {
				t.Errorf("Load = %v, %v; want decode error", rel, err)
			}
			if errors.Is(err, driver.ErrReleaseNotFound) {
				t.Errorf("Load error should be a decode error, got not-found: %v", err)
			}
		})
	}
}

func TestStoreLoad(t *testing.T) {
	for _, drv := range drivers {
		t.Run(drv, func(t *testing.T) {
			s := seed(t, drv, testRelease("app", "prod", 2))
			rel, err := s.Load(Ref{Namespace: "prod", Object: "sh.helm.release.v1.app.v2"})
			if err != nil || rel.Name != "app" || rel.Version != 2 || rel.Manifest != "kind: x\n" {
				t.Errorf("Load = %+v, %v", rel, err)
			}
			if _, err := s.Load(Ref{Namespace: "prod", Object: "missing"}); !errors.Is(err, driver.ErrReleaseNotFound) {
				t.Errorf("Load missing err = %v, want driver.ErrReleaseNotFound", err)
			}
		})
	}
}

func metaScheme() *runtime.Scheme {
	sc := metafake.NewTestScheme()
	for _, k := range []string{"Secret", "ConfigMap"} {
		sc.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: k}, &metav1.PartialObjectMetadata{})
		sc.AddKnownTypeWithName(schema.GroupVersionKind{Version: "v1", Kind: k + "List"}, &metav1.PartialObjectMetadataList{})
	}
	return sc
}

func partial(drv, ns, name string, labels map[string]string) *metav1.PartialObjectMetadata {
	kind := "Secret"
	if drv == StorageConfigMap {
		kind = "ConfigMap"
	}
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: kind},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
	}
}

func TestListRefs(t *testing.T) {
	helmLabels := func(rel, ver string) map[string]string {
		return map[string]string{"owner": "helm", "name": rel, "version": ver}
	}
	for _, drv := range drivers {
		t.Run(drv, func(t *testing.T) {
			objs := []runtime.Object{
				partial(drv, "prod", "sh.helm.release.v1.app.v1", helmLabels("app", "1")),
				partial(drv, "prod", "sh.helm.release.v1.app.v2", helmLabels("app", "2")),
				partial(drv, "dev", "sh.helm.release.v1.db.v4", map[string]string{"owner": "helm", "name": "db"}),
				partial(drv, "prod", "unrelated", map[string]string{"owner": "someone"}),
				partial(drv, "prod", "nolabels", nil),
			}
			s := &Store{Metadata: metafake.NewSimpleMetadataClient(metaScheme(), objs...), Driver: drv}

			all, err := s.ListRefs(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 3 {
				t.Fatalf("all namespaces: got %d refs %+v, want 3", len(all), all)
			}
			byObj := map[string]Ref{}
			for _, r := range all {
				byObj[r.Object] = r
			}
			if r := byObj["sh.helm.release.v1.db.v4"]; r != (Ref{Namespace: "dev", Object: "sh.helm.release.v1.db.v4", Release: "db", Version: 4}) {
				t.Errorf("version should fall back to object name, got %+v", r)
			}
			if r := byObj["sh.helm.release.v1.app.v2"]; r.Version != 2 || r.Release != "app" || r.Namespace != "prod" {
				t.Errorf("unexpected ref %+v", r)
			}

			prod, err := s.ListRefs(context.Background(), "prod")
			if err != nil {
				t.Fatal(err)
			}
			if len(prod) != 2 {
				t.Errorf("prod: got %+v, want 2 refs", prod)
			}
			for _, r := range prod {
				if r.Namespace != "prod" {
					t.Errorf("prod listing returned %+v", r)
				}
			}
		})
	}
}

func TestListRefsPaginationAndError(t *testing.T) {
	mc := metafake.NewSimpleMetadataClient(metaScheme())
	pages := 0
	mc.PrependReactor("list", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		pages++
		list := &metav1.List{}
		if pages == 1 {
			list.Continue = "next"
			list.Items = []runtime.RawExtension{{Object: partial(StorageSecret, "n", "sh.helm.release.v1.a.v1", map[string]string{"owner": "helm", "name": "a", "version": "1"})}}
		} else {
			list.Items = []runtime.RawExtension{{Object: partial(StorageSecret, "n", "sh.helm.release.v1.a.v2", map[string]string{"owner": "helm", "name": "a", "version": "2"})}}
		}
		return true, list, nil
	})
	s := &Store{Metadata: mc, Driver: StorageSecret}
	refs, err := s.ListRefs(context.Background(), "")
	if err != nil || len(refs) != 2 || pages != 2 || refs[1].Version != 2 {
		t.Errorf("ListRefs = %+v, %v (pages=%d); want 2 refs over 2 pages", refs, err, pages)
	}

	mc2 := metafake.NewSimpleMetadataClient(metaScheme())
	mc2.PrependReactor("list", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	s2 := &Store{Metadata: mc2, Driver: StorageConfigMap}
	if _, err := s2.ListRefs(context.Background(), ""); err == nil {
		t.Error("expected list error")
	}
}

func TestLatestOnlyEmpty(t *testing.T) {
	if got := LatestOnly(nil); len(got) != 0 {
		t.Errorf("LatestOnly(nil) = %+v, want empty", got)
	}
}
