package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	metadatafake "k8s.io/client-go/metadata/fake"

	"github.com/aakuz/helm-debug/internal/helm"
	"github.com/aakuz/helm-debug/internal/kube"
)

const simpleManifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\n"

// badRecord stores a release record that cannot be decoded.
func (c *cluster) badRecord(name, ns string) {
	c.t.Helper()
	_, err := c.kube.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1." + name + ".v1",
			Namespace: ns,
			Labels:    map[string]string{"owner": "helm", "name": name, "version": "1"},
		},
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{"release": []byte("not-a-release")},
	}, metav1.CreateOptions{})
	if err != nil {
		c.t.Fatal(err)
	}
}

func wantExecError(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || errors.Is(err, ErrProblems) {
		t.Fatalf("want execution error containing %q, got %v", substr, err)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("error %q lacks %q", err, substr)
	}
}

// ---- resourceRow.problem ----

func TestResourceRowProblem(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  resourceRow
		want bool
	}{
		{"ok", resourceRow{Status: kube.StatusPresent, Ownership: kube.OwnershipOK}, false},
		{"absent", resourceRow{Status: kube.StatusAbsent}, true},
		{"unknown kind", resourceRow{Status: kube.StatusUnknownKind}, true},
		{"lookup error", resourceRow{Status: kube.StatusError}, true},
		{"ownership missing", resourceRow{Status: kube.StatusPresent, Ownership: kube.OwnershipMissing}, true},
		{"ownership mismatch", resourceRow{Status: kube.StatusPresent, Ownership: kube.OwnershipMismatch}, true},
		{"present but ownership unset", resourceRow{Status: kube.StatusPresent}, true},
		{"absent hook", resourceRow{Object: helm.Object{Hook: []string{"pre-upgrade"}}, Status: kube.StatusAbsent}, false},
		{"hook with bad ownership", resourceRow{Object: helm.Object{Hook: []string{"hook"}}, Status: kube.StatusPresent, Ownership: kube.OwnershipMismatch}, false},
	} {
		if got := tc.row.problem(); got != tc.want {
			t.Errorf("%s: problem() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ---- parseKindName ----

func TestParseKindName(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		kind, name string
		wantErr    bool
	}{
		{[]string{"deployment/api"}, "deployment", "api", false},
		{[]string{"deploy", "api"}, "deploy", "api", false},
		{[]string{"certificates.cert-manager.io/web"}, "certificates.cert-manager.io", "web", false},
		{[]string{"cm/a/b"}, "cm", "a/b", false}, // only the first slash splits
		{[]string{"api"}, "", "", true},
		{[]string{"/api"}, "", "", true},
		{[]string{"deployment/"}, "", "", true},
		{[]string{"/"}, "", "", true},
		{[]string{""}, "", "", true},
	} {
		kind, name, err := parseKindName(tc.args)
		if (err != nil) != tc.wantErr || kind != tc.kind || name != tc.name {
			t.Errorf("parseKindName(%q) = (%q, %q, %v), want (%q, %q, err=%v)", tc.args, kind, name, err, tc.kind, tc.name, tc.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "KIND/NAME") {
			t.Errorf("parseKindName(%q) error %q should explain the expected forms", tc.args, err)
		}
	}
}

// ---- find ----

func TestFindArgumentErrors(t *testing.T) {
	c := backendCluster(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"find"}, "accepts between 1 and 2 arg(s), received 0"},
		{[]string{"find", "a", "b", "c"}, "accepts between 1 and 2 arg(s), received 3"},
		{[]string{"find", "backend"}, "KIND/NAME"},
		{[]string{"find", "/backend"}, "KIND/NAME"},
		{[]string{"find", "deployment/"}, "KIND/NAME"},
	} {
		_, err := run(t, c, "prod", tc.args...)
		wantExecError(t, err, tc.want)
	}
}

func TestFindTwoArgumentForm(t *testing.T) {
	out, err := run(t, backendCluster(t), "prod", "find", "deployment", "backend")
	wantProblems(t, err, false)
	for _, want := range []string{"prod/Deployment/backend", "annotated with release prod/backend", "Live object"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestFindUnknownKindNoMatch(t *testing.T) {
	out, err := run(t, backendCluster(t), "prod", "find", "Gadget/nothing")
	wantProblems(t, err, true)
	golden(t, out)
}

func TestFindUnknownKindMatchedByName(t *testing.T) {
	// Widget is not served by the fake cluster, but the release declares it: matching falls back to the typed kind.
	out, err := run(t, backendCluster(t), "prod", "find", "widget/backend-widget")
	wantProblems(t, err, false)
	if !strings.Contains(out, "backend") || strings.Contains(out, "Live object") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestFindNoMatchExitsTwoAndPrintsReport(t *testing.T) {
	out, err := run(t, backendCluster(t), "prod", "find", "service/nothing")
	wantProblems(t, err, true)
	if !strings.Contains(out, "is not declared by any Helm release") {
		t.Fatalf("missing explanation:\n%s", out)
	}
}

func TestFindNoMatchJSONHasEmptyMatchesArray(t *testing.T) {
	out, err := run(t, backendCluster(t), "prod", "find", "service/nothing", "-o", "json")
	wantProblems(t, err, true)
	var res struct {
		Matches []any
		Source  string
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatal(jerr)
	}
	if res.Matches == nil || len(res.Matches) != 0 || res.Source != "scan" {
		t.Fatalf("unexpected json: %s", out)
	}
	if !strings.Contains(out, `"matches": []`) {
		t.Fatalf("matches must serialize as [] not null:\n%s", out)
	}
}

func TestFindAllRevisionsListsEveryRevision(t *testing.T) {
	out, err := run(t, backendCluster(t), "prod", "find", "deployment/backend", "--all-revisions", "-o", "json")
	wantProblems(t, err, false)
	var res struct {
		Source  string
		Matches []struct{ Revision int }
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatal(jerr)
	}
	if res.Source != "annotation" || len(res.Matches) != 2 {
		t.Fatalf("want both revisions via annotation, got %s", out)
	}
	out, err = run(t, backendCluster(t), "prod", "find", "deployment/backend", "-o", "json")
	wantProblems(t, err, false)
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	res2 := res
	if len(res2.Matches) != 1 || res2.Matches[0].Revision != 2 {
		t.Fatalf("default must be latest revision only, got %s", out)
	}
}

func TestFindAllNamespacesShortAndLongFlag(t *testing.T) {
	for _, flagName := range []string{"-A", "--all-namespaces"} {
		out, err := run(t, backendCluster(t), "staging", "find", "job/backend-migrate", flagName)
		wantProblems(t, err, false)
		if !strings.Contains(out, "prod") {
			t.Fatalf("%s: match in prod expected:\n%s", flagName, out)
		}
	}
	_, err := run(t, backendCluster(t), "staging", "find", "job/backend-migrate")
	wantProblems(t, err, true)
}

func TestFindClusterScopedIgnoresNamespace(t *testing.T) {
	out, err := run(t, backendCluster(t), "somewhere-else", "find", "clusterrole/backend-reader")
	wantProblems(t, err, false)
	if !strings.Contains(out, "annotation") && !strings.Contains(out, "prod") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestFindAnnotatedReleaseUnreadable(t *testing.T) {
	c := newCluster(t)
	c.object("v1", "ConfigMap", "prod", "cfg", "ghost", "prod")
	out, err := run(t, c, "prod", "find", "configmap/cfg")
	wantProblems(t, err, true)
	if !strings.Contains(out, "annotated with release prod/ghost, but it cannot be read") {
		t.Fatalf("missing warning:\n%s", out)
	}
}

func TestFindAnnotatedReleaseDoesNotDeclareObject(t *testing.T) {
	c := newCluster(t)
	c.release("other", "prod", 1, common.StatusDeployed, simpleManifest)
	c.object("v1", "ConfigMap", "prod", "stray", "other", "prod")
	out, err := run(t, c, "prod", "find", "configmap/stray")
	wantProblems(t, err, true)
	if !strings.Contains(out, "that release does not declare it") {
		t.Fatalf("missing warning:\n%s", out)
	}
}

func TestFindDeclaredByTwoReleasesWarns(t *testing.T) {
	c := newCluster(t)
	c.release("one", "prod", 1, common.StatusDeployed, simpleManifest)
	c.release("two", "prod", 1, common.StatusDeployed, simpleManifest)
	c.release("three", "other", 1, common.StatusDeployed, simpleManifest)
	out, err := run(t, c, "prod", "find", "configmap/cfg")
	wantProblems(t, err, false)
	if !strings.Contains(out, "declared by 2 releases (prod/one, prod/two)") {
		t.Fatalf("missing multi-owner warning:\n%s", out)
	}
	out, err = run(t, c, "prod", "find", "configmap/cfg", "--all-revisions")
	wantProblems(t, err, false)
	if strings.Contains(out, "declared by") {
		t.Fatalf("--all-revisions must not emit the multi-owner warning:\n%s", out)
	}
}

func TestFindLiveObjectWithoutAnnotationsWarns(t *testing.T) {
	c := newCluster(t)
	c.release("one", "prod", 1, common.StatusDeployed, simpleManifest)
	c.object("v1", "ConfigMap", "prod", "cfg", "", "")
	out, err := run(t, c, "prod", "find", "configmap/cfg")
	wantProblems(t, err, false)
	if !strings.Contains(out, "Helm will refuse to adopt it") {
		t.Fatalf("missing adoption warning:\n%s", out)
	}
}

func TestFindDecodeErrorsReported(t *testing.T) {
	c := backendCluster(t)
	c.badRecord("broken", "prod")
	out, err := run(t, c, "prod", "find", "service/nothing")
	wantProblems(t, err, true)
	if !strings.Contains(out, "1 could not be decoded") {
		t.Fatalf("decode errors not reported:\n%s", out)
	}
	out, err = run(t, c, "prod", "find", "service/nothing", "-o", "json")
	wantProblems(t, err, true)
	if !strings.Contains(out, `"decodeErrors": 1`) {
		t.Fatalf("decodeErrors missing:\n%s", out)
	}
}

func TestFindColorHighlightsWarnings(t *testing.T) {
	c := newCluster(t)
	c.object("v1", "ConfigMap", "prod", "cfg", "ghost", "prod")
	var out bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut, o.Color = &out, &bytes.Buffer{}, boolPtr(true)
	o.NewClients = func(*Options) (*Clients, error) { return c.clients("prod"), nil }
	cmd := NewRootCommand(o)
	cmd.SetArgs([]string{"find", "configmap/cfg"})
	if err := cmd.Execute(); !errors.Is(err, ErrProblems) {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), yellow+"warning:"+reset) {
		t.Fatalf("warning not colored: %q", out.String())
	}
}

func TestFindClientsError(t *testing.T) {
	o := NewOptions()
	o.Out, o.ErrOut = &bytes.Buffer{}, &bytes.Buffer{}
	o.NewClients = func(*Options) (*Clients, error) { return nil, errors.New("no cluster") }
	cmd := NewRootCommand(o)
	for _, args := range [][]string{
		{"find", "configmap/x"}, {"history", "x"}, {"stuck"}, {"diff", "x", "1", "2"}, {"resources", "x"}, {"drift", "x"},
	} {
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil || err.Error() != "no cluster" {
			t.Errorf("%v: got %v", args, err)
		}
	}
}

// ---- history ----

func TestHistoryUnknownRelease(t *testing.T) {
	_, err := run(t, backendCluster(t), "prod", "history", "nope")
	if !errors.Is(err, helm.ErrReleaseNotFound) || !strings.Contains(err.Error(), "prod/nope") {
		t.Fatalf("got %v", err)
	}
	_, err = run(t, backendCluster(t), "other-ns", "history", "backend")
	if !errors.Is(err, helm.ErrReleaseNotFound) {
		t.Fatalf("release in another namespace must not be found, got %v", err)
	}
}

func TestHistoryArgumentCount(t *testing.T) {
	c := backendCluster(t)
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"history"}, "accepts 1 arg(s), received 0"}, {[]string{"history", "a", "b"}, "accepts 1 arg(s), received 2"}} {
		_, err := run(t, c, "prod", tc.args...)
		wantExecError(t, err, tc.want)
	}
}

func historyCluster(t *testing.T) *cluster {
	c := newCluster(t)
	for v := 1; v <= 4; v++ {
		st := common.StatusSuperseded
		if v == 4 {
			st = common.StatusDeployed
		}
		c.release("backend", "prod", v, st, simpleManifest)
	}
	return c
}

func TestHistoryMax(t *testing.T) {
	for _, tc := range []struct {
		max       string
		wantRevs  []string
		wantAbsOf string
	}{
		{"1", []string{"4"}, "3"},
		{"2", []string{"4", "3"}, "2"},
		{"0", []string{"4", "3", "2", "1"}, ""},
		{"10", []string{"4", "3", "2", "1"}, ""},
		{"-1", []string{"4", "3", "2", "1"}, ""},
	} {
		out, err := run(t, historyCluster(t), "prod", "history", "backend", "--max", tc.max, "-o", "json")
		wantProblems(t, err, false)
		var res struct{ Revisions []struct{ Revision int } }
		if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
			t.Fatal(jerr)
		}
		if len(res.Revisions) != len(tc.wantRevs) {
			t.Errorf("--max %s: got %d revisions, want %d", tc.max, len(res.Revisions), len(tc.wantRevs))
			continue
		}
		for i, r := range res.Revisions {
			if got := strconv.Itoa(r.Revision); got != tc.wantRevs[i] {
				t.Errorf("--max %s: revision[%d] = %s, want %s", tc.max, i, got, tc.wantRevs[i])
			}
		}
	}
}

func TestHistoryMaxStillReportsLatestStatus(t *testing.T) {
	c := newCluster(t)
	c.release("backend", "prod", 1, common.StatusDeployed, simpleManifest)
	c.release("backend", "prod", 2, common.StatusFailed, simpleManifest)
	_, err := run(t, c, "prod", "history", "backend", "--max", "1")
	wantProblems(t, err, true)
}

func TestHistoryPendingLatestIsProblem(t *testing.T) {
	for _, st := range []common.Status{common.StatusPendingInstall, common.StatusPendingUpgrade, common.StatusPendingRollback} {
		c := newCluster(t)
		c.release("backend", "prod", 1, st, simpleManifest)
		_, err := run(t, c, "prod", "history", "backend")
		if !errors.Is(err, ErrProblems) {
			t.Errorf("%s: want ErrProblems, got %v", st, err)
		}
	}
}

func TestHistoryOldFailureIsNotProblem(t *testing.T) {
	c := newCluster(t)
	c.release("backend", "prod", 1, common.StatusFailed, simpleManifest)
	c.release("backend", "prod", 2, common.StatusDeployed, simpleManifest)
	_, err := run(t, c, "prod", "history", "backend")
	wantProblems(t, err, false)
}

func TestHistoryFormats(t *testing.T) {
	c := newCluster(t)
	c.release("backend", "prod", 1, common.StatusFailed, simpleManifest)
	out, err := run(t, c, "prod", "history", "backend", "-o", "yaml")
	wantProblems(t, err, true)
	for _, want := range []string{"release: backend", "namespace: prod", "chart: backend-1.1.0", "appVersion: \"2.0\"", "timed out waiting"} {
		if !strings.Contains(out, want) {
			t.Errorf("yaml lacks %q:\n%s", want, out)
		}
	}
	out, err = run(t, c, "prod", "history", "backend", "-o", "json")
	wantProblems(t, err, true)
	if !strings.Contains(out, `"status": "failed"`) {
		t.Fatalf("json lacks status:\n%s", out)
	}
}

func TestHistoryTableHasDashForEmptyCells(t *testing.T) {
	c := newCluster(t)
	c.release("backend", "prod", 1, common.StatusDeployed, simpleManifest)
	out, err := run(t, c, "prod", "history", "backend")
	wantProblems(t, err, false)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[1], "-") {
		t.Fatalf("empty description must render as '-':\n%s", out)
	}
}

func TestHistoryWithoutInfoOrChart(t *testing.T) {
	row := toHistoryRow(&rspb.Release{Version: 3})
	if row.Revision != 3 || row.Status != "" || row.Chart != "" || !row.Updated.IsZero() {
		t.Fatalf("unexpected row %+v", row)
	}
}

// ---- stuck ----

func TestStuckNoReleases(t *testing.T) {
	out, err := run(t, newCluster(t), "prod", "stuck")
	wantProblems(t, err, false)
	if out != "No stuck releases among 0 scanned\n" {
		t.Fatalf("got %q", out)
	}
}

func stuckCluster(t *testing.T) *cluster {
	c := newCluster(t)
	c.release("rolled", "prod", 1, common.StatusSuperseded, simpleManifest)
	c.release("rolled", "prod", 2, common.StatusDeployed, simpleManifest)
	c.release("rolled", "prod", 3, common.StatusPendingUpgrade, simpleManifest)
	c.release("fresh", "prod", 1, common.StatusPendingInstall, simpleManifest)
	c.release("elsewhere", "dev", 1, common.StatusPendingRollback, simpleManifest)
	return c
}

func TestStuckFixCommands(t *testing.T) {
	out, err := run(t, stuckCluster(t), "prod", "stuck", "-A")
	wantProblems(t, err, true)
	for _, want := range []string{
		"helm rollback rolled 2 -n prod",
		"helm uninstall fresh -n prod",
		"helm uninstall elsewhere -n dev",
		"pending-rollback",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestStuckNamespaceScope(t *testing.T) {
	out, err := run(t, stuckCluster(t), "prod", "stuck")
	wantProblems(t, err, true)
	if strings.Contains(out, "elsewhere") {
		t.Fatalf("dev release leaked into the prod scan:\n%s", out)
	}
	out, err = run(t, stuckCluster(t), "dev", "stuck")
	wantProblems(t, err, true)
	if !strings.Contains(out, "elsewhere") || strings.Contains(out, "rolled") {
		t.Fatalf("dev scan wrong:\n%s", out)
	}
	_, err = run(t, stuckCluster(t), "empty", "stuck")
	wantProblems(t, err, false)
}

func TestStuckOlderThan(t *testing.T) {
	// release() sets LastDeployed = now - (10 - version)h, so version 9 is 1h old and version 1 is 9h old.
	c := newCluster(t)
	c.release("hour", "prod", 9, common.StatusPendingUpgrade, simpleManifest)
	c.release("old", "prod", 1, common.StatusPendingUpgrade, simpleManifest)
	for _, tc := range []struct {
		olderThan string
		want      []string
		notWant   []string
		problems  bool
	}{
		{"0s", []string{"hour", "old"}, nil, true},
		{"1h", []string{"hour", "old"}, nil, true}, // exactly 1h old is not "more recent"
		{"90m", []string{"old"}, []string{"hour"}, true},
		{"10h", nil, []string{"hour", "old"}, false},
	} {
		out, err := run(t, c, "prod", "stuck", "--older-than", tc.olderThan)
		wantProblems(t, err, tc.problems)
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("--older-than %s: output lacks %q:\n%s", tc.olderThan, w, out)
			}
		}
		for _, w := range tc.notWant {
			if strings.Contains(out, w) {
				t.Errorf("--older-than %s: output has %q:\n%s", tc.olderThan, w, out)
			}
		}
	}
}

func TestStuckOlderThanInvalid(t *testing.T) {
	_, err := run(t, newCluster(t), "prod", "stuck", "--older-than", "soon")
	wantExecError(t, err, `invalid argument "soon" for "--older-than"`)
}

func TestStuckRejectsArguments(t *testing.T) {
	_, err := run(t, newCluster(t), "prod", "stuck", "extra")
	wantExecError(t, err, `unknown command "extra"`)
}

func TestStuckDecodeErrors(t *testing.T) {
	c := newCluster(t)
	c.release("ok", "prod", 1, common.StatusDeployed, simpleManifest)
	c.badRecord("broken", "prod")
	out, err := run(t, c, "prod", "stuck")
	wantProblems(t, err, false)
	golden(t, out)
	out, err = run(t, c, "prod", "stuck", "-o", "json")
	wantProblems(t, err, false)
	var res struct {
		Scanned, DecodeErrors int
		Releases              []any
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatal(jerr)
	}
	if res.Scanned != 2 || res.DecodeErrors != 1 || res.Releases == nil {
		t.Fatalf("unexpected json: %s", out)
	}
}

func TestStuckJSONAndYAML(t *testing.T) {
	out, err := run(t, stuckCluster(t), "prod", "stuck", "-o", "json")
	wantProblems(t, err, true)
	var res struct {
		Scanned  int
		Releases []struct{ Release, Status, Fix string }
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatal(jerr)
	}
	if res.Scanned != 2 || len(res.Releases) != 2 {
		t.Fatalf("unexpected json: %s", out)
	}
	out, err = run(t, stuckCluster(t), "prod", "stuck", "-o", "yaml")
	wantProblems(t, err, true)
	if !strings.Contains(out, "fix: helm rollback rolled 2 -n prod") {
		t.Fatalf("yaml lacks fix:\n%s", out)
	}
}

func TestUnlockCommandIgnoresNewerAndFailedRevisions(t *testing.T) {
	c := newCluster(t)
	c.release("app", "prod", 1, common.StatusFailed, simpleManifest)
	c.release("app", "prod", 2, common.StatusPendingUpgrade, simpleManifest)
	c.release("app", "prod", 3, common.StatusDeployed, simpleManifest)
	store := c.clients("prod").Store
	if got := unlockCommand(store, "app", "prod", 2); got != "helm uninstall app -n prod" {
		t.Fatalf("only a failed revision precedes: got %q", got)
	}
	if got := unlockCommand(store, "app", "prod", 4); got != "helm rollback app 3 -n prod" {
		t.Fatalf("got %q", got)
	}
	if got := unlockCommand(store, "unknown", "prod", 2); got != "helm uninstall unknown -n prod" {
		t.Fatalf("unknown release: got %q", got)
	}
}

func TestPrintStuckWithoutTimestamp(t *testing.T) {
	var b bytes.Buffer
	o := &Options{Now: func() time.Time { return now }}
	printStuck(o, &b, stuckResult{Scanned: 1, Releases: []stuckRow{{Release: "r", Namespace: "n", Revision: 1, Status: "pending-install", Fix: "x"}}})
	if lines := strings.Split(strings.TrimSpace(b.String()), "\n"); len(lines) != 2 || !strings.Contains(lines[1], "-    x") {
		t.Fatalf("missing timestamp must render '-' age:\n%s", b.String())
	}
}

// ---- diff ----

func TestDiffUnknownRelease(t *testing.T) {
	_, err := run(t, diffCluster(t), "prod", "diff", "nope", "1", "2")
	if !errors.Is(err, helm.ErrReleaseNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestDiffUnknownRevisionNamesIt(t *testing.T) {
	_, err := run(t, diffCluster(t), "prod", "diff", "api", "1", "9")
	if !errors.Is(err, helm.ErrReleaseNotFound) || !strings.Contains(err.Error(), "revision 9") {
		t.Fatalf("got %v", err)
	}
	_, err = run(t, diffCluster(t), "prod", "diff", "api", "9", "1")
	if !errors.Is(err, helm.ErrReleaseNotFound) || !strings.Contains(err.Error(), "revision 9") {
		t.Fatalf("got %v", err)
	}
}

func TestDiffBadRevisionForms(t *testing.T) {
	c := diffCluster(t)
	for _, bad := range []string{"0", "1.5", "", "latest"} {
		_, err := run(t, c, "prod", "diff", "api", bad, "2")
		wantExecError(t, err, "invalid revision")
		_, err = run(t, c, "prod", "diff", "api", "1", bad)
		wantExecError(t, err, "invalid revision")
	}
}

func TestDiffArgumentCount(t *testing.T) {
	c := diffCluster(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"diff"}, "accepts 3 arg(s), received 0"},
		{[]string{"diff", "api"}, "accepts 3 arg(s), received 1"},
		{[]string{"diff", "api", "1"}, "accepts 3 arg(s), received 2"},
		{[]string{"diff", "api", "1", "2", "3"}, "accepts 3 arg(s), received 4"},
	} {
		_, err := run(t, c, "prod", tc.args...)
		wantExecError(t, err, tc.want)
	}
}

func TestDiffSameRevisionJSON(t *testing.T) {
	out, err := run(t, diffCluster(t), "prod", "diff", "api", "1", "1", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		From, To int
		Manifest []any
		Values   struct{ Status string }
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatal(jerr)
	}
	if res.From != 1 || res.To != 1 || len(res.Manifest) != 0 || res.Values.Status != "unchanged" {
		t.Fatalf("unexpected result: %s", out)
	}
	if !strings.Contains(out, `"manifest": []`) {
		t.Fatalf("manifest must serialize as []:\n%s", out)
	}
}

func TestDiffReversedRevisionsSwapStatuses(t *testing.T) {
	out, err := run(t, diffCluster(t), "prod", "diff", "api", "2", "1", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Manifest []struct{ Kind, Name, Status string }
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatal(jerr)
	}
	got := map[string]string{}
	for _, m := range res.Manifest {
		got[m.Kind+"/"+m.Name] = m.Status
	}
	if got["Service/added"] != "removed" || got["ConfigMap/removed"] != "added" {
		t.Fatalf("statuses not swapped: %v", got)
	}
}

func TestDiffValuesOnlyChange(t *testing.T) {
	c := newCluster(t)
	c.releaseWithValues("api", "prod", 1, common.StatusSuperseded, simpleManifest, map[string]any{"a": 1})
	c.releaseWithValues("api", "prod", 2, common.StatusDeployed, simpleManifest, map[string]any{"a": 2})
	out, err := run(t, c, "prod", "diff", "api", "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Manifest:\n  No differences.", "-a: 1", "+a: 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDiffInvalidManifest(t *testing.T) {
	for name, manifest := range map[string]string{
		"bad yaml":        "kind: [unclosed\n",
		"missing name":    "apiVersion: v1\nkind: ConfigMap\nmetadata: {}\n",
		"missing kind":    "apiVersion: v1\nmetadata:\n  name: x\n",
		"missing version": "kind: ConfigMap\nmetadata:\n  name: x\n",
	} {
		want := "missing apiVersion, kind or metadata.name"
		if name == "bad yaml" {
			want = "invalid YAML in document 1"
		}
		for _, bad := range []int{1, 2} {
			c := newCluster(t)
			m1, m2 := simpleManifest, simpleManifest
			if bad == 1 {
				m1 = manifest
			} else {
				m2 = manifest
			}
			c.release("api", "prod", 1, common.StatusSuperseded, m1)
			c.release("api", "prod", 2, common.StatusDeployed, m2)
			_, err := run(t, c, "prod", "diff", "api", "1", "2")
			wantExecError(t, err, want)
		}
	}
}

const secretV1 = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: creds\ndata:\n  password: aHVudGVyMg==\n"
const secretV2 = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: creds\ndata:\n  password: aHVudGVyMw==\n"

func TestDiffShowSecretsFlag(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusSuperseded, secretV1)
	c.release("api", "prod", 2, common.StatusDeployed, secretV2)
	masked, err := run(t, c, "prod", "diff", "api", "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(masked, "aHVudGVy") || !strings.Contains(masked, "<masked:old>") || !strings.Contains(masked, "<masked:new>") {
		t.Fatalf("secret not masked:\n%s", masked)
	}
	shown, err := run(t, c, "prod", "diff", "api", "1", "2", "--show-secrets")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shown, "-  password: aHVudGVyMg==") || !strings.Contains(shown, "+  password: aHVudGVyMw==") {
		t.Fatalf("--show-secrets must reveal values:\n%s", shown)
	}
}

func TestDiffAddedAndRemovedSecretsAreMasked(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusSuperseded, simpleManifest)
	c.release("api", "prod", 2, common.StatusDeployed, simpleManifest+"---\n"+secretV1)
	for _, args := range [][]string{{"1", "2"}, {"2", "1"}} {
		out, err := run(t, c, "prod", append([]string{"diff", "api"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "aHVudGVy") || !strings.Contains(out, "<masked>") {
			t.Fatalf("%v: secret leaked or not masked:\n%s", args, out)
		}
	}
}

func TestDiffYAMLOutput(t *testing.T) {
	out, err := run(t, diffCluster(t), "prod", "diff", "api", "1", "2", "-o", "yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"release: api", "from: 1", "to: 2", "kind: Service", "status: added", "status: removed"} {
		if !strings.Contains(out, want) {
			t.Errorf("yaml lacks %q:\n%s", want, out)
		}
	}
}

// ---- resources ----

func TestResourcesUnknownRevision(t *testing.T) {
	for _, rev := range []string{"9", "-1"} {
		_, err := run(t, backendCluster(t), "prod", "resources", "backend", "--revision", rev)
		if !errors.Is(err, helm.ErrReleaseNotFound) || !strings.Contains(err.Error(), "revision "+rev) {
			t.Errorf("--revision %s: got %v", rev, err)
		}
	}
	_, err := run(t, backendCluster(t), "prod", "resources", "backend", "-r", "x")
	wantExecError(t, err, `invalid argument "x" for "-r, --revision"`)
}

func TestResourcesRevisionSelectsRecord(t *testing.T) {
	out, err := run(t, backendCluster(t), "prod", "resources", "backend", "--revision", "1", "-o", "json")
	wantProblems(t, err, true)
	if !strings.Contains(out, `"revision": 1`) || !strings.Contains(out, `"status": "superseded"`) {
		t.Fatalf("wrong revision:\n%s", out)
	}
}

func TestResourcesUnknownKindRow(t *testing.T) {
	out, err := run(t, backendCluster(t), "prod", "resources", "backend")
	wantProblems(t, err, true)
	if !strings.Contains(out, "unknown-kind") || !strings.Contains(out, "error: Widget/backend-widget") {
		t.Fatalf("unknown kind not reported:\n%s", out)
	}
}

func TestResourcesNothingExistsWarns(t *testing.T) {
	c := newCluster(t)
	c.release("gone", "prod", 1, common.StatusUninstalled, simpleManifest)
	out, err := run(t, c, "prod", "resources", "gone")
	wantProblems(t, err, true)
	if !strings.Contains(out, "none of the declared objects exist") {
		t.Fatalf("missing warning:\n%s", out)
	}
}

func TestResourcesOnlyHooksIsHealthy(t *testing.T) {
	c := newCluster(t)
	c.release("jobs", "prod", 1, common.StatusDeployed, "", migrateHook)
	out, err := run(t, c, "prod", "resources", "jobs")
	wantProblems(t, err, false)
	if !strings.Contains(out, "pre-upgrade") {
		t.Fatalf("hook missing:\n%s", out)
	}
}

func TestResourcesMismatchOwnerAndFixes(t *testing.T) {
	out, err := run(t, backendCluster(t), "prod", "resources", "backend", "-o", "json")
	wantProblems(t, err, true)
	var res struct {
		Fixes     []string
		Resources []struct{ Kind, Name, Ownership, Owner string }
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatal(jerr)
	}
	owners := map[string]string{}
	for _, r := range res.Resources {
		owners[r.Kind+"/"+r.Name] = r.Ownership + "|" + r.Owner
	}
	if !strings.HasPrefix(owners["ConfigMap/backend-config"], "mismatch|prod/frontend") {
		t.Fatalf("mismatch owner wrong: %v", owners)
	}
	if !strings.HasPrefix(owners["Service/backend"], "missing|") {
		t.Fatalf("missing ownership wrong: %v", owners)
	}
	if len(res.Fixes) == 0 {
		t.Fatal("fixes expected")
	}
}

func TestResourcesYAML(t *testing.T) {
	out, err := run(t, backendCluster(t), "prod", "resources", "backend", "-o", "yaml")
	wantProblems(t, err, true)
	if !strings.Contains(out, "release: backend") || !strings.Contains(out, "fixes:") {
		t.Fatalf("unexpected yaml:\n%s", out)
	}
}

// ---- drift ----

func TestDriftInvalidManifest(t *testing.T) {
	for name, manifest := range map[string]string{
		"bad yaml":     "kind: [unclosed\n",
		"missing name": "apiVersion: v1\nkind: ConfigMap\nmetadata: {}\n",
	} {
		c := newCluster(t)
		c.release("api", "prod", 1, common.StatusDeployed, manifest)
		_, err := run(t, c, "prod", "drift", "api")
		want := "parsing release manifest"
		if name == "missing name" {
			want = "missing apiVersion, kind or metadata.name"
		}
		wantExecError(t, err, want)
	}
}

func TestDriftValuesWithStructuredOutputPrintsDocument(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, simpleManifest)
	for _, format := range []string{"json", "yaml"} {
		out, err := run(t, c, "prod", "drift", "api", "--values", "-o", format)
		wantProblems(t, err, true) // absent
		if strings.Contains(out, "# absent") || !strings.Contains(out, "absent") {
			t.Errorf("%s: --values must be ignored, got:\n%s", format, out)
		}
	}
}

func TestDriftValuesNoDrift(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusDeployed, simpleManifest)
	c.object("v1", "ConfigMap", "prod", "cfg", "api", "prod")
	out, err := run(t, c, "prod", "drift", "api", "--values")
	wantProblems(t, err, false)
	if out != "# No drifted objects\n" {
		t.Fatalf("got %q", out)
	}
}

func TestDriftArgumentCount(t *testing.T) {
	c := newCluster(t)
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"drift"}, "accepts 1 arg(s), received 0"}, {[]string{"drift", "a", "b"}, "accepts 1 arg(s), received 2"}} {
		_, err := run(t, c, "prod", tc.args...)
		wantExecError(t, err, tc.want)
	}
}

// ---- resources arg handling ----

func TestResourcesArgumentCount(t *testing.T) {
	c := newCluster(t)
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"resources"}, "accepts 1 arg(s), received 0"}, {[]string{"resources", "a", "b"}, "accepts 1 arg(s), received 2"}} {
		_, err := run(t, c, "prod", tc.args...)
		wantExecError(t, err, tc.want)
	}
}

// ---- storage drivers ----

// configMapClients builds clients whose release records live in ConfigMaps.
func configMapClients(t *testing.T, ns string, rels ...*rspb.Release) *Clients {
	t.Helper()
	c := newCluster(t)
	var metas []runtime.Object
	for _, rel := range rels {
		key := "sh.helm.release.v1." + rel.Name + ".v" + strconv.Itoa(rel.Version)
		if err := driver.NewConfigMaps(c.kube.CoreV1().ConfigMaps(rel.Namespace)).Create(key, rel); err != nil {
			t.Fatal(err)
		}
	}
	cms, err := c.kube.CoreV1().ConfigMaps("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, cm := range cms.Items {
		metas = append(metas, &metav1.PartialObjectMetadata{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: cm.ObjectMeta,
		})
	}
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := c.clients(ns)
	cl.Store = &helm.Store{Kube: c.kube, Metadata: metadatafake.NewSimpleMetadataClient(scheme, metas...), Driver: helm.StorageConfigMap}
	return cl
}

func configMapRelease(name, ns string, version int, status common.Status) *rspb.Release {
	return &rspb.Release{
		Name: name, Namespace: ns, Version: version, Manifest: simpleManifest,
		Info: &rspb.Info{Status: status, LastDeployed: now.Add(-time.Hour)},
	}
}

func runWithClients(t *testing.T, cl *Clients, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut, o.Now = &out, &bytes.Buffer{}, func() time.Time { return now }
	o.NewClients = func(*Options) (*Clients, error) { return cl, nil }
	cmd := NewRootCommand(o)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestConfigMapStorage(t *testing.T) {
	cl := configMapClients(t, "prod",
		configMapRelease("app", "prod", 1, common.StatusSuperseded),
		configMapRelease("app", "prod", 2, common.StatusPendingUpgrade),
	)
	out, err := runWithClients(t, cl, "history", "app")
	wantProblems(t, err, true)
	if !strings.Contains(out, "pending-upgrade") || !strings.Contains(out, "superseded") {
		t.Fatalf("history over configmaps:\n%s", out)
	}
	out, err = runWithClients(t, cl, "stuck", "--older-than", "1m")
	wantProblems(t, err, true)
	if !strings.Contains(out, "helm rollback app 1 -n prod") {
		t.Fatalf("stuck over configmaps:\n%s", out)
	}
	out, err = runWithClients(t, cl, "find", "configmap/cfg", "-A")
	wantProblems(t, err, false)
	if !strings.Contains(out, "Scanned 1 release record(s)") {
		t.Fatalf("find scans configmap records:\n%s", out)
	}
	// Nothing is stored as Secrets, so the default driver sees no releases.
	cl.Store.Driver = helm.StorageSecret
	if _, err = runWithClients(t, cl, "history", "app"); !errors.Is(err, helm.ErrReleaseNotFound) {
		t.Fatalf("secret driver must not see configmap records, got %v", err)
	}
}

func TestStorageFlagReachesClientFactory(t *testing.T) {
	for _, tc := range []struct{ arg, want string }{
		{"configmap", helm.StorageConfigMap},
		{"secret", helm.StorageSecret},
	} {
		o := capturedOptions(t, "stuck", "--storage", tc.arg)
		if o.Storage != tc.arg {
			t.Fatalf("--storage %s: factory saw %q", tc.arg, o.Storage)
		}
		if got, err := helm.NormalizeDriver(o.Storage); err != nil || got != tc.want {
			t.Fatalf("NormalizeDriver(%q) = %q, %v; want %q", o.Storage, got, err, tc.want)
		}
	}
}
