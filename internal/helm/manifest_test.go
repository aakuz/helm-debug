package helm

import (
	"reflect"
	"testing"

	rspb "helm.sh/helm/v4/pkg/release/v1"
)

func doc(apiVersion, kind, ns, name string) string {
	s := ""
	if apiVersion != "" {
		s += "apiVersion: " + apiVersion + "\n"
	}
	if kind != "" {
		s += "kind: " + kind + "\n"
	}
	s += "metadata:\n"
	if name != "" {
		s += "  name: " + name + "\n"
	}
	if ns != "" {
		s += "  namespace: " + ns + "\n"
	}
	return s
}

func TestObjects(t *testing.T) {
	cases := []struct {
		name string
		rel  *rspb.Release
		want []Object
	}{
		{"empty", &rspb.Release{}, nil},
		{
			"multi-doc sorted by kind, namespace, name; namespace as written",
			&rspb.Release{Manifest: "---\n# Source: a.yaml\n" + doc("v1", "Service", "", "web") +
				"---\n" + doc("apps/v1", "Deployment", "z", "api") +
				"---\n" + doc("apps/v1", "Deployment", "a", "zeta") +
				"---\n" + doc("apps/v1", "Deployment", "a", "alpha") +
				"---\n" + doc("v1", "ConfigMap", "", "cfg")},
			[]Object{
				{APIVersion: "v1", Kind: "ConfigMap", Name: "cfg"},
				{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "a", Name: "alpha"},
				{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "a", Name: "zeta"},
				{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "z", Name: "api"},
				{APIVersion: "v1", Kind: "Service", Name: "web"},
			},
		},
		{
			"documents missing apiVersion, kind or name or invalid are skipped",
			&rspb.Release{Manifest: "---\n" + doc("", "Service", "", "a") +
				"---\n" + doc("v1", "", "", "b") +
				"---\n" + doc("v1", "Service", "", "") +
				"---\nkind: [unclosed\n" +
				"---\n" + doc("v1", "Service", "", "ok")},
			[]Object{{APIVersion: "v1", Kind: "Service", Name: "ok"}},
		},
		{
			"hooks go after non-hooks; no events becomes hook; invalid hook skipped",
			&rspb.Release{
				Manifest: doc("v1", "Service", "", "web"),
				Hooks: []*rspb.Hook{
					{Manifest: doc("batch/v1", "Job", "", "migrate"), Events: []rspb.HookEvent{rspb.HookPreInstall, rspb.HookPreUpgrade}},
					{Manifest: doc("v1", "ConfigMap", "", "bare")},
					{Manifest: doc("v1", "Pod", "", ""), Events: []rspb.HookEvent{rspb.HookPostInstall}},
				},
			},
			[]Object{
				{APIVersion: "v1", Kind: "Service", Name: "web"},
				{APIVersion: "v1", Kind: "ConfigMap", Name: "bare", Hook: []string{"hook"}},
				{APIVersion: "batch/v1", Kind: "Job", Name: "migrate", Hook: []string{"pre-install", "pre-upgrade"}},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Objects(c.rel); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Objects = %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestHookEvents(t *testing.T) {
	for _, c := range []struct {
		hook []string
		want string
	}{
		{nil, ""},
		{[]string{"hook"}, "hook"},
		{[]string{"pre-install", "post-upgrade"}, "pre-install,post-upgrade"},
	} {
		if got := (Object{Hook: c.hook}).HookEvents(); got != c.want {
			t.Errorf("HookEvents(%v) = %q, want %q", c.hook, got, c.want)
		}
	}
}
