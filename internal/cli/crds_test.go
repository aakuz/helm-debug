package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

// crdYAML renders the CRD <plural>.example.com. Versions read like the table:
// "v2*" is the storage version, "(v1beta1)" is not served.
func crdYAML(plural string, versions ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: %s.example.com
spec:
  group: example.com
  names:
    kind: %s
    plural: %s
  scope: Namespaced
  versions:
`, plural, strings.ToUpper(plural[:1])+plural[1:], plural)
	for _, v := range versions {
		served := !strings.HasPrefix(v, "(")
		v = strings.Trim(v, "()")
		storage := strings.HasSuffix(v, "*")
		v = strings.TrimSuffix(v, "*")
		fmt.Fprintf(&b, `  - name: %s
    served: %t
    storage: %t
    schema:
      openAPIV3Schema:
        type: object
`, v, served, storage)
	}
	return b.String()
}

// crdFile is a chart file under crds/ holding the given YAML documents.
func crdFile(name string, docs ...string) *chartcommon.File {
	return &chartcommon.File{Name: "crds/" + name, Data: []byte(strings.Join(docs, "---\n"))}
}

// crdRelease stores revision version of release prod/api whose chart "api" ships files.
func crdRelease(t *testing.T, c *cluster, version int, manifest string, files ...*chartcommon.File) {
	t.Helper()
	storeCRDRelease(t, c, version, manifest, &chart.Chart{Metadata: &chart.Metadata{Name: "api", Version: "1.0.0"}, Files: files})
}

func storeCRDRelease(t *testing.T, c *cluster, version int, manifest string, ch *chart.Chart) {
	t.Helper()
	rel := &rspb.Release{
		Name: "api", Namespace: "prod", Version: version, Manifest: manifest,
		Info:  &rspb.Info{Status: common.StatusDeployed, LastDeployed: now},
		Chart: ch,
	}
	key := fmt.Sprintf("sh.helm.release.v1.api.v%d", version)
	if err := driver.NewSecrets(c.kube.CoreV1().Secrets("prod")).Create(key, rel); err != nil {
		t.Fatal(err)
	}
}

// liveCRD adds the CRD doc to the cluster as the API server returns it: with server
// metadata, conversion (strategy None when conversion is nil) and status.storedVersions.
func liveCRD(t *testing.T, c *cluster, doc string, conversion map[string]any, stored ...string) {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
		t.Fatal(err)
	}
	u.SetUID("generated")
	u.SetResourceVersion("42")
	u.SetGeneration(3)
	u.SetAnnotations(map[string]string{"controller-gen.kubebuilder.io/version": "v0.16.0"})
	if conversion == nil {
		conversion = map[string]any{"strategy": "None"}
	}
	if err := unstructured.SetNestedField(u.Object, conversion, "spec", "conversion"); err != nil {
		t.Fatal(err)
	}
	storedAny := make([]any, len(stored))
	for i, s := range stored {
		storedAny[i] = s
	}
	u.Object["status"] = map[string]any{
		"storedVersions": storedAny,
		"acceptedNames":  map[string]any{"kind": "X", "plural": "x"},
		"conditions":     []any{map[string]any{"type": "Established", "status": "True"}},
	}
	c.live = append(c.live, u)
}

type crdVersionJSON struct {
	Name    string `json:"name"`
	Served  bool   `json:"served"`
	Storage bool   `json:"storage"`
}

type crdRowJSON struct {
	Name            string           `json:"name"`
	Source          string           `json:"source"`
	Status          string           `json:"status"`
	ChartVersions   []crdVersionJSON `json:"chartVersions"`
	ClusterVersions []crdVersionJSON `json:"clusterVersions"`
	StoredVersions  []string         `json:"storedVersions"`
	Conversion      string           `json:"conversion"`
	WebhookService  string           `json:"webhookService"`
	Changes         []string         `json:"changes"`
	Diff            string           `json:"diff"`
	Notes           []string         `json:"notes"`
	Error           string           `json:"error"`
}

type crdsJSON struct {
	Release   string       `json:"release"`
	Namespace string       `json:"namespace"`
	Revision  int          `json:"revision"`
	CRDs      []crdRowJSON `json:"crds"`
}

// crdsReport runs `crds api -o json` and decodes the report.
func crdsReport(t *testing.T, c *cluster, args ...string) (crdsJSON, error) {
	t.Helper()
	out, err := run(t, c, "prod", append([]string{"crds", "api", "-o", "json"}, args...)...)
	var res crdsJSON
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("bad json (err %v): %v\n%s", err, jerr, out)
	}
	return res, err
}

func oneCRD(t *testing.T, res crdsJSON) crdRowJSON {
	t.Helper()
	if len(res.CRDs) != 1 {
		t.Fatalf("want one CRD row, got %+v", res.CRDs)
	}
	return res.CRDs[0]
}

func versionsEqual(a, b []crdVersionJSON) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCrdsStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		live     bool
		problems bool
		want     string
	}{
		{"in-sync despite API defaults and server metadata", true, false, "in-sync"},
		{"absent", false, true, "absent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newCluster(t)
			crdRelease(t, c, 1, "", crdFile("widgets.yaml", crdYAML("widgets", "v1*")))
			if tt.live {
				liveCRD(t, c, crdYAML("widgets", "v1*"), nil, "v1")
			}
			res, err := crdsReport(t, c)
			wantProblems(t, err, tt.problems)
			row := oneCRD(t, res)
			if row.Name != "widgets.example.com" || row.Source != "api/crds/widgets.yaml" || row.Status != tt.want {
				t.Fatalf("row = %+v", row)
			}
			if len(row.Changes) != 0 || row.Diff != "" {
				t.Errorf("unexpected changes: %+v", row)
			}
			if want := []crdVersionJSON{{"v1", true, true}}; !versionsEqual(row.ChartVersions, want) {
				t.Errorf("chartVersions = %+v", row.ChartVersions)
			}
			if tt.live {
				if !versionsEqual(row.ClusterVersions, []crdVersionJSON{{"v1", true, true}}) || len(row.StoredVersions) != 1 || row.StoredVersions[0] != "v1" || row.Conversion != "None" {
					t.Errorf("live summary = %+v", row)
				}
			} else if len(row.ClusterVersions) != 0 || len(row.StoredVersions) != 0 || len(row.Notes) != 0 {
				t.Errorf("absent CRD has a live summary: %+v", row)
			}
		})
	}
}

func TestCrdsOutdatedNewVersion(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	crdRelease(t, c, 1, "", crdFile("widgets.yaml", crdYAML("widgets", "v1", "v2*")))
	liveCRD(t, c, crdYAML("widgets", "v1*"), nil, "v1")
	res, err := crdsReport(t, c)
	wantProblems(t, err, true)
	row := oneCRD(t, res)
	if row.Status != "outdated" {
		t.Fatalf("status = %q", row.Status)
	}
	versionPath := false
	for _, p := range row.Changes {
		versionPath = versionPath || strings.HasPrefix(p, "/spec/versions")
	}
	if !versionPath {
		t.Errorf("changes lack a /spec/versions path: %v", row.Changes)
	}
	if !versionsEqual(row.ChartVersions, []crdVersionJSON{{"v1", true, false}, {"v2", true, true}}) {
		t.Errorf("chartVersions = %+v", row.ChartVersions)
	}
	if !versionsEqual(row.ClusterVersions, []crdVersionJSON{{"v1", true, true}}) {
		t.Errorf("clusterVersions = %+v", row.ClusterVersions)
	}
	if len(row.StoredVersions) != 1 || row.StoredVersions[0] != "v1" {
		t.Errorf("storedVersions = %v", row.StoredVersions)
	}
	// The chart is the expected ("-") side: its v1 is not the storage version and
	// its v2 is missing from the cluster. The cluster's "storage: true" lines up
	// with the chart's v2 one, so the minimal diff has no "+" lines here.
	var chartV2, chartV1Storage bool
	for _, l := range strings.Split(row.Diff, "\n") {
		chartV2 = chartV2 || strings.HasPrefix(l, "-") && strings.Contains(l, "name: v2")
		chartV1Storage = chartV1Storage || strings.HasPrefix(l, "-") && strings.Contains(l, "storage: false")
	}
	if !chartV2 || !chartV1Storage || strings.Contains(row.Diff, "\n+") {
		t.Errorf("diff sides wrong:\n%s", row.Diff)
	}
	for _, unwanted := range []string{"conversion", "storedVersions", "resourceVersion", "uid"} {
		if strings.Contains(row.Diff, unwanted) {
			t.Errorf("diff contains %q:\n%s", unwanted, row.Diff)
		}
	}
	want := "storage version is v1 in the cluster, v2 in the chart"
	if len(row.Notes) != 1 || row.Notes[0] != want {
		t.Errorf("notes = %q, want [%q]", row.Notes, want)
	}
}

func TestCrdsStoredVersionDropped(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	crdRelease(t, c, 1, "", crdFile("widgets.yaml", crdYAML("widgets", "v1*")))
	liveCRD(t, c, crdYAML("widgets", "v1*"), nil, "v1beta1", "v1")
	res, err := crdsReport(t, c)
	wantProblems(t, err, false) // the spec matches; only the note tells about the migration
	row := oneCRD(t, res)
	want := "storedVersions has v1beta1, which the chart's CRD drops: the API server rejects the CRD until stored objects are migrated and v1beta1 is removed from status.storedVersions"
	if row.Status != "in-sync" || len(row.Notes) != 1 || row.Notes[0] != want {
		t.Fatalf("row = %+v", row)
	}
	if len(row.StoredVersions) != 2 || row.StoredVersions[0] != "v1beta1" || row.StoredVersions[1] != "v1" {
		t.Errorf("storedVersions = %v", row.StoredVersions)
	}
	out, err := run(t, c, "prod", "crds", "api")
	wantProblems(t, err, false)
	if !strings.Contains(out, "note: widgets.example.com: "+want) {
		t.Errorf("table lacks the note:\n%s", out)
	}
}

func TestCrdsConversionWebhook(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		clientConfig map[string]any
		want         string
	}{
		{"service", map[string]any{"service": map[string]any{"namespace": "cert-manager", "name": "cert-manager-webhook", "path": "/convert"}}, "cert-manager/cert-manager-webhook"},
		{"url", map[string]any{"url": "https://convert.example.com/convert"}, "https://convert.example.com/convert"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newCluster(t)
			crdRelease(t, c, 1, "", crdFile("widgets.yaml", crdYAML("widgets", "v1*")))
			liveCRD(t, c, crdYAML("widgets", "v1*"), map[string]any{
				"strategy": "Webhook",
				"webhook":  map[string]any{"clientConfig": tt.clientConfig, "conversionReviewVersions": []any{"v1"}},
			}, "v1")
			res, err := crdsReport(t, c)
			wantProblems(t, err, false)
			row := oneCRD(t, res)
			if row.Conversion != "Webhook" || row.WebhookService != tt.want {
				t.Fatalf("conversion = %q, webhookService = %q", row.Conversion, row.WebhookService)
			}
			out, err := run(t, c, "prod", "crds", "api")
			wantProblems(t, err, false)
			if !strings.Contains(out, "Webhook "+tt.want) {
				t.Errorf("table lacks the webhook:\n%s", out)
			}
		})
	}
}

// Charts often ship a placeholder caBundle that cert-manager's cainjector
// overwrites in the cluster, so a different caBundle alone is not drift.
func TestCrdsConversionCABundleIgnored(t *testing.T) {
	t.Parallel()
	chartCRD := crdYAML("widgets", "v1*") + `  conversion:
    strategy: Webhook
    webhook:
      clientConfig:
        service:
          namespace: cert-manager
          name: cert-manager-webhook
        caBundle: Cg==
      conversionReviewVersions: ["v1"]
`
	tests := []struct {
		name        string
		liveService string
		problems    bool
		want        string
	}{
		{"only caBundle differs", "cert-manager-webhook", false, "in-sync"},
		{"service name differs", "other-webhook", true, "outdated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newCluster(t)
			crdRelease(t, c, 1, "", crdFile("widgets.yaml", chartCRD))
			liveCRD(t, c, crdYAML("widgets", "v1*"), map[string]any{
				"strategy": "Webhook",
				"webhook": map[string]any{
					"clientConfig": map[string]any{
						"service":  map[string]any{"namespace": "cert-manager", "name": tt.liveService},
						"caBundle": "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCg==",
					},
					"conversionReviewVersions": []any{"v1"},
				},
			}, "v1")
			res, err := crdsReport(t, c)
			row := oneCRD(t, res)
			if row.Status != tt.want {
				t.Fatalf("status = %q, want %q; changes = %v\n%s", row.Status, tt.want, row.Changes, row.Diff)
			}
			wantProblems(t, err, tt.problems)
			if tt.problems {
				if !slices.Contains(row.Changes, "/spec/conversion/webhook/clientConfig/service/name") {
					t.Errorf("changes lack the service name: %v", row.Changes)
				}
				return
			}
			if len(row.Changes) != 0 || row.Diff != "" {
				t.Errorf("unexpected changes: %v\n%s", row.Changes, row.Diff)
			}
		})
	}
}

func TestCrdsTemplateSource(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	manifest := "---\n# Source: api/templates/crd.yaml\n" + crdYAML("widgets", "v1*") +
		"---\n# Source: api/templates/cm.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n"
	crdRelease(t, c, 1, manifest)
	liveCRD(t, c, crdYAML("widgets", "v1*"), nil, "v1")
	res, err := crdsReport(t, c)
	wantProblems(t, err, false)
	row := oneCRD(t, res)
	if row.Name != "widgets.example.com" || row.Source != "templates" || row.Status != "in-sync" {
		t.Fatalf("row = %+v", row)
	}
}

func TestCrdsMultiDocumentFilesSorted(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	crdRelease(t, c, 1, "",
		crdFile("bundle.yaml",
			crdYAML("widgets", "v1*"),
			"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: not-a-crd\n",
			"# only a comment\n",
			crdYAML("apples", "v1*"),
		),
		crdFile("gadgets.yml", crdYAML("gadgets", "v1*")),
		&chartcommon.File{Name: "crds/README.md", Data: []byte("not a manifest")},
	)
	for _, p := range []string{"apples", "gadgets", "widgets"} {
		liveCRD(t, c, crdYAML(p, "v1*"), nil, "v1")
	}
	res, err := crdsReport(t, c)
	wantProblems(t, err, false)
	var got []string
	for _, r := range res.CRDs {
		got = append(got, r.Name+" "+r.Source+" "+r.Status)
	}
	want := []string{
		"apples.example.com api/crds/bundle.yaml in-sync",
		"gadgets.example.com api/crds/gadgets.yml in-sync",
		"widgets.example.com api/crds/bundle.yaml in-sync",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCrdsSameNameSortedBySource(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	crdRelease(t, c, 1, "---\n# Source: api/templates/crd.yaml\n"+crdYAML("widgets", "v1*"), crdFile("widgets.yaml", crdYAML("widgets", "v1*")))
	liveCRD(t, c, crdYAML("widgets", "v1*"), nil, "v1")
	res, err := crdsReport(t, c)
	wantProblems(t, err, false)
	if len(res.CRDs) != 2 || res.CRDs[0].Source != "api/crds/widgets.yaml" || res.CRDs[1].Source != "templates" {
		t.Fatalf("rows = %+v", res.CRDs)
	}
}

func TestCrdsNone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		chart *chart.Chart
	}{
		{"chart without crds", &chart.Chart{Metadata: &chart.Metadata{Name: "api", Version: "1.0.0"}, Files: []*chartcommon.File{{Name: "README.md", Data: []byte("x")}}}},
		{"no chart in record", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newCluster(t)
			storeCRDRelease(t, c, 1, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n", tt.chart)
			out, err := run(t, c, "prod", "crds", "api")
			wantProblems(t, err, false)
			if out != "Release prod/api revision 1 ships no CRDs\n" {
				t.Errorf("table = %q", out)
			}
			out, err = run(t, c, "prod", "crds", "api", "-o", "json")
			wantProblems(t, err, false)
			var got map[string]any
			if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
				t.Fatalf("bad json: %v\n%s", jerr, out)
			}
			if crds, ok := got["crds"].([]any); !ok || len(crds) != 0 {
				t.Errorf("crds must be [], got %s", out)
			}
		})
	}
}

func TestCrdsRevision(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	crdRelease(t, c, 1, "", crdFile("widgets.yaml", crdYAML("widgets", "v1*")))
	crdRelease(t, c, 2, "", crdFile("widgets.yaml", crdYAML("widgets", "v1", "v2*")))
	liveCRD(t, c, crdYAML("widgets", "v1*"), nil, "v1")

	res, err := crdsReport(t, c)
	wantProblems(t, err, true)
	if res.Revision != 2 || oneCRD(t, res).Status != "outdated" {
		t.Fatalf("latest: %+v", res)
	}
	for _, flag := range []string{"--revision", "-r"} {
		res, err = crdsReport(t, c, flag, "1")
		wantProblems(t, err, false)
		if res.Release != "api" || res.Namespace != "prod" || res.Revision != 1 || oneCRD(t, res).Status != "in-sync" {
			t.Fatalf("%s 1: %+v", flag, res)
		}
	}
	_, err = run(t, c, "prod", "crds", "api", "--revision", "-1")
	if err == nil || errors.Is(err, ErrProblems) || !strings.Contains(err.Error(), "invalid revision -1: must not be negative") {
		t.Fatalf("negative revision: %v", err)
	}
	if _, err = run(t, c, "prod", "crds", "api", "--revision", "9"); err == nil || errors.Is(err, ErrProblems) {
		t.Fatalf("missing revision: %v", err)
	}
}

func TestCrdsParseError(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	crdRelease(t, c, 1, "", &chartcommon.File{Name: "crds/broken.yaml", Data: []byte("kind: [unclosed\n")})
	_, err := run(t, c, "prod", "crds", "api")
	if err == nil || errors.Is(err, ErrProblems) {
		t.Fatalf("want a parse error, got %v", err)
	}
}

func TestCrdsMissingName(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	crdRelease(t, c, 1, "", crdFile("nameless.yaml", "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nspec: {}\n"))
	_, err := run(t, c, "prod", "crds", "api")
	if err == nil || errors.Is(err, ErrProblems) || !strings.Contains(err.Error(), "crds/nameless.yaml") {
		t.Fatalf("want an error naming the file, got %v", err)
	}
}

func TestCrdsLookupError(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	crdRelease(t, c, 1, "", crdFile("widgets.yaml", crdYAML("widgets", "v1*")))
	liveCRD(t, c, crdYAML("widgets", "v1*"), nil, "v1")
	var out, errOut bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut, o.Now = &out, &errOut, func() time.Time { return now }
	o.NewClients = func(*Options) (*Clients, error) {
		cl := c.clients("prod")
		cl.Cluster.Dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "customresourcedefinitions", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("customresourcedefinitions is forbidden")
		})
		return cl, nil
	}
	cmd := NewRootCommand(o)
	cmd.SetArgs([]string{"crds", "api", "-o", "json"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || errors.Is(err, ErrProblems) || !strings.Contains(err.Error(), "widgets.example.com") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("want a lookup error naming the CRD, got %v", err)
	}
	var res crdsJSON
	if jerr := json.Unmarshal(out.Bytes(), &res); jerr != nil {
		t.Fatalf("bad json: %v\n%s", jerr, out.String())
	}
	row := oneCRD(t, res)
	if row.Status != "error" || !strings.Contains(row.Error, "forbidden") {
		t.Fatalf("row = %+v", row)
	}
}

func TestCrdsJSONKeys(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	crdRelease(t, c, 1, "", crdFile("widgets.yaml", crdYAML("widgets", "v1", "v2*")))
	liveCRD(t, c, crdYAML("widgets", "v1*"), map[string]any{
		"strategy": "Webhook",
		"webhook":  map[string]any{"clientConfig": map[string]any{"service": map[string]any{"namespace": "ns", "name": "conv"}}},
	}, "v1")
	out, err := run(t, c, "prod", "crds", "api", "-o", "json")
	wantProblems(t, err, true)
	var got map[string]any
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("bad json: %v\n%s", jerr, out)
	}
	if len(got) != 4 || got["release"] != "api" || got["namespace"] != "prod" || got["revision"] != float64(1) {
		t.Fatalf("top level = %v", got)
	}
	crds, _ := got["crds"].([]any)
	if len(crds) != 1 {
		t.Fatalf("crds = %v", got["crds"])
	}
	row, _ := crds[0].(map[string]any)
	for _, k := range []string{"name", "source", "status", "chartVersions", "clusterVersions", "storedVersions", "conversion", "webhookService", "changes", "diff", "notes"} {
		if _, ok := row[k]; !ok {
			t.Errorf("row lacks %q: %v", k, row)
		}
	}
	if _, ok := row["error"]; ok {
		t.Errorf("error must be omitted when empty: %v", row)
	}
	versions, _ := row["chartVersions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("chartVersions = %v", row["chartVersions"])
	}
	v, _ := versions[1].(map[string]any)
	if len(v) != 3 || v["name"] != "v2" || v["served"] != true || v["storage"] != true {
		t.Errorf("version = %v", v)
	}

	// Optional fields are omitted on an in-sync row with no webhook.
	c = newCluster(t)
	crdRelease(t, c, 1, "", crdFile("widgets.yaml", crdYAML("widgets", "v1*")))
	liveCRD(t, c, crdYAML("widgets", "v1*"), nil, "v1")
	out, err = run(t, c, "prod", "crds", "api", "-o", "json")
	wantProblems(t, err, false)
	got = nil
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil {
		t.Fatalf("bad json: %v\n%s", jerr, out)
	}
	row = got["crds"].([]any)[0].(map[string]any)
	for _, k := range []string{"webhookService", "changes", "diff", "notes", "error"} {
		if _, ok := row[k]; ok {
			t.Errorf("%q must be omitted: %v", k, row)
		}
	}
}

func TestCrdsTable(t *testing.T) {
	t.Parallel()
	c := newCluster(t)
	manifest := "---\n# Source: api/templates/things.yaml\n" + crdYAML("things", "v1*") +
		"---\n# Source: api/templates/cm.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n"
	crdRelease(t, c, 3, manifest,
		crdFile("widgets.yaml", crdYAML("widgets", "v1", "v2*")),
		crdFile("gadgets.yaml", crdYAML("gadgets", "(v1beta1)", "v1*")),
		crdFile("certs.yaml", crdYAML("certs", "v1*")),
	)
	liveCRD(t, c, crdYAML("widgets", "v1*"), nil, "v1")
	liveCRD(t, c, crdYAML("gadgets", "(v1beta1)", "v1*"), nil, "v1beta1", "v1")
	liveCRD(t, c, crdYAML("certs", "v1*"), map[string]any{
		"strategy": "Webhook",
		"webhook":  map[string]any{"clientConfig": map[string]any{"service": map[string]any{"namespace": "cert-manager", "name": "cert-manager-webhook"}}},
	}, "v1")
	out, err := run(t, c, "prod", "crds", "api")
	wantProblems(t, err, true)
	golden(t, out)
}
