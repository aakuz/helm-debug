package helm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	rspb "helm.sh/helm/v4/pkg/release/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	metafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
)

var recordCreated = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// withMeta renames a stored object to key and replaces its labels (nil stays nil) and creation time.
func withMeta(t *testing.T, obj runtime.Object, key string, labels map[string]string, created time.Time) runtime.Object {
	t.Helper()
	m, err := meta.Accessor(obj)
	if err != nil {
		t.Fatal(err)
	}
	m.SetName(key)
	m.SetLabels(labels)
	m.SetCreationTimestamp(metav1.NewTime(created))
	m.SetResourceVersion("")
	return obj
}

// recordObject encodes rel with the real Helm driver and returns the stored object under key, with the given labels.
func recordObject(t *testing.T, drv, ns, key string, rel *rspb.Release, labels map[string]string) runtime.Object {
	t.Helper()
	tmp := &Store{Kube: fake.NewClientset(), Driver: drv}
	if err := tmp.driver(ns).Create("tmp", rel); err != nil {
		t.Fatal(err)
	}
	var obj runtime.Object
	var err error
	if drv == StorageConfigMap {
		obj, err = tmp.Kube.CoreV1().ConfigMaps(ns).Get(context.Background(), "tmp", metav1.GetOptions{})
	} else {
		obj, err = tmp.Kube.CoreV1().Secrets(ns).Get(context.Background(), "tmp", metav1.GetOptions{})
	}
	if err != nil {
		t.Fatal(err)
	}
	return withMeta(t, obj, key, labels, recordCreated)
}

// recordStore puts objs into both fakes: the metadata client serves the list, the typed client the GET.
func recordStore(t *testing.T, objs ...runtime.Object) *Store {
	t.Helper()
	var partials []runtime.Object
	for _, o := range objs {
		partials = append(partials, partialOf(t, o))
	}
	return &Store{Kube: fake.NewClientset(objs...), Metadata: metafake.NewSimpleMetadataClient(metaScheme(), partials...)}
}

func partialOf(t *testing.T, obj runtime.Object) *metav1.PartialObjectMetadata {
	t.Helper()
	m, err := meta.Accessor(obj)
	if err != nil {
		t.Fatal(err)
	}
	drv := StorageSecret
	if _, ok := obj.(*corev1.ConfigMap); ok {
		drv = StorageConfigMap
	}
	p := partial(drv, m.GetNamespace(), m.GetName(), m.GetLabels())
	p.CreationTimestamp = m.GetCreationTimestamp()
	return p
}

// dataSize is the expected Bytes of a stored object: Secret data; ConfigMap data and binaryData.
func dataSize(obj runtime.Object) int {
	n := 0
	switch o := obj.(type) {
	case *corev1.Secret:
		for _, v := range o.Data {
			n += len(v)
		}
	case *corev1.ConfigMap:
		for _, v := range o.Data {
			n += len(v)
		}
		for _, v := range o.BinaryData {
			n += len(v)
		}
	}
	return n
}

func recordObjects(recs []StoredRecord) []string {
	out := []string{}
	for _, r := range recs {
		out = append(out, r.Object)
	}
	return out
}

func helmLabels(rel, ver string) map[string]string {
	return map[string]string{"owner": "helm", "name": rel, "version": ver, "status": "deployed"}
}

// Records are found by object name, so a record Helm cannot see because of its labels is still returned.
func TestRecordsIgnoresLabels(t *testing.T) {
	t.Parallel()
	for _, drv := range drivers {
		for _, tc := range []struct {
			name   string
			labels map[string]string
		}{
			{"helm labels", helmLabels("web", "1")},
			{"wrong labels", map[string]string{"name": "other", "owner": "foo"}},
			{"no labels", nil},
		} {
			t.Run(drv+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				key := "sh.helm.release.v1.web.v1"
				obj := recordObject(t, drv, "prod", key, testRelease("web", "prod", 1), tc.labels)
				s := recordStore(t, obj)

				recs, err := s.Records(context.Background(), drv, "web", "prod")
				if err != nil {
					t.Fatal(err)
				}
				if len(recs) != 1 {
					t.Fatalf("Records = %+v, want 1 record", recs)
				}
				r := recs[0]
				if r.Driver != drv || r.Object != key || r.Revision != 1 {
					t.Errorf("record = %+v, want driver %s, object %s, revision 1", r, drv, key)
				}
				if len(tc.labels) == 0 {
					if len(r.Labels) != 0 {
						t.Errorf("Labels = %v, want none", r.Labels)
					}
				} else if !reflect.DeepEqual(r.Labels, tc.labels) {
					t.Errorf("Labels = %v, want %v as stored", r.Labels, tc.labels)
				}
				if r.DecodeErr != nil || r.Release == nil || r.Release.Name != "web" || r.Release.Version != 1 {
					t.Errorf("Release = %+v, DecodeErr = %v; want decoded web v1", r.Release, r.DecodeErr)
				}
			})
		}
	}
}

// Release names may contain dots and dashes, so only sh.helm.release.v1.<name>.v<digits> in the namespace matches.
func TestRecordsMatchesExactName(t *testing.T) {
	t.Parallel()
	for _, drv := range drivers {
		t.Run(drv, func(t *testing.T) {
			t.Parallel()
			rec := func(ns, key, rel string, ver int) runtime.Object {
				return recordObject(t, drv, ns, key, testRelease(rel, ns, ver), helmLabels(rel, "1"))
			}
			s := recordStore(t,
				rec("prod", "sh.helm.release.v1.web.v1", "web", 1),
				rec("prod", "sh.helm.release.v1.web.v9", "web", 9),
				rec("prod", "sh.helm.release.v1.web.v12", "web", 12),
				rec("prod", "sh.helm.release.v1.web-api.v1", "web-api", 1),
				rec("prod", "sh.helm.release.v1.web.v2.v1", "web.v2", 1),
				rec("prod", "sh.helm.release.v1.web.vx", "web", 1),
				rec("prod", "sh.helm.release.v1.web.v", "web", 1),
				rec("prod", "sh.helm.release.v1.web.v1x", "web", 1),
				rec("prod", "xsh.helm.release.v1.web.v1", "web", 1),
				rec("prod", "default-token", "web", 1),
				rec("staging", "sh.helm.release.v1.web.v3", "web", 3),
			)

			recs, err := s.Records(context.Background(), drv, "web", "prod")
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"sh.helm.release.v1.web.v1", "sh.helm.release.v1.web.v9", "sh.helm.release.v1.web.v12"}
			if got := recordObjects(recs); !reflect.DeepEqual(got, want) {
				t.Errorf("Records objects = %v, want %v", got, want)
			}

			// The dotted release "web.v2" is its own release, not revision 2 of web.
			recs, err = s.Records(context.Background(), drv, "web.v2", "prod")
			if err != nil {
				t.Fatal(err)
			}
			if got := recordObjects(recs); !reflect.DeepEqual(got, []string{"sh.helm.release.v1.web.v2.v1"}) {
				t.Errorf("Records(web.v2) objects = %v, want only its own record", got)
			}
		})
	}
}

// The key is what Helm collides on, so the revision comes from it even when the label or payload disagrees.
func TestRecordsRevisionFromKey(t *testing.T) {
	t.Parallel()
	for _, drv := range drivers {
		t.Run(drv, func(t *testing.T) {
			t.Parallel()
			obj := recordObject(t, drv, "prod", "sh.helm.release.v1.web.v5", testRelease("web", "prod", 4), helmLabels("web", "4"))
			recs, err := recordStore(t, obj).Records(context.Background(), drv, "web", "prod")
			if err != nil || len(recs) != 1 {
				t.Fatalf("Records = %+v, %v; want 1 record", recs, err)
			}
			if recs[0].Revision != 5 {
				t.Errorf("Revision = %d, want 5 from the key (label and payload say 4)", recs[0].Revision)
			}
			if recs[0].Release == nil || recs[0].Release.Version != 4 {
				t.Errorf("Release = %+v, want the payload as decoded (version 4)", recs[0].Release)
			}
		})
	}
}

func TestRecordsCorruptPayload(t *testing.T) {
	t.Parallel()
	for _, drv := range drivers {
		t.Run(drv, func(t *testing.T) {
			t.Parallel()
			labels := map[string]string{"owner": "helm", "name": "web", "version": "2", "status": "deployed"}
			bad := withMeta(t, corruptObject(drv, "prod", "web", "2"), "sh.helm.release.v1.web.v2", labels, recordCreated)
			good := recordObject(t, drv, "prod", "sh.helm.release.v1.web.v1", testRelease("web", "prod", 1), helmLabels("web", "1"))
			recs, err := recordStore(t, bad, good).Records(context.Background(), drv, "web", "prod")
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != 2 {
				t.Fatalf("Records = %+v, want the good and the corrupt record", recs)
			}
			if r := recs[0]; r.Release == nil || r.DecodeErr != nil {
				t.Errorf("good record = %+v, want decoded", r)
			}
			r := recs[1]
			if r.Object != "sh.helm.release.v1.web.v2" || r.Revision != 2 {
				t.Errorf("corrupt record = %+v, want web.v2", r)
			}
			if r.Release != nil || r.DecodeErr == nil {
				t.Errorf("corrupt record Release = %+v, DecodeErr = %v; want nil release and a decode error", r.Release, r.DecodeErr)
			}
			if !reflect.DeepEqual(r.Labels, labels) {
				t.Errorf("Labels = %v, want %v", r.Labels, labels)
			}
			if want := len("%%%not-base64%%%"); r.Bytes != want {
				t.Errorf("Bytes = %d, want %d", r.Bytes, want)
			}
		})
	}
}

func TestRecordsBytesAndCreated(t *testing.T) {
	t.Parallel()
	for _, drv := range drivers {
		t.Run(drv, func(t *testing.T) {
			t.Parallel()
			created := time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC)
			obj := recordObject(t, drv, "prod", "sh.helm.release.v1.web.v1", testRelease("web", "prod", 1), helmLabels("web", "1"))
			obj = withMeta(t, obj, "sh.helm.release.v1.web.v1", helmLabels("web", "1"), created)
			// Extra keys count towards the object size too.
			switch o := obj.(type) {
			case *corev1.Secret:
				o.Data["extra"] = []byte("abc")
			case *corev1.ConfigMap:
				o.Data["extra"] = "abc"
				o.BinaryData = map[string][]byte{"blob": []byte("12345")}
			}
			want := dataSize(obj)

			recs, err := recordStore(t, obj).Records(context.Background(), drv, "web", "prod")
			if err != nil || len(recs) != 1 {
				t.Fatalf("Records = %+v, %v; want 1 record", recs, err)
			}
			if recs[0].Bytes != want {
				t.Errorf("Bytes = %d, want %d", recs[0].Bytes, want)
			}
			if !recs[0].Created.Equal(created) {
				t.Errorf("Created = %v, want %v", recs[0].Created, created)
			}
		})
	}
}

// The drv argument picks the storage to read, whatever the store was configured with.
func TestRecordsDriverArgument(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		configured, drv, want string
	}{
		{StorageConfigMap, StorageSecret, "sh.helm.release.v1.web.v1"},
		{StorageSecret, StorageConfigMap, "sh.helm.release.v1.web.v2"},
		{"", StorageConfigMap, "sh.helm.release.v1.web.v2"},
	} {
		t.Run(tc.configured+"->"+tc.drv, func(t *testing.T) {
			t.Parallel()
			s := recordStore(t,
				recordObject(t, StorageSecret, "prod", "sh.helm.release.v1.web.v1", testRelease("web", "prod", 1), helmLabels("web", "1")),
				recordObject(t, StorageConfigMap, "prod", "sh.helm.release.v1.web.v2", testRelease("web", "prod", 2), helmLabels("web", "2")),
			)
			s.Driver = tc.configured
			recs, err := s.Records(context.Background(), tc.drv, "web", "prod")
			if err != nil {
				t.Fatal(err)
			}
			if got := recordObjects(recs); !reflect.DeepEqual(got, []string{tc.want}) {
				t.Fatalf("Records objects = %v, want [%s]", got, tc.want)
			}
			if recs[0].Driver != tc.drv || recs[0].Release == nil {
				t.Errorf("record = %+v, want driver %s and a decoded release", recs[0], tc.drv)
			}
		})
	}
}

func TestRecordsSorted(t *testing.T) {
	t.Parallel()
	for _, drv := range drivers {
		t.Run(drv, func(t *testing.T) {
			t.Parallel()
			var objs []runtime.Object
			// v02 and v2 share revision 2 and are ordered by object name.
			for _, k := range []string{"v10", "v2", "v1", "v02", "v3"} {
				objs = append(objs, recordObject(t, drv, "prod", "sh.helm.release.v1.web."+k, testRelease("web", "prod", 1), nil))
			}
			recs, err := recordStore(t, objs...).Records(context.Background(), drv, "web", "prod")
			if err != nil {
				t.Fatal(err)
			}
			var got []int
			for _, r := range recs {
				got = append(got, r.Revision)
			}
			if !reflect.DeepEqual(got, []int{1, 2, 2, 3, 10}) {
				t.Errorf("revisions = %v, want [1 2 2 3 10]", got)
			}
			want := []string{"sh.helm.release.v1.web.v1", "sh.helm.release.v1.web.v02", "sh.helm.release.v1.web.v2", "sh.helm.release.v1.web.v3", "sh.helm.release.v1.web.v10"}
			if objs := recordObjects(recs); !reflect.DeepEqual(objs, want) {
				t.Errorf("objects = %v, want %v", objs, want)
			}
		})
	}
}

func TestRecordsNone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		objs []runtime.Object
	}{
		{"empty storage", nil},
		{"unrelated objects", []runtime.Object{
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "default-token", Labels: map[string]string{"owner": "helm", "name": "web"}}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "sh.helm.release.v1.api.v1"}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "dev", Name: "sh.helm.release.v1.web.v1"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recs, err := recordStore(t, tc.objs...).Records(context.Background(), StorageSecret, "web", "prod")
			if err != nil || len(recs) != 0 {
				t.Errorf("Records = %+v, %v; want none and no error", recs, err)
			}
		})
	}
}

func TestRecordsListError(t *testing.T) {
	t.Parallel()
	for _, drv := range drivers {
		t.Run(drv, func(t *testing.T) {
			t.Parallel()
			boom := errors.New("boom")
			s := recordStore(t)
			mc := s.Metadata.(*metafake.FakeMetadataClient)
			mc.PrependReactor("list", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, boom
			})
			recs, err := s.Records(context.Background(), drv, "web", "prod")
			if !errors.Is(err, boom) || !strings.Contains(err.Error(), "listing helm") {
				t.Errorf("Records = %+v, %v; want a wrapped list error mentioning \"listing helm\"", recs, err)
			}
		})
	}
}

// Records with broken labels must be found, so the list is paged without a label selector.
func TestRecordsPaginationWithoutSelector(t *testing.T) {
	t.Parallel()
	for _, drv := range drivers {
		t.Run(drv, func(t *testing.T) {
			t.Parallel()
			v1 := recordObject(t, drv, "prod", "sh.helm.release.v1.web.v1", testRelease("web", "prod", 1), nil)
			v2 := recordObject(t, drv, "prod", "sh.helm.release.v1.web.v2", testRelease("web", "prod", 2), helmLabels("web", "2"))
			s := recordStore(t, v1, v2)
			mc := s.Metadata.(*metafake.FakeMetadataClient)
			var selectors []string
			mc.PrependReactor("list", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
				selectors = append(selectors, a.(clienttesting.ListAction).GetListRestrictions().Labels.String())
				list := &metav1.List{}
				if len(selectors) == 1 {
					list.Continue = "next"
					list.Items = []runtime.RawExtension{{Object: partialOf(t, v1)}}
				} else {
					list.Items = []runtime.RawExtension{{Object: partialOf(t, v2)}}
				}
				return true, list, nil
			})

			recs, err := s.Records(context.Background(), drv, "web", "prod")
			if err != nil {
				t.Fatal(err)
			}
			if got := recordObjects(recs); !reflect.DeepEqual(got, []string{"sh.helm.release.v1.web.v1", "sh.helm.release.v1.web.v2"}) {
				t.Errorf("Records objects = %v, want both pages", got)
			}
			if len(selectors) != 2 {
				t.Errorf("list calls = %d, want 2 (the continue token must be followed)", len(selectors))
			}
			for i, sel := range selectors {
				if sel != "" {
					t.Errorf("list call %d used label selector %q, want none", i+1, sel)
				}
			}
		})
	}
}

// An object deleted between the list and the GET is not an error: it is simply no longer there.
func TestRecordsSkipsVanishedObject(t *testing.T) {
	t.Parallel()
	for _, drv := range drivers {
		t.Run(drv, func(t *testing.T) {
			t.Parallel()
			gone := recordObject(t, drv, "prod", "sh.helm.release.v1.web.v1", testRelease("web", "prod", 1), helmLabels("web", "1"))
			kept := recordObject(t, drv, "prod", "sh.helm.release.v1.web.v2", testRelease("web", "prod", 2), helmLabels("web", "2"))
			s := recordStore(t, kept)
			s.Metadata = metafake.NewSimpleMetadataClient(metaScheme(), partialOf(t, gone), partialOf(t, kept))

			recs, err := s.Records(context.Background(), drv, "web", "prod")
			if err != nil {
				t.Fatal(err)
			}
			if got := recordObjects(recs); !reflect.DeepEqual(got, []string{"sh.helm.release.v1.web.v2"}) {
				t.Errorf("Records objects = %v, want only the record still present", got)
			}
		})
	}
}
