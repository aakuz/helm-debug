package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aakuz/helm-debug/internal/helm"
)

const nextRevisionProblem = "next revision: Helm writes this key on the next upgrade"

// ---- analyzeStorage ----

func storageKey(name string, rev int) string {
	return fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, rev)
}

// helmLabels are the labels Helm's driver writes for revision rev of release web.
func helmLabels(rev int, status string) map[string]string {
	return map[string]string{"name": "web", "owner": "helm", "version": strconv.Itoa(rev), "status": status}
}

// withLabels returns helmLabels(rev, status) edited by edit.
func withLabels(rev int, status string, edit func(map[string]string)) map[string]string {
	l := helmLabels(rev, status)
	edit(l)
	return l
}

func body(name string, version int, status common.Status) *rspb.Release {
	return &rspb.Release{Name: name, Namespace: "prod", Version: version, Info: &rspb.Info{Status: status}}
}

// stored is a decoded record of release web under key v<rev> of driver drv.
func stored(drv string, rev int, labels map[string]string, rel *rspb.Release) helm.StoredRecord {
	return helm.StoredRecord{Driver: drv, Object: storageKey("web", rev), Revision: rev, Labels: labels, Release: rel}
}

// good is a healthy record: labels and body agree with the key.
func good(drv string, rev int, status common.Status) helm.StoredRecord {
	return stored(drv, rev, helmLabels(rev, status.String()), body("web", rev, status))
}

type wantRecord struct {
	driver, object string
	visible        bool
	problems       []string
	fix            string
}

func TestAnalyzeStorage(t *testing.T) {
	const (
		sec = helm.StorageSecret
		cm  = helm.StorageConfigMap
	)
	sup, dep, pend := common.StatusSuperseded, common.StatusDeployed, common.StatusPendingUpgrade
	undecodable := helm.StoredRecord{
		Driver: sec, Object: storageKey("web", 1), Revision: 1, Labels: helmLabels(1, "superseded"),
		DecodeErr: errors.New("illegal base64\n data at\x1b[31m input byte 4"),
	}
	labelFix := func(kind string, rev int, status string) string {
		f := fmt.Sprintf("kubectl label %s %s -n prod --overwrite name=web owner=helm version=%d", kind, storageKey("web", rev), rev)
		if status != "" {
			f += " status=" + status
		}
		return f
	}
	deleteFix := func(kind string, rev int) string {
		return fmt.Sprintf("kubectl delete %s %s -n prod", kind, storageKey("web", rev))
	}
	ok := func(drv string, rev int) wantRecord {
		return wantRecord{driver: drv, object: storageKey("web", rev), visible: true}
	}

	tests := []struct {
		name       string
		selected   string
		sel, other []helm.StoredRecord
		latest     int
		next       string
		nextExists bool
		records    []wantRecord
	}{
		{
			name:     "healthy history",
			selected: sec,
			sel:      []helm.StoredRecord{good(sec, 1, sup), good(sec, 2, dep)},
			latest:   2, next: storageKey("web", 3),
			records: []wantRecord{ok(sec, 1), ok(sec, 2)},
		},
		{
			name:     "next revision without owner label causes already exists",
			selected: sec,
			sel: []helm.StoredRecord{
				good(sec, 1, sup), good(sec, 2, dep),
				stored(sec, 3, withLabels(3, "pending-upgrade", func(l map[string]string) { delete(l, "owner") }), body("web", 3, pend)),
			},
			latest: 2, next: storageKey("web", 3), nextExists: true,
			records: []wantRecord{ok(sec, 1), ok(sec, 2), {
				driver: sec, object: storageKey("web", 3),
				problems: []string{nextRevisionProblem, "missing label owner"},
				fix:      deleteFix("secret", 3),
			}},
		},
		{
			name:     "next revision with wrong name label",
			selected: sec,
			sel: []helm.StoredRecord{
				good(sec, 1, dep),
				stored(sec, 2, withLabels(2, "deployed", func(l map[string]string) { l["name"] = "other" }), body("web", 2, dep)),
			},
			latest: 1, next: storageKey("web", 2), nextExists: true,
			records: []wantRecord{ok(sec, 1), {
				driver: sec, object: storageKey("web", 2),
				problems: []string{nextRevisionProblem, "label name=other, want web"},
				fix:      deleteFix("secret", 2),
			}},
		},
		{
			name:     "wrong owner label hides a record",
			selected: sec,
			sel: []helm.StoredRecord{
				stored(sec, 1, withLabels(1, "superseded", func(l map[string]string) { l["owner"] = "tiller" }), body("web", 1, sup)),
				good(sec, 2, dep),
			},
			latest: 2, next: storageKey("web", 3),
			records: []wantRecord{{
				driver: sec, object: storageKey("web", 1),
				problems: []string{"label owner=tiller, want helm"},
				fix:      labelFix("secret", 1, "superseded"),
			}, ok(sec, 2)},
		},
		{
			name:     "key and version label disagree",
			selected: sec,
			sel: []helm.StoredRecord{
				good(sec, 1, sup),
				stored(sec, 2, withLabels(2, "deployed", func(l map[string]string) { l["version"] = "5" }), body("web", 2, dep)),
			},
			latest: 2, next: storageKey("web", 3),
			records: []wantRecord{ok(sec, 1), {
				driver: sec, object: storageKey("web", 2), visible: true,
				problems: []string{"label version=5, key says 2"},
				fix:      labelFix("secret", 2, "deployed"),
			}},
		},
		{
			name:     "status label disagrees with the record",
			selected: sec,
			sel: []helm.StoredRecord{
				stored(sec, 1, helmLabels(1, "deployed"), body("web", 1, sup)),
				good(sec, 2, dep),
			},
			latest: 2, next: storageKey("web", 3),
			records: []wantRecord{{
				driver: sec, object: storageKey("web", 1), visible: true,
				problems: []string{"label status=deployed, record says superseded"},
				fix:      labelFix("secret", 1, "superseded"),
			}, ok(sec, 2)},
		},
		{
			name:     "decoded release name differs from the key",
			selected: sec,
			sel:      []helm.StoredRecord{good(sec, 1, sup), stored(sec, 2, helmLabels(2, "deployed"), body("api", 2, dep))},
			latest:   2, next: storageKey("web", 3),
			records: []wantRecord{ok(sec, 1), {
				driver: sec, object: storageKey("web", 2), visible: true,
				problems: []string{"record says api revision 2"},
				// Relabelling cannot fix a body that disagrees with its key: the record must go.
				fix: deleteFix("secret", 2),
			}},
		},
		{
			// Helm takes the latest revision from the decoded bodies, so the body's revision 3 moves the next key to v4.
			name:     "decoded revision differs from the key",
			selected: sec,
			sel:      []helm.StoredRecord{good(sec, 1, sup), stored(sec, 2, helmLabels(2, "deployed"), body("web", 3, dep))},
			latest:   3, next: storageKey("web", 4),
			records: []wantRecord{ok(sec, 1), {
				driver: sec, object: storageKey("web", 2), visible: true,
				problems: []string{"record says web revision 3"},
				// Relabelling cannot fix a body that disagrees with its key: the record must go.
				fix: deleteFix("secret", 2),
			}},
		},
		{
			// A copy of revision 7 under a name Helm does not write still counts, as Helm lists records by labels.
			name:     "labelled copy under another name",
			selected: sec,
			sel: []helm.StoredRecord{
				good(sec, 1, sup), good(sec, 2, dep),
				{Driver: sec, Object: "web-backup", Revision: 0, Labels: helmLabels(7, "deployed"), Release: body("web", 7, dep)},
			},
			latest: 7, next: storageKey("web", 8),
			records: []wantRecord{{
				driver: sec, object: "web-backup", visible: true,
				problems: []string{"label version=7, key says 0", "record says web revision 7"},
				fix:      "kubectl delete secret web-backup -n prod",
			}, ok(sec, 1), ok(sec, 2)},
		},
		{
			// Relabelling would keep another release's record in web's history, so it is deleted.
			name:     "another release's key labelled as web",
			selected: cm,
			sel: []helm.StoredRecord{
				good(cm, 1, dep),
				{Driver: cm, Object: storageKey("other", 3), Revision: 0, Labels: helmLabels(3, "superseded"), Release: body("other", 3, sup)},
			},
			latest: 3, next: storageKey("web", 4),
			records: []wantRecord{{
				driver: cm, object: storageKey("other", 3), visible: true,
				problems: []string{"label version=3, key says 0", "record says other revision 3"},
				fix:      "kubectl delete configmap sh.helm.release.v1.other.v3 -n prod",
			}, ok(cm, 1)},
		},
		{
			name:     "duplicate version label",
			selected: sec,
			sel: []helm.StoredRecord{
				good(sec, 1, sup),
				stored(sec, 2, withLabels(2, "deployed", func(l map[string]string) { l["version"] = "1" }), body("web", 2, dep)),
			},
			latest: 2, next: storageKey("web", 3),
			records: []wantRecord{{
				driver: sec, object: storageKey("web", 1), visible: true,
				problems: []string{"duplicate version label 1 (also sh.helm.release.v1.web.v2)"},
				fix:      labelFix("secret", 1, "superseded"),
			}, {
				driver: sec, object: storageKey("web", 2), visible: true,
				problems: []string{"label version=1, key says 2", "duplicate version label 1 (also sh.helm.release.v1.web.v1)"},
				fix:      labelFix("secret", 2, "deployed"),
			}},
		},
		{
			name:     "undecodable record",
			selected: sec,
			sel:      []helm.StoredRecord{undecodable, good(sec, 2, dep)},
			latest:   2, next: storageKey("web", 3),
			records: []wantRecord{{
				driver: sec, object: storageKey("web", 1),
				problems: []string{"undecodable: illegal base64 data at input byte 4"},
				fix:      deleteFix("secret", 1),
			}, ok(sec, 2)},
		},
		{
			name:     "undecodable next revision",
			selected: sec,
			sel: []helm.StoredRecord{good(sec, 1, dep), {
				Driver: sec, Object: storageKey("web", 2), Revision: 2, Labels: helmLabels(2, "pending-upgrade"),
				DecodeErr: errors.New("gzip: invalid header"),
			}},
			latest: 1, next: storageKey("web", 2), nextExists: true,
			records: []wantRecord{ok(sec, 1), {
				driver: sec, object: storageKey("web", 2),
				problems: []string{"undecodable: gzip: invalid header", nextRevisionProblem},
				fix:      deleteFix("secret", 2),
			}},
		},
		{
			name:     "missing labels in order, without status",
			selected: sec,
			sel:      []helm.StoredRecord{stored(sec, 1, nil, body("web", 1, sup)), good(sec, 2, dep)},
			latest:   2, next: storageKey("web", 3),
			records: []wantRecord{{
				driver: sec, object: storageKey("web", 1),
				problems: []string{"missing label name", "missing label owner", "missing label version", "missing label status"},
				fix:      labelFix("secret", 1, "superseded"),
			}, ok(sec, 2)},
		},
		{
			name:     "missing status label keeps the record visible",
			selected: sec,
			sel: []helm.StoredRecord{
				stored(sec, 1, withLabels(1, "", func(l map[string]string) { delete(l, "status") }), body("web", 1, dep)),
			},
			latest: 1, next: storageKey("web", 2),
			records: []wantRecord{{
				driver: sec, object: storageKey("web", 1), visible: true,
				problems: []string{"missing label status"},
				fix:      labelFix("secret", 1, "deployed"),
			}},
		},
		{
			name:     "records in the other driver",
			selected: sec,
			sel:      []helm.StoredRecord{good(sec, 1, dep)},
			// configmap v2 has the next key's name, but only the selected driver's keys collide.
			other:  []helm.StoredRecord{good(cm, 1, sup), good(cm, 2, dep)},
			latest: 1, next: storageKey("web", 2),
			records: []wantRecord{ok(sec, 1), {
				driver: cm, object: storageKey("web", 1),
				problems: []string{"stored by the configmap driver, Helm uses secret"},
			}, {
				driver: cm, object: storageKey("web", 2),
				problems: []string{"stored by the configmap driver, Helm uses secret"},
			}},
		},
		{
			name:     "other driver records get label checks too",
			selected: cm,
			sel:      []helm.StoredRecord{good(cm, 1, dep)},
			// The secret v1 shares version label 1 with configmap v1: duplicates count within one driver only.
			other:  []helm.StoredRecord{stored(sec, 1, withLabels(1, "deployed", func(l map[string]string) { delete(l, "owner") }), body("web", 1, dep))},
			latest: 1, next: storageKey("web", 2),
			records: []wantRecord{ok(cm, 1), {
				driver: sec, object: storageKey("web", 1),
				problems: []string{"missing label owner", "stored by the secret driver, Helm uses configmap"},
				fix:      labelFix("secret", 1, "deployed"),
			}},
		},
		{
			name:     "nothing visible: next key is v1 and it exists",
			selected: sec,
			sel: []helm.StoredRecord{
				stored(sec, 1, withLabels(1, "pending-install", func(l map[string]string) { delete(l, "owner") }), body("web", 1, common.StatusPendingInstall)),
			},
			latest: 0, next: storageKey("web", 1), nextExists: true,
			records: []wantRecord{{
				driver: sec, object: storageKey("web", 1),
				problems: []string{nextRevisionProblem, "missing label owner"},
				fix:      deleteFix("secret", 1),
			}},
		},
		{
			name:     "only the other driver has records",
			selected: sec,
			other:    []helm.StoredRecord{good(cm, 1, dep)},
			latest:   0, next: storageKey("web", 1),
			records: []wantRecord{{
				driver: cm, object: storageKey("web", 1),
				problems: []string{"stored by the configmap driver, Helm uses secret"},
			}},
		},
		{
			name:     "records sorted by revision, then object",
			selected: sec,
			sel:      []helm.StoredRecord{good(sec, 2, dep), good(sec, 1, sup)},
			other:    []helm.StoredRecord{good(cm, 3, dep), good(cm, 1, sup)},
			latest:   2, next: storageKey("web", 3),
			records: []wantRecord{ok(sec, 1), ok(sec, 2),
				{driver: cm, object: storageKey("web", 1), problems: []string{"stored by the configmap driver, Helm uses secret"}},
				{driver: cm, object: storageKey("web", 3), problems: []string{"stored by the configmap driver, Helm uses secret"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := analyzeStorage("web", "prod", tt.selected, tt.sel, tt.other)
			if res.Release != "web" || res.Namespace != "prod" || res.Driver != tt.selected {
				t.Errorf("release/namespace/driver = %q/%q/%q, want web/prod/%s", res.Release, res.Namespace, res.Driver, tt.selected)
			}
			if res.LatestRevision != tt.latest || res.NextObject != tt.next || res.NextExists != tt.nextExists {
				t.Errorf("latest=%d next=%q exists=%v, want %d %q %v", res.LatestRevision, res.NextObject, res.NextExists, tt.latest, tt.next, tt.nextExists)
			}
			var got []wantRecord
			for _, r := range res.Records {
				w := wantRecord{driver: r.Driver, object: r.Object, visible: r.Visible, problems: r.Problems, fix: r.Fix}
				if len(w.problems) == 0 {
					w.problems = nil // no problems: nil and empty are the same
				}
				got = append(got, w)
			}
			if !reflect.DeepEqual(got, tt.records) {
				t.Errorf("records:\n got %+v\nwant %+v", got, tt.records)
			}
		})
	}
}

// TestAnalyzeStorageCopiesRecordFacts checks the fields copied from a StoredRecord as they are.
func TestAnalyzeStorageCopiesRecordFacts(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 10, 18, 2, 0, 0, 0, time.UTC)
	rec := good(helm.StorageSecret, 1, common.StatusDeployed)
	rec.Created, rec.Bytes = created, 1234
	res := analyzeStorage("web", "prod", helm.StorageSecret, []helm.StoredRecord{rec}, nil)
	if len(res.Records) != 1 {
		t.Fatalf("want 1 record, got %+v", res.Records)
	}
	r := res.Records[0]
	if r.Revision != 1 || r.Status != "deployed" || !r.Created.Equal(created) || r.Bytes != 1234 || !reflect.DeepEqual(r.Labels, rec.Labels) {
		t.Errorf("got %+v", r)
	}
	if len(r.Problems) != 0 || r.Fix != "" {
		t.Errorf("healthy record has problems %q, fix %q", r.Problems, r.Fix)
	}
}

// ---- the storage command against the fake cluster ----

// storeRecord writes rel under key with the Helm driver drv in namespace prod, then gives it a fixed creation time,
// a fixed createdAt label (Helm writes the wall clock there) and lets edit change its labels.
func storeRecord(c *cluster, drv, key string, rel *rspb.Release, edit func(map[string]string)) {
	c.t.Helper()
	ctx := context.Background()
	created := now.Add(-time.Duration(10-rel.Version) * time.Hour)
	fixLabels := func(l map[string]string) {
		l["createdAt"] = strconv.FormatInt(created.Unix(), 10)
		if edit != nil {
			edit(l)
		}
	}
	if drv == helm.StorageConfigMap {
		cms := c.kube.CoreV1().ConfigMaps("prod")
		if err := driver.NewConfigMaps(cms).Create(key, rel); err != nil {
			c.t.Fatal(err)
		}
		cm, err := cms.Get(ctx, key, metav1.GetOptions{})
		if err != nil {
			c.t.Fatal(err)
		}
		fixLabels(cm.Labels)
		cm.CreationTimestamp = metav1.NewTime(created)
		if _, err := cms.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			c.t.Fatal(err)
		}
		return
	}
	secrets := c.kube.CoreV1().Secrets("prod")
	if err := driver.NewSecrets(secrets).Create(key, rel); err != nil {
		c.t.Fatal(err)
	}
	s, err := secrets.Get(ctx, key, metav1.GetOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
	fixLabels(s.Labels)
	s.CreationTimestamp = metav1.NewTime(created)
	if _, err := secrets.Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		c.t.Fatal(err)
	}
}

// webRevision stores revision rev of release web in drv; edit may break its labels.
func webRevision(c *cluster, drv string, rev int, status common.Status, edit func(map[string]string)) {
	c.t.Helper()
	storeRecord(c, drv, storageKey("web", rev), body("web", rev, status), edit)
}

// healthyWeb has revisions 1 (superseded) and 2 (deployed) of web as Secrets.
func healthyWeb(t *testing.T) *cluster {
	c := newCluster(t)
	webRevision(c, helm.StorageSecret, 1, common.StatusSuperseded, nil)
	webRevision(c, helm.StorageSecret, 2, common.StatusDeployed, nil)
	return c
}

// alreadyExistsWeb is the case of issue #45: Helm sees revisions 1-4, while the v5 key it writes next is taken
// by a record without the owner label.
func alreadyExistsWeb(t *testing.T) *cluster {
	c := newCluster(t)
	for rev := 1; rev <= 3; rev++ {
		webRevision(c, helm.StorageSecret, rev, common.StatusSuperseded, nil)
	}
	webRevision(c, helm.StorageSecret, 4, common.StatusDeployed, nil)
	webRevision(c, helm.StorageSecret, 5, common.StatusPendingUpgrade, func(l map[string]string) { delete(l, "owner") })
	return c
}

// runStorage runs `storage ARGS` in namespace prod with colors off. The store driver follows o.Storage
// (from --storage or HELM_DRIVER), as with the real client factory.
func runStorage(t *testing.T, c *cluster, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	off := false
	o := NewOptions()
	o.Out, o.ErrOut, o.Now, o.Color = &out, &bytes.Buffer{}, func() time.Time { return now }, &off
	o.NewClients = func(o *Options) (*Clients, error) {
		drv, err := helm.NormalizeDriver(o.Storage)
		if err != nil {
			return nil, err
		}
		cl := c.clients("prod")
		cl.Store.Driver = drv
		return cl, nil
	}
	cmd := NewRootCommand(o)
	cmd.SetArgs(append([]string{"storage"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

type storageJSON struct {
	Release        string `json:"release"`
	Namespace      string `json:"namespace"`
	Driver         string `json:"driver"`
	DriverSource   string `json:"driverSource"`
	LatestRevision int    `json:"latestRevision"`
	NextObject     string `json:"nextObject"`
	NextExists     bool   `json:"nextExists"`
	Records        []struct {
		Driver   string            `json:"driver"`
		Object   string            `json:"object"`
		Revision int               `json:"revision"`
		Status   string            `json:"status"`
		Visible  bool              `json:"visible"`
		Created  time.Time         `json:"created"`
		Bytes    int               `json:"bytes"`
		Labels   map[string]string `json:"labels"`
		Problems []string          `json:"problems"`
		Fix      string            `json:"fix"`
	} `json:"records"`
}

func storageJSONOf(t *testing.T, c *cluster, args ...string) (storageJSON, error) {
	t.Helper()
	out, err := runStorage(t, c, append([]string{"-o", "json"}, args...)...)
	var res storageJSON
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("bad json (err=%v): %v\n%s", err, jerr, out)
	}
	return res, err
}

// TestStorageTable is not parallel: HELM_DRIVER is cleared so that the driver source reads "default".
func TestStorageTable(t *testing.T) {
	t.Setenv("HELM_DRIVER", "")
	out, err := runStorage(t, alreadyExistsWeb(t), "web")
	wantProblems(t, err, true)
	for _, want := range []string{
		"Release web in namespace prod, storage driver secret (default)\n",
		"Helm sees revisions up to 4; the next upgrade writes sh.helm.release.v1.web.v5, which already exists: this record causes \"release: already exists\".\n",
		"Fixes (not run):\n  kubectl delete secret sh.helm.release.v1.web.v5 -n prod\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	created5 := now.Add(-5 * time.Hour)
	labels := regexp.MustCompile(`(?m)^  labels: createdAt=` + strconv.FormatInt(created5.Unix(), 10) +
		`, name=web, status=pending-upgrade, version=5  created: 2026-10-18 07:00:00  size: \d+ bytes$`)
	if !labels.MatchString(out) {
		t.Errorf("output lacks the labels line of v5:\n%s", out)
	}
	if strings.Contains(out, "No records Helm cannot see") {
		t.Errorf("clean message printed with problems:\n%s", out)
	}
	golden(t, out)
}

func TestStorageHealthy(t *testing.T) {
	t.Parallel()
	out, err := runStorage(t, healthyWeb(t), "web")
	wantProblems(t, err, false)
	for _, want := range []string{
		"Helm sees revisions up to 2; the next upgrade writes sh.helm.release.v1.web.v3.\n",
		"\nNo records Helm cannot see.",
		"stuck",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Fixes (not run)") || strings.Contains(out, "already exists") {
		t.Errorf("healthy release reported problems:\n%s", out)
	}
}

func TestStorageNoVisibleRevision(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	webRevision(c, helm.StorageSecret, 1, common.StatusPendingInstall, func(l map[string]string) { l["name"] = "other" })
	out, err := runStorage(t, c, "web")
	wantProblems(t, err, true)
	if want := "Helm sees no revision of web; the next install writes sh.helm.release.v1.web.v1."; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

func TestStorageJSON(t *testing.T) {
	t.Parallel()
	res, err := storageJSONOf(t, alreadyExistsWeb(t), "web")
	wantProblems(t, err, true)
	if res.Release != "web" || res.Namespace != "prod" || res.Driver != "secret" {
		t.Errorf("release/namespace/driver = %q/%q/%q", res.Release, res.Namespace, res.Driver)
	}
	if res.LatestRevision != 4 || res.NextObject != "sh.helm.release.v1.web.v5" || !res.NextExists {
		t.Errorf("latest=%d next=%q exists=%v", res.LatestRevision, res.NextObject, res.NextExists)
	}
	if len(res.Records) != 5 {
		t.Fatalf("want 5 records, got %+v", res.Records)
	}
	for i, r := range res.Records[:4] {
		if r.Revision != i+1 || !r.Visible || r.Problems != nil || r.Fix != "" || r.Driver != "secret" {
			t.Errorf("record %d: %+v", i, r)
		}
	}
	v5 := res.Records[4]
	if v5.Object != "sh.helm.release.v1.web.v5" || v5.Visible || v5.Status != "pending-upgrade" || v5.Bytes == 0 {
		t.Errorf("v5: %+v", v5)
	}
	if !v5.Created.Equal(now.Add(-5 * time.Hour)) {
		t.Errorf("v5 created %v", v5.Created)
	}
	if want := []string{nextRevisionProblem, "missing label owner"}; !reflect.DeepEqual(v5.Problems, want) {
		t.Errorf("v5 problems %q, want %q", v5.Problems, want)
	}
	if want := "kubectl delete secret sh.helm.release.v1.web.v5 -n prod"; v5.Fix != want {
		t.Errorf("v5 fix %q, want %q", v5.Fix, want)
	}
	if v5.Labels["name"] != "web" || v5.Labels["version"] != "5" {
		t.Errorf("v5 labels %v", v5.Labels)
	}
}

func TestStorageUndecodable(t *testing.T) {
	t.Parallel()
	c := healthyWeb(t)
	bad := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: storageKey("web", 10), Namespace: "prod", Labels: map[string]string{"name": "web", "owner": "helm", "version": "10", "status": "deployed"}},
		Type:       "helm.sh/release.v1",
		Data:       map[string][]byte{"release": []byte("not base64 !!")},
	}
	if _, err := c.kube.CoreV1().Secrets("prod").Create(context.Background(), bad, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := storageJSONOf(t, c, "web")
	wantProblems(t, err, true)
	if len(res.Records) != 3 {
		t.Fatalf("want 3 records, got %+v", res.Records)
	}
	r := res.Records[2]
	if r.Object != "sh.helm.release.v1.web.v10" || r.Revision != 10 || r.Visible {
		t.Errorf("undecodable record: %+v", r)
	}
	if len(r.Problems) == 0 || !strings.HasPrefix(r.Problems[0], "undecodable: ") {
		t.Errorf("problems %q", r.Problems)
	}
	if want := "kubectl delete secret sh.helm.release.v1.web.v10 -n prod"; r.Fix != want {
		t.Errorf("fix %q, want %q", r.Fix, want)
	}
	if res.LatestRevision != 2 || res.NextExists {
		t.Errorf("latest=%d exists=%v", res.LatestRevision, res.NextExists)
	}
}

func TestStorageNotFound(t *testing.T) {
	t.Parallel()
	c := healthyWeb(t)
	for _, args := range [][]string{{"nope"}, {"nope", "--storage", "configmap"}} {
		if _, err := runStorage(t, c, args...); !errors.Is(err, helm.ErrReleaseNotFound) {
			t.Errorf("%v: want ErrReleaseNotFound, got %v", args, err)
		}
	}
	if _, err := runStorage(t, newCluster(t), "web"); !errors.Is(err, helm.ErrReleaseNotFound) {
		t.Errorf("empty cluster: want ErrReleaseNotFound, got %v", err)
	}
}

// TestStorageOtherDriverOnly: records stored only as ConfigMaps are a finding under the secret driver, not "not found".
func TestStorageOtherDriverOnly(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	webRevision(c, helm.StorageConfigMap, 1, common.StatusDeployed, nil)
	res, err := storageJSONOf(t, c, "web", "--storage", "secret")
	wantProblems(t, err, true)
	if res.Driver != "secret" || res.LatestRevision != 0 || res.NextObject != "sh.helm.release.v1.web.v1" || res.NextExists {
		t.Errorf("driver=%q latest=%d next=%q exists=%v", res.Driver, res.LatestRevision, res.NextObject, res.NextExists)
	}
	if len(res.Records) != 1 || res.Records[0].Driver != "configmap" ||
		!reflect.DeepEqual(res.Records[0].Problems, []string{"stored by the configmap driver, Helm uses secret"}) || res.Records[0].Fix != "" {
		t.Errorf("records %+v", res.Records)
	}
}

// TestStorageConfigMapDriver: with --storage configmap the ConfigMap records are Helm's and the Secrets the other driver.
func TestStorageConfigMapDriver(t *testing.T) {
	t.Parallel()
	c := healthyWeb(t)
	webRevision(c, helm.StorageConfigMap, 1, common.StatusSuperseded, nil)
	webRevision(c, helm.StorageConfigMap, 2, common.StatusSuperseded, nil)
	webRevision(c, helm.StorageConfigMap, 3, common.StatusDeployed, nil)
	res, err := storageJSONOf(t, c, "web", "--storage", "configmap")
	wantProblems(t, err, true)
	if res.Driver != "configmap" || res.DriverSource != "--storage" || res.LatestRevision != 3 || res.NextObject != "sh.helm.release.v1.web.v4" || res.NextExists {
		t.Errorf("driver=%q source=%q latest=%d next=%q exists=%v", res.Driver, res.DriverSource, res.LatestRevision, res.NextObject, res.NextExists)
	}
	var got []string
	for _, r := range res.Records {
		got = append(got, fmt.Sprintf("%s/v%d visible=%v %q fix=%q", r.Driver, r.Revision, r.Visible, r.Problems, r.Fix))
	}
	other := `["stored by the secret driver, Helm uses configmap"] fix=""`
	want := []string{
		`configmap/v1 visible=true [] fix=""`,
		`configmap/v2 visible=true [] fix=""`,
		`configmap/v3 visible=true [] fix=""`,
		`secret/v1 visible=false ` + other,
		`secret/v2 visible=false ` + other,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records:\n got %q\nwant %q", got, want)
	}
}

// TestStorageDriverSource is not parallel: it sets HELM_DRIVER.
func TestStorageDriverSource(t *testing.T) {
	tests := []struct {
		name, env  string
		args       []string
		wantDriver string
		wantSource string
	}{
		{"default", "", nil, "secret", "default"},
		{"HELM_DRIVER", "configmap", nil, "configmap", "HELM_DRIVER"},
		{"HELM_DRIVER secret", "secret", nil, "secret", "HELM_DRIVER"},
		{"--storage", "", []string{"--storage", "secret"}, "secret", "--storage"},
		{"--storage wins over HELM_DRIVER", "configmap", []string{"--storage", "secret"}, "secret", "--storage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HELM_DRIVER", tt.env)
			c := healthyWeb(t)
			webRevision(c, helm.StorageConfigMap, 1, common.StatusDeployed, nil)
			res, _ := storageJSONOf(t, c, append([]string{"web"}, tt.args...)...)
			if res.Driver != tt.wantDriver || res.DriverSource != tt.wantSource {
				t.Errorf("driver=%q source=%q, want %q %q", res.Driver, res.DriverSource, tt.wantDriver, tt.wantSource)
			}
		})
	}
}

// TestStorageTableDriverSource checks the source in the table header.
func TestStorageTableDriverSource(t *testing.T) {
	t.Setenv("HELM_DRIVER", "secret")
	out, err := runStorage(t, healthyWeb(t), "web")
	wantProblems(t, err, false)
	if want := "Release web in namespace prod, storage driver secret (HELM_DRIVER)\n"; !strings.HasPrefix(out, want) {
		t.Errorf("output does not start with %q:\n%s", want, out)
	}
}

func TestStorageSanitizesClusterText(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	webRevision(c, helm.StorageSecret, 1, common.StatusDeployed, nil)
	webRevision(c, helm.StorageSecret, 2, common.StatusPendingUpgrade, func(l map[string]string) {
		delete(l, "owner")
		l["team"] = "\x1b[31mred\x1b[0m"
		l["status"] = "\x1b]0;pwned\x07deployed"
	})
	out, err := runStorage(t, c, "web")
	wantProblems(t, err, true)
	if strings.ContainsRune(out, '\x1b') || strings.ContainsRune(out, '\x07') {
		t.Errorf("raw escape sequence printed:\n%q", out)
	}
	for _, want := range []string{"team=red", "label status=deployed, record says pending-upgrade"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestStorageArgs(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	for _, tc := range []struct {
		args []string
		want string
	}{{nil, "accepts 1 arg(s), received 0"}, {[]string{"a", "b"}, "accepts 1 arg(s), received 2"}} {
		_, err := runStorage(t, c, tc.args...)
		wantExecError(t, err, tc.want)
	}
}

// TestStorageLabelledCopy: a copy of a record under another name, labelled name=web,owner=helm, is listed by Helm
// and sets its latest revision; the command finds it and says to delete it.
func TestStorageLabelledCopy(t *testing.T) {
	t.Parallel()
	c := healthyWeb(t)
	storeRecord(c, helm.StorageSecret, "web-backup", body("web", 7, common.StatusDeployed), func(l map[string]string) {
		for k := range l {
			if k != "name" && k != "owner" {
				delete(l, k)
			}
		}
	})
	res, err := storageJSONOf(t, c, "web")
	wantProblems(t, err, true)
	if res.LatestRevision != 7 || res.NextObject != "sh.helm.release.v1.web.v8" || res.NextExists {
		t.Errorf("latest=%d next=%q exists=%v, want 7 sh.helm.release.v1.web.v8 false", res.LatestRevision, res.NextObject, res.NextExists)
	}
	if len(res.Records) != 3 {
		t.Fatalf("want web-backup, v1 and v2, got %+v", res.Records)
	}
	r := res.Records[0]
	if r.Driver != "secret" || r.Object != "web-backup" || r.Revision != 0 || !r.Visible || r.Status != "deployed" {
		t.Errorf("web-backup: %+v", r)
	}
	found := false
	for _, p := range r.Problems {
		found = found || p == "record says web revision 7"
	}
	if !found {
		t.Errorf("web-backup problems %q, want \"record says web revision 7\"", r.Problems)
	}
	if want := "kubectl delete secret web-backup -n prod"; r.Fix != want {
		t.Errorf("web-backup fix %q, want %q", r.Fix, want)
	}
	for i, r := range res.Records[1:] {
		if r.Object != storageKey("web", i+1) || !r.Visible || r.Problems != nil {
			t.Errorf("record %d: %+v", i+1, r)
		}
	}
}
