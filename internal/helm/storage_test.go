package helm

import (
	"reflect"
	"testing"
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
