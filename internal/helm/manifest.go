package helm

import (
	"sort"
	"strings"
	"time"

	rspb "helm.sh/helm/v4/pkg/release/v1"
	releaseutil "helm.sh/helm/v4/pkg/release/v1/util"
	"sigs.k8s.io/yaml"
)

// Object is a Kubernetes object declared by a release (in its manifest or as a hook).
type Object struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Namespace  string   `json:"namespace,omitempty"`
	Name       string   `json:"name"`
	Hook       []string `json:"hook,omitempty"`
}

type head struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

func parse(doc string) (head, bool) {
	var h head
	if err := yaml.Unmarshal([]byte(doc), &h); err != nil {
		return h, false
	}
	return h, h.APIVersion != "" && h.Kind != "" && h.Metadata.Name != ""
}

// Objects returns all objects declared by the release, hooks included, sorted by kind and name.
// The namespace is left as written in the manifest; callers default it for namespaced kinds.
func Objects(rel *rspb.Release) []Object {
	var objs []Object
	for _, doc := range releaseutil.SplitManifests(rel.Manifest) {
		if h, ok := parse(doc); ok {
			objs = append(objs, Object{APIVersion: h.APIVersion, Kind: h.Kind, Namespace: h.Metadata.Namespace, Name: h.Metadata.Name})
		}
	}
	for _, hk := range rel.Hooks {
		h, ok := parse(hk.Manifest)
		if !ok {
			continue
		}
		events := make([]string, 0, len(hk.Events))
		for _, e := range hk.Events {
			events = append(events, string(e))
		}
		if len(events) == 0 {
			events = []string{"hook"}
		}
		objs = append(objs, Object{APIVersion: h.APIVersion, Kind: h.Kind, Namespace: h.Metadata.Namespace, Name: h.Metadata.Name, Hook: events})
	}
	sort.SliceStable(objs, func(i, j int) bool {
		a, b := objs[i], objs[j]
		if (len(a.Hook) > 0) != (len(b.Hook) > 0) {
			return len(b.Hook) > 0
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return objs
}

// HookEvents formats hook events for display.
func (o Object) HookEvents() string { return strings.Join(o.Hook, ",") }

// Hook is a hook declared by a release revision, with the metadata Helm recorded about its last run.
type Hook struct {
	Object
	Weight         int
	DeletePolicies []string
	Phase          string
	LastRun        time.Time
}

// Hooks returns the hooks of the release sorted by weight, then kind and name (the order Helm runs them in).
func Hooks(rel *rspb.Release) []Hook {
	var hooks []Hook
	for _, hk := range rel.Hooks {
		h, ok := parse(hk.Manifest)
		if !ok {
			continue
		}
		obj := Object{APIVersion: h.APIVersion, Kind: h.Kind, Namespace: h.Metadata.Namespace, Name: h.Metadata.Name}
		for _, e := range hk.Events {
			obj.Hook = append(obj.Hook, string(e))
		}
		policies := make([]string, 0, len(hk.DeletePolicies))
		for _, p := range hk.DeletePolicies {
			policies = append(policies, string(p))
		}
		// Helm records the start of the run and, once it ends, the completion.
		last := hk.LastRun.CompletedAt
		if last.IsZero() {
			last = hk.LastRun.StartedAt
		}
		hooks = append(hooks, Hook{Object: obj, Weight: hk.Weight, DeletePolicies: policies, Phase: string(hk.LastRun.Phase), LastRun: last})
	}
	sort.SliceStable(hooks, func(i, j int) bool {
		a, b := hooks[i], hooks[j]
		if a.Weight != b.Weight {
			return a.Weight < b.Weight
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return hooks
}
