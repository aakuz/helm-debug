// Package helm reads Helm release records from the cluster storage (Secrets or ConfigMaps).
package helm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
)

// Storage drivers supported by the tool.
const (
	StorageSecret    = "secret"
	StorageConfigMap = "configmap"
)

// ErrReleaseNotFound is returned when a release has no records in the storage.
var ErrReleaseNotFound = errors.New("release not found")

const pageSize = 500

// Store reads release records using the Helm SDK storage drivers.
type Store struct {
	Kube     kubernetes.Interface
	Metadata metadata.Interface
	Driver   string
}

// NormalizeDriver maps HELM_DRIVER-style values to a supported driver name.
func NormalizeDriver(s string) (string, error) {
	switch strings.ToLower(s) {
	case "", "secret", "secrets":
		return StorageSecret, nil
	case "configmap", "configmaps":
		return StorageConfigMap, nil
	default:
		return "", fmt.Errorf("unsupported storage driver %q (supported: secret, configmap)", s)
	}
}

func (s *Store) driver(namespace string) driver.Driver {
	if s.Driver == StorageConfigMap {
		return driver.NewConfigMaps(s.Kube.CoreV1().ConfigMaps(namespace))
	}
	return driver.NewSecrets(s.Kube.CoreV1().Secrets(namespace))
}

func (s *Store) gvr() schema.GroupVersionResource {
	if s.Driver == StorageConfigMap {
		return schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	}
	return schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
}

// History returns all revisions of a release, newest first.
func (s *Store) History(name, namespace string) ([]*rspb.Release, error) {
	items, err := s.driver(namespace).Query(map[string]string{"name": name, "owner": "helm"})
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, fmt.Errorf("%w: %s/%s", ErrReleaseNotFound, namespace, name)
	}
	if err != nil {
		return nil, err
	}
	rels := make([]*rspb.Release, 0, len(items))
	for _, it := range items {
		if r, ok := it.(*rspb.Release); ok {
			rels = append(rels, r)
		}
	}
	if len(rels) == 0 {
		return nil, fmt.Errorf("%w: %s/%s (records could not be decoded)", ErrReleaseNotFound, namespace, name)
	}
	sort.Slice(rels, func(i, j int) bool { return rels[i].Version > rels[j].Version })
	return rels, nil
}

// Revision returns a specific revision of a release, or the latest one when revision is 0.
func (s *Store) Revision(name, namespace string, revision int) (*rspb.Release, error) {
	rels, err := s.History(name, namespace)
	if err != nil {
		return nil, err
	}
	if revision == 0 {
		return rels[0], nil
	}
	for _, r := range rels {
		if r.Version == revision {
			return r, nil
		}
	}
	return nil, fmt.Errorf("%w: %s/%s revision %d", ErrReleaseNotFound, namespace, name, revision)
}

// Ref points to a single release record in the storage.
type Ref struct {
	Namespace string
	Object    string
	Release   string
	Version   int
}

// ListRefs lists release records page by page, reading only object metadata.
// An empty namespace means all namespaces.
func (s *Store) ListRefs(ctx context.Context, namespace string) ([]Ref, error) {
	var refs []Ref
	opts := metav1.ListOptions{LabelSelector: "owner=helm", Limit: pageSize}
	for {
		list, err := s.Metadata.Resource(s.gvr()).Namespace(namespace).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing helm %ss: %w", s.Driver, err)
		}
		for _, it := range list.Items {
			refs = append(refs, Ref{
				Namespace: it.Namespace,
				Object:    it.Name,
				Release:   it.Labels["name"],
				Version:   recordVersion(it.Name, it.Labels["version"]),
			})
		}
		if list.Continue == "" {
			return refs, nil
		}
		opts.Continue = list.Continue
	}
}

func recordVersion(object, label string) int {
	if v, err := strconv.Atoi(label); err == nil {
		return v
	}
	if i := strings.LastIndex(object, ".v"); i >= 0 {
		if v, err := strconv.Atoi(object[i+2:]); err == nil {
			return v
		}
	}
	return 0
}

// LatestOnly keeps the highest revision of every release.
func LatestOnly(refs []Ref) []Ref {
	latest := map[string]Ref{}
	for _, r := range refs {
		key := r.Namespace + "/" + r.Release
		if cur, ok := latest[key]; !ok || r.Version > cur.Version {
			latest[key] = r
		}
	}
	out := make([]Ref, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Release < out[j].Release
	})
	return out
}

// Load fetches and decodes a single record.
func (s *Store) Load(ref Ref) (*rspb.Release, error) {
	item, err := s.driver(ref.Namespace).Get(ref.Object)
	if err != nil {
		return nil, err
	}
	rel, ok := item.(*rspb.Release)
	if !ok {
		return nil, fmt.Errorf("unexpected release type %T", item)
	}
	return rel, nil
}
