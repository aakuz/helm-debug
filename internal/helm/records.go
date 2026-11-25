package helm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StoredRecord is a storage object whose name is a record key of a release, read whatever its labels say.
type StoredRecord struct {
	Driver    string            // StorageSecret or StorageConfigMap
	Object    string            // object name, the record key sh.helm.release.v1.NAME.vN
	Revision  int               // N parsed from the key
	Labels    map[string]string // labels as stored
	Created   time.Time         // creationTimestamp
	Bytes     int               // Secret .data; ConfigMap .data + .binaryData
	Release   *rspb.Release     // nil when DecodeErr is set
	DecodeErr error
}

// Records returns every object of the given storage that is a record key of the release, sorted by revision.
// Objects are matched by name, not by labels, because records with broken labels are what this is meant to find.
func (s *Store) Records(ctx context.Context, drv, name, namespace string) ([]StoredRecord, error) {
	d := *s
	d.Driver = drv
	prefix := "sh.helm.release.v1." + name + ".v"

	var recs []StoredRecord
	opts := metav1.ListOptions{Limit: pageSize}
	for {
		list, err := d.Metadata.Resource(d.gvr()).Namespace(namespace).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing helm %ss: %w", drv, err)
		}
		for _, it := range list.Items {
			rev, ok := recordRevision(it.Name, prefix)
			if !ok {
				continue
			}
			rec, ok, err := d.record(ctx, namespace, it.Name, rev)
			if err != nil {
				return nil, err
			}
			if ok {
				recs = append(recs, rec)
			}
		}
		if list.Continue == "" {
			break
		}
		opts.Continue = list.Continue
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Revision != recs[j].Revision {
			return recs[i].Revision < recs[j].Revision
		}
		return recs[i].Object < recs[j].Object
	})
	return recs, nil
}

// recordRevision returns N when object is prefix followed by ASCII digits only.
func recordRevision(object, prefix string) (int, bool) {
	digits, ok := strings.CutPrefix(object, prefix)
	if !ok || digits == "" {
		return 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil
}

// record reads one object for its metadata and size, then decodes it. The Helm SDK's decoder is unexported
// and ADR 0002 forbids our own, so the object is fetched twice: once typed here, once by the driver.
// ok is false when the object vanished since the list.
func (s *Store) record(ctx context.Context, namespace, key string, rev int) (StoredRecord, bool, error) {
	rec := StoredRecord{Driver: s.Driver, Object: key, Revision: rev}
	var err error
	if s.Driver == StorageConfigMap {
		var cm *corev1.ConfigMap
		if cm, err = s.Kube.CoreV1().ConfigMaps(namespace).Get(ctx, key, metav1.GetOptions{}); err == nil {
			rec.Labels, rec.Created = cm.Labels, cm.CreationTimestamp.Time
			for _, v := range cm.Data {
				rec.Bytes += len(v)
			}
			for _, v := range cm.BinaryData {
				rec.Bytes += len(v)
			}
		}
	} else {
		var sec *corev1.Secret
		if sec, err = s.Kube.CoreV1().Secrets(namespace).Get(ctx, key, metav1.GetOptions{}); err == nil {
			rec.Labels, rec.Created = sec.Labels, sec.CreationTimestamp.Time
			for _, v := range sec.Data {
				rec.Bytes += len(v)
			}
		}
	}
	if apierrors.IsNotFound(err) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, fmt.Errorf("reading helm %s %s: %w", s.Driver, key, err)
	}

	item, err := s.driver(namespace).Get(key)
	switch {
	case errors.Is(err, driver.ErrReleaseNotFound):
		return rec, false, nil
	case err != nil:
		rec.DecodeErr = err
	default:
		rel, ok := item.(*rspb.Release)
		if !ok {
			rec.DecodeErr = fmt.Errorf("unexpected release type %T", item)
		} else {
			rec.Release = rel
		}
	}
	return rec, true, nil
}
