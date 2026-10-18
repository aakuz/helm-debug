package kube

import (
	"encoding/base64"
	"reflect"
	"testing"
)

type obj = map[string]any

func deployment(spec obj) obj {
	return obj{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": obj{"name": "a"}, "spec": spec}
}

func podTemplate(containers ...any) obj {
	return obj{"template": obj{"spec": obj{"containers": containers}}}
}

func container(res obj) obj {
	return obj{"name": "c", "resources": res}
}

func TestChangedPaths(t *testing.T) {
	cm := func(data obj) obj {
		return obj{"apiVersion": "v1", "kind": "ConfigMap", "metadata": obj{"name": "a"}, "data": data}
	}
	tests := []struct {
		name     string
		expected obj
		actual   obj
		want     []string
	}{
		{"identical", deployment(obj{"replicas": 2}), deployment(obj{"replicas": 2}), nil},
		{"numeric yaml vs json types", deployment(obj{"replicas": int64(2)}), deployment(obj{"replicas": 2.0}), nil},
		{"changed scalar", deployment(obj{"replicas": 2}), deployment(obj{"replicas": 3}), []string{"/spec/replicas"}},
		{"removed field", deployment(obj{"replicas": 2, "paused": true}), deployment(obj{"replicas": 2}), []string{"/spec/paused"}},
		{"removed nested map", deployment(obj{"strategy": obj{"type": "Recreate"}}), deployment(obj{}), []string{"/spec/strategy"}},
		{"live-only field ignored", deployment(obj{"replicas": 2}), deployment(obj{"replicas": 2, "revisionHistoryLimit": 10}), nil},
		{"live-only metadata ignored", deployment(obj{}), obj{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": obj{"name": "a", "labels": obj{"x": "y"}}, "spec": obj{}}, nil},
		{"type mismatch map vs scalar", deployment(obj{"strategy": obj{"type": "x"}}), deployment(obj{"strategy": "x"}), []string{"/spec/strategy"}},
		{"type mismatch list vs scalar", deployment(obj{"x": []any{1}}), deployment(obj{"x": 1}), []string{"/spec/x"}},
		{"status ignored", obj{"kind": "Pod", "status": obj{"phase": "A"}}, obj{"kind": "Pod", "status": obj{"phase": "B"}}, nil},
		{"declared status dropped", obj{"kind": "Pod", "status": obj{"phase": "A"}}, obj{"kind": "Pod"}, nil},
		{
			"server metadata ignored",
			obj{"kind": "Pod", "metadata": obj{"name": "a", "resourceVersion": "1", "uid": "u", "generation": 1, "creationTimestamp": "t", "managedFields": []any{1}, "selfLink": "s"}},
			obj{"kind": "Pod", "metadata": obj{"name": "a", "resourceVersion": "2", "uid": "v", "generation": 5}},
			nil,
		},
		{
			"non-server metadata compared",
			obj{"kind": "Pod", "metadata": obj{"name": "a", "labels": obj{"k": "v"}}},
			obj{"kind": "Pod", "metadata": obj{"name": "a", "labels": obj{"k": "w"}}},
			[]string{"/metadata/labels/k"},
		},
		{"list longer live", deployment(obj{"x": []any{"a"}}), deployment(obj{"x": []any{"a", "b"}}), []string{"/spec/x"}},
		{"list shorter live", deployment(obj{"x": []any{"a", "b"}}), deployment(obj{"x": []any{"a"}}), []string{"/spec/x"}},
		{"list reordered", deployment(obj{"x": []any{"a", "b"}}), deployment(obj{"x": []any{"b", "a"}}), []string{"/spec/x/0", "/spec/x/1"}},
		{
			"list element field changed, live extras ignored",
			deployment(obj{"x": []any{obj{"n": "a", "v": 1}}}),
			deployment(obj{"x": []any{obj{"n": "a", "v": 2, "extra": true}}}),
			[]string{"/spec/x/0/v"},
		},
		{"paths sorted", deployment(obj{"z": 1, "a": 1, "m": 1}), deployment(obj{"z": 2, "a": 2, "m": 2}), []string{"/spec/a", "/spec/m", "/spec/z"}},
		{"key escaping ~ and /", obj{"kind": "Pod", "metadata": obj{"annotations": obj{"a/b": "1", "c~d": "1", "e~/f": "1"}}},
			obj{"kind": "Pod", "metadata": obj{"annotations": obj{"a/b": "2", "c~d": "2", "e~/f": "2"}}},
			[]string{"/metadata/annotations/a~1b", "/metadata/annotations/c~0d", "/metadata/annotations/e~0~1f"}},
		{"configmap data changed", cm(obj{"a": "1"}), cm(obj{"a": "2"}), []string{"/data/a"}},
		{"configmap added key", cm(obj{"a": "1"}), cm(obj{"a": "1", "b": "2"}), []string{"/data/b"}},
		{"configmap added key with escaping", cm(obj{}), cm(obj{"x/y": "1"}), []string{"/data/x~1y"}},
		{"configmap removed key", cm(obj{"a": "1", "b": "2"}), cm(obj{"a": "1"}), []string{"/data/b"}},
		{"configmap no declared data but live has data", obj{"apiVersion": "v1", "kind": "ConfigMap"}, cm(obj{"a": "1"}), []string{"/data/a"}},
		{
			"configmap binaryData added key",
			obj{"apiVersion": "v1", "kind": "ConfigMap", "binaryData": obj{"a": "AA=="}},
			obj{"apiVersion": "v1", "kind": "ConfigMap", "binaryData": obj{"a": "AA==", "b": "AQ=="}},
			[]string{"/binaryData/b"},
		},
		{
			"configmap other live-only field ignored",
			cm(obj{"a": "1"}),
			obj{"apiVersion": "v1", "kind": "ConfigMap", "metadata": obj{"name": "a"}, "immutable": false, "data": obj{"a": "1"}},
			nil,
		},
		{
			"non-v1 configmap lookalike data added is ignored",
			obj{"apiVersion": "example.com/v1", "kind": "ConfigMap", "data": obj{"a": "1"}},
			obj{"apiVersion": "example.com/v1", "kind": "ConfigMap", "data": obj{"a": "1", "b": "2"}},
			nil,
		},
		{
			"secret stringData equals live data",
			obj{"apiVersion": "v1", "kind": "Secret", "stringData": obj{"k": "hello"}},
			obj{"apiVersion": "v1", "kind": "Secret", "data": obj{"k": base64.StdEncoding.EncodeToString([]byte("hello"))}},
			nil,
		},
		{
			"secret stringData differs from live data",
			obj{"apiVersion": "v1", "kind": "Secret", "stringData": obj{"k": "hello"}},
			obj{"apiVersion": "v1", "kind": "Secret", "data": obj{"k": base64.StdEncoding.EncodeToString([]byte("bye"))}},
			[]string{"/data/k"},
		},
		{
			"secret added live key",
			obj{"apiVersion": "v1", "kind": "Secret", "stringData": obj{"k": "hello"}},
			obj{"apiVersion": "v1", "kind": "Secret", "data": obj{"k": "aGVsbG8=", "extra": "eA=="}},
			[]string{"/data/extra"},
		},
		{
			"secret live stringData folded too",
			obj{"apiVersion": "v1", "kind": "Secret", "data": obj{"k": "aGVsbG8="}},
			obj{"apiVersion": "v1", "kind": "Secret", "stringData": obj{"k": "hello"}},
			nil,
		},
		{
			"cpu 100m equals 0.1",
			deployment(podTemplate(container(obj{"requests": obj{"cpu": "100m"}}))),
			deployment(podTemplate(container(obj{"requests": obj{"cpu": 0.1}}))),
			nil,
		},
		{
			"memory 1Gi equals 1024Mi",
			deployment(podTemplate(container(obj{"limits": obj{"memory": "1Gi"}}))),
			deployment(podTemplate(container(obj{"limits": obj{"memory": "1024Mi"}}))),
			nil,
		},
		{
			"quantity really different",
			deployment(podTemplate(container(obj{"requests": obj{"cpu": "100m"}}))),
			deployment(podTemplate(container(obj{"requests": obj{"cpu": "200m"}}))),
			[]string{"/spec/template/spec/containers/0/resources/requests/cpu"},
		},
		{
			"invalid declared quantity falls back to exact compare",
			deployment(podTemplate(container(obj{"requests": obj{"cpu": "lots"}}))),
			deployment(podTemplate(container(obj{"requests": obj{"cpu": "100m"}}))),
			[]string{"/spec/template/spec/containers/0/resources/requests/cpu"},
		},
		{
			"invalid live quantity falls back to exact compare",
			deployment(podTemplate(container(obj{"requests": obj{"cpu": "100m"}}))),
			deployment(podTemplate(container(obj{"requests": obj{"cpu": "bogus"}}))),
			[]string{"/spec/template/spec/containers/0/resources/requests/cpu"},
		},
		{
			"identical invalid quantities are equal",
			deployment(podTemplate(container(obj{"requests": obj{"cpu": "lots"}}))),
			deployment(podTemplate(container(obj{"requests": obj{"cpu": "lots"}}))),
			nil,
		},
		{
			"quantity-like string outside quantity field is exact",
			deployment(obj{"template": obj{"spec": obj{"containers": []any{obj{"name": "c", "env": []any{obj{"name": "X", "value": "100m"}}}}}}}),
			deployment(obj{"template": obj{"spec": obj{"containers": []any{obj{"name": "c", "env": []any{obj{"name": "X", "value": "0.1"}}}}}}}),
			[]string{"/spec/template/spec/containers/0/env/0/value"},
		},
		{
			"pvc storage 1G equals 1000M",
			obj{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "spec": obj{"resources": obj{"requests": obj{"storage": "1G"}}}},
			obj{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "spec": obj{"resources": obj{"requests": obj{"storage": "1000M"}}}},
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ChangedPaths(tt.expected, tt.actual)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ChangedPaths = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChangedPathsDoesNotMutateInputs(t *testing.T) {
	expected := obj{"apiVersion": "v1", "kind": "Secret", "status": obj{"x": 1}, "stringData": obj{"k": "v"}, "metadata": obj{"uid": "u"}}
	actual := obj{"apiVersion": "v1", "kind": "Secret", "data": obj{"k": "dg=="}, "metadata": obj{"uid": "v"}}
	ChangedPaths(expected, actual)
	if _, ok := expected["stringData"]; !ok {
		t.Error("expected was mutated: stringData removed")
	}
	if _, ok := expected["status"]; !ok {
		t.Error("expected was mutated: status removed")
	}
	if expected["metadata"].(obj)["uid"] != "u" {
		t.Error("expected metadata was mutated")
	}
}

func TestNormalize(t *testing.T) {
	t.Run("drops status and server metadata", func(t *testing.T) {
		in := obj{
			"kind":   "Pod",
			"status": obj{"phase": "Running"},
			"metadata": obj{
				"name": "a", "labels": obj{"k": "v"},
				"managedFields": []any{}, "resourceVersion": "1", "uid": "u", "generation": 1,
				"creationTimestamp": "t", "deletionTimestamp": "t", "deletionGracePeriodSeconds": 1, "selfLink": "s",
			},
		}
		want := obj{"kind": "Pod", "metadata": obj{"name": "a", "labels": obj{"k": "v"}}}
		if got := Normalize(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("converts numbers to JSON types", func(t *testing.T) {
		got := Normalize(obj{"n": int32(3)})
		if _, ok := got["n"].(float64); !ok {
			t.Fatalf("n has type %T, want float64", got["n"])
		}
	})
	t.Run("secret stringData folded into data, stringData wins", func(t *testing.T) {
		in := obj{"apiVersion": "v1", "kind": "Secret", "data": obj{"a": "old", "keep": "Zg=="}, "stringData": obj{"a": "new", "n": 1}}
		got := Normalize(in)
		want := obj{"apiVersion": "v1", "kind": "Secret", "data": obj{"a": base64.StdEncoding.EncodeToString([]byte("new")), "keep": "Zg=="}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
	t.Run("secret without data gets empty data", func(t *testing.T) {
		got := Normalize(obj{"apiVersion": "v1", "kind": "Secret"})
		if !reflect.DeepEqual(got["data"], obj{}) {
			t.Fatalf("data = %v", got["data"])
		}
		if _, ok := got["binaryData"]; ok {
			t.Error("Secret must not get binaryData")
		}
	})
	t.Run("configmap gets empty data and binaryData", func(t *testing.T) {
		got := Normalize(obj{"apiVersion": "v1", "kind": "ConfigMap"})
		if !reflect.DeepEqual(got["data"], obj{}) || !reflect.DeepEqual(got["binaryData"], obj{}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("configmap stringData untouched", func(t *testing.T) {
		got := Normalize(obj{"apiVersion": "v1", "kind": "ConfigMap", "stringData": obj{"a": "b"}})
		if _, ok := got["stringData"]; !ok {
			t.Error("stringData should only be folded for Secrets")
		}
	})
	t.Run("other kinds untouched", func(t *testing.T) {
		in := obj{"apiVersion": "apps/v1", "kind": "Deployment"}
		if got := Normalize(in); !reflect.DeepEqual(got, in) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("does not mutate input", func(t *testing.T) {
		in := obj{"kind": "Pod", "status": obj{}, "metadata": obj{"uid": "u"}}
		Normalize(in)
		if _, ok := in["status"]; !ok || in["metadata"].(obj)["uid"] != "u" {
			t.Fatal("input mutated")
		}
	})
}

func TestProject(t *testing.T) {
	t.Run("live-only keys dropped", func(t *testing.T) {
		want, got := Project(deployment(obj{"replicas": 2}), deployment(obj{"replicas": 3, "paused": true}))
		if !reflect.DeepEqual(want["spec"], obj{"replicas": 2.0}) || !reflect.DeepEqual(got["spec"], obj{"replicas": 3.0}) {
			t.Fatalf("want=%v got=%v", want, got)
		}
	})
	t.Run("removed field absent from got", func(t *testing.T) {
		_, got := Project(deployment(obj{"replicas": 2, "paused": true}), deployment(obj{"replicas": 2}))
		if _, ok := got["spec"].(obj)["paused"]; ok {
			t.Fatalf("got = %v", got)
		}
	})
	t.Run("equal-length lists projected elementwise", func(t *testing.T) {
		_, got := Project(
			deployment(obj{"x": []any{obj{"a": 1}}}),
			deployment(obj{"x": []any{obj{"a": 2, "extra": 1}}}))
		if !reflect.DeepEqual(got["spec"].(obj)["x"], []any{obj{"a": 2.0}}) {
			t.Fatalf("got = %v", got)
		}
	})
	t.Run("different-length lists kept whole", func(t *testing.T) {
		_, got := Project(
			deployment(obj{"x": []any{obj{"a": 1}}}),
			deployment(obj{"x": []any{obj{"a": 1, "extra": 1}, obj{"b": 2}}}))
		if !reflect.DeepEqual(got["spec"].(obj)["x"], []any{obj{"a": 1.0, "extra": 1.0}, obj{"b": 2.0}}) {
			t.Fatalf("got = %v", got)
		}
	})
	t.Run("type mismatch keeps live value", func(t *testing.T) {
		_, got := Project(deployment(obj{"s": obj{"a": 1}}), deployment(obj{"s": "str"}))
		if got["spec"].(obj)["s"] != "str" {
			t.Fatalf("got = %v", got)
		}
	})
	t.Run("equal quantities take declared string", func(t *testing.T) {
		want, got := Project(
			deployment(podTemplate(container(obj{"requests": obj{"cpu": "100m"}}))),
			deployment(podTemplate(container(obj{"requests": obj{"cpu": 0.1}}))))
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("want=%v got=%v", want, got)
		}
	})
	t.Run("different quantities keep live", func(t *testing.T) {
		want, got := Project(
			deployment(podTemplate(container(obj{"requests": obj{"cpu": "100m"}}))),
			deployment(podTemplate(container(obj{"requests": obj{"cpu": "200m"}}))))
		if reflect.DeepEqual(want, got) {
			t.Fatal("want and got should differ")
		}
	})
	t.Run("configmap payload keeps added live keys", func(t *testing.T) {
		cm := func(d obj) obj { return obj{"apiVersion": "v1", "kind": "ConfigMap", "data": d, "immutable": false} }
		_, got := Project(cm(obj{"a": "1"}), cm(obj{"a": "1", "b": "2"}))
		if !reflect.DeepEqual(got["data"], obj{"a": "1", "b": "2"}) {
			t.Fatalf("got = %v", got)
		}
	})
	t.Run("secret stringData normalized in want", func(t *testing.T) {
		want, got := Project(
			obj{"apiVersion": "v1", "kind": "Secret", "stringData": obj{"k": "hello"}},
			obj{"apiVersion": "v1", "kind": "Secret", "data": obj{"k": "aGVsbG8="}, "type": "Opaque"})
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("want=%v got=%v", want, got)
		}
	})
	t.Run("status and server metadata dropped from both", func(t *testing.T) {
		want, got := Project(
			obj{"kind": "Pod", "status": obj{"a": 1}},
			obj{"kind": "Pod", "status": obj{"a": 2}, "metadata": obj{"uid": "u"}})
		if _, ok := want["status"]; ok {
			t.Error("want has status")
		}
		if _, ok := got["status"]; ok {
			t.Error("got has status")
		}
	})
	t.Run("consistent with ChangedPaths when in sync", func(t *testing.T) {
		e := deployment(obj{"replicas": 2})
		a := deployment(obj{"replicas": 2, "paused": false})
		want, got := Project(e, a)
		if len(ChangedPaths(e, a)) != 0 || !reflect.DeepEqual(want, got) {
			t.Fatalf("want=%v got=%v", want, got)
		}
	})
}

func TestPointerChild(t *testing.T) {
	tests := []struct{ path, key, want string }{
		{"", "a", "/a"},
		{"/a", "b", "/a/b"},
		{"", "a/b", "/a~1b"},
		{"", "a~b", "/a~0b"},
		{"", "~/", "/~0~1"},
		{"", "~1", "/~01"},
		{"/x", "", "/x/"},
	}
	for _, tt := range tests {
		if got := pointerChild(tt.path, tt.key); got != tt.want {
			t.Errorf("pointerChild(%q,%q) = %q, want %q", tt.path, tt.key, got, tt.want)
		}
	}
}

func TestQuantityField(t *testing.T) {
	tests := []struct {
		apiVersion, kind, path string
		want                   bool
	}{
		{"v1", "Pod", "/spec/containers/0/resources/requests/cpu", true},
		{"v1", "Pod", "/spec/initContainers/1/resources/limits/memory", true},
		{"v1", "Pod", "/spec/ephemeralContainers/0/resources/limits/memory", true},
		{"v1", "Pod", "/spec/resources/requests/cpu", true},
		{"v1", "Pod", "/spec/overhead/cpu", true},
		{"v1", "Pod", "/spec/containers/0/env/0/value", false},
		{"v1", "Pod", "/spec/containers/x/resources/requests/cpu", false},
		{"apps/v1", "Deployment", "/spec/template/spec/containers/0/resources/limits/cpu", true},
		{"apps/v1", "Deployment", "/spec/containers/0/resources/limits/cpu", false},
		{"apps/v1", "StatefulSet", "/spec/template/spec/containers/0/resources/limits/cpu", true},
		{"apps/v1", "DaemonSet", "/spec/template/spec/containers/0/resources/limits/cpu", true},
		{"batch/v1", "Job", "/spec/template/spec/containers/0/resources/limits/cpu", true},
		{"batch/v1", "CronJob", "/spec/jobTemplate/spec/template/spec/containers/0/resources/limits/cpu", true},
		{"batch/v1", "CronJob", "/spec/template/spec/containers/0/resources/limits/cpu", false},
		{"v1", "PersistentVolumeClaim", "/spec/resources/requests/storage", true},
		{"v1", "PersistentVolumeClaim", "/spec/resources/limits/storage", true},
		{"v1", "PersistentVolumeClaim", "/spec/storageClassName", false},
		{"v1", "PersistentVolume", "/spec/capacity/storage", true},
		{"v1", "ResourceQuota", "/spec/hard/cpu", true},
		{"v1", "LimitRange", "/spec/limits/0/max/cpu", true},
		{"v1", "LimitRange", "/spec/limits/0/defaultRequest/memory", true},
		{"v1", "LimitRange", "/spec/limits/0/type", false},
		{"v1", "ConfigMap", "/data/cpu", false},
		{"example.com/v1", "Pod", "/spec/containers/0/resources/requests/cpu", false},
	}
	for _, tt := range tests {
		got := quantityField(obj{"apiVersion": tt.apiVersion, "kind": tt.kind}, tt.path)
		if got != tt.want {
			t.Errorf("quantityField(%s %s, %s) = %v, want %v", tt.apiVersion, tt.kind, tt.path, got, tt.want)
		}
	}
}

func TestPayloadMap(t *testing.T) {
	tests := []struct {
		apiVersion, kind, path string
		want                   bool
	}{
		{"v1", "ConfigMap", "/data", true},
		{"v1", "ConfigMap", "/binaryData", true},
		{"v1", "Secret", "/data", true},
		{"v1", "Secret", "/binaryData", true},
		{"v1", "Secret", "/stringData", false},
		{"v1", "ConfigMap", "/data/x", false},
		{"v1", "Pod", "/data", false},
		{"x/v1", "ConfigMap", "/data", false},
	}
	for _, tt := range tests {
		if got := payloadMap(obj{"apiVersion": tt.apiVersion, "kind": tt.kind}, tt.path); got != tt.want {
			t.Errorf("payloadMap(%s %s %s) = %v, want %v", tt.apiVersion, tt.kind, tt.path, got, tt.want)
		}
	}
}
