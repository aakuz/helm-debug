package render

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
)

// testChart builds an in-memory chart from template name/content pairs.
func testChart(name string, values map[string]any, templates ...string) *chart.Chart {
	ch := &chart.Chart{Metadata: &chart.Metadata{APIVersion: "v2", Name: name, Version: "0.1.0"}, Values: values}
	for i := 0; i < len(templates); i += 2 {
		ch.Templates = append(ch.Templates, &common.File{Name: templates[i], Data: []byte(templates[i+1])})
	}
	return ch
}

func lines(from int, failing int, texts ...string) []Line {
	out := make([]Line, len(texts))
	for i, s := range texts {
		out[i] = Line{Number: from + i, Text: s, Failing: from+i == failing}
	}
	return out
}

func TestLintInvalidYAMLShowsRenderedContext(t *testing.T) {
	t.Parallel()
	ch := testChart("app", nil, "templates/cm.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: one
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: two
data:
  a: b
 bad: indent
  c: d
`)
	got, err := Lint(ch, Options{ReleaseName: "rel", Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %+v", got)
	}
	f := got[0]
	// go-yaml reports the line where the broken mapping started (the same line helm names), so the bad line is
	// in the context right after it.
	if f.File != "app/templates/cm.yaml" || f.Line != 11 {
		t.Fatalf("file/line = %s:%d, want app/templates/cm.yaml:11", f.File, f.Line)
	}
	if f.Error != "error converting YAML to JSON: yaml: line 6: did not find expected key" {
		t.Fatalf("error = %q", f.Error)
	}
	want := lines(6, 11, "apiVersion: v1", "kind: ConfigMap", "metadata:", "  name: two", "data:", "  a: b", " bad: indent", "  c: d")
	if !reflect.DeepEqual(f.Context, want) {
		t.Fatalf("context:\n got %+v\nwant %+v", f.Context, want)
	}
}

func TestLintReportsEveryRenderErrorAndChecksTheRest(t *testing.T) {
	t.Parallel()
	ch := testChart("app", map[string]any{"name": "x"},
		"templates/_helpers.tpl", `{{ define "app.name" }}{{ .Values.name }}{{ end }}`,
		"templates/a.yaml", `{{ required "image.tag is required" .Values.image }}`,
		"templates/b.yaml", `{{ .Values.missing.deep }}`,
		// c.yaml defines a block used by d.yaml, so it must stay parsed while d.yaml renders on its own.
		"templates/c.yaml", `{{ define "app.extra" }}extra{{ end }}{{ fail "c is broken" }}`,
		"templates/d.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ include \"app.name\" . }}-{{ include \"app.extra\" . }}\n bad: x\n",
	)
	got, err := Lint(ch, Options{ReleaseName: "rel", Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, f := range got {
		files = append(files, f.File)
	}
	want := []string{"app/templates/a.yaml", "app/templates/b.yaml", "app/templates/c.yaml", "app/templates/d.yaml"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %v, want %v\n%+v", files, want, got)
	}
	for i, sub := range []string{"image.tag is required", "nil pointer evaluating interface {}.deep", "c is broken"} {
		if !strings.Contains(got[i].Error, sub) || !strings.Contains(got[i].Error, want[i]) {
			t.Errorf("%s: error %q must name the template and contain %q", want[i], got[i].Error, sub)
		}
	}
	if got[3].Line != 4 || got[3].Context[3].Text != "  name: x-extra" {
		t.Errorf("d.yaml must be rendered with blocks from c.yaml and checked: %+v", got[3])
	}
}

func TestLintFlagsEmptyRequiredFields(t *testing.T) {
	t.Parallel()
	ch := testChart("app", nil,
		// A misspelled value renders an empty name (helm/helm#8847).
		"templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  labels:\n    name: b\n  name: {{ .Values.nam }}\n",
		"templates/nokind.yaml", "# only a comment\n---\napiVersion: v1\nmetadata:\n  name: x\n",
		"templates/ok.yaml", "apiVersion: v1\nkind: Pod\nmetadata:\n  generateName: x-\n---\napiVersion: v1\nkind: List\nitems: []\n",
		"templates/empty.yaml", "{{- if false }}\nkind: Pod\n{{- end }}\n",
	)
	got, err := Lint(ch, Options{ReleaseName: "rel", Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Finding{
		{File: "app/templates/cm.yaml", Line: 6, Error: "metadata.name is empty",
			Context: lines(1, 6, "apiVersion: v1", "kind: ConfigMap", "metadata:", "  labels:", "    name: b", "  name: ")},
		{File: "app/templates/nokind.yaml", Line: 3, Error: "kind is empty",
			Context: lines(1, 3, "# only a comment", "---", "apiVersion: v1", "metadata:", "  name: x")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestLintMasksSecretValuesInContext(t *testing.T) {
	t.Parallel()
	secret := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\ndata:\n  password: aHVudGVy\nstringData:\n  key: |\n    -----BEGIN KEY-----\n    c2VjcmV0\n  token: \"abc\"\n bad: x\n"
	ch := testChart("app", nil, "templates/secret.yaml", secret)

	got, err := Lint(ch, Options{ReleaseName: "rel", Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %+v", got)
	}
	var texts []string
	for _, l := range got[0].Context {
		texts = append(texts, l.Text)
	}
	want := []string{"  password: <masked>", "stringData:", "  key: <masked>", "    <masked>", "    <masked>", "  token: <masked>", " bad: <masked>"}
	if !reflect.DeepEqual(texts, want) {
		t.Fatalf("context:\n got %q\nwant %q", texts, want)
	}

	shown, err := Lint(ch, Options{ReleaseName: "rel", Namespace: "default", ShowSecrets: true})
	if err != nil {
		t.Fatal(err)
	}
	if s := shown[0].Context[0].Text; s != "  password: aHVudGVy" {
		t.Fatalf("--show-secrets must keep values, got %q", s)
	}
}

func TestValuesMergesFilesThenSets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")
	if err := os.WriteFile(a, []byte("image:\n  repo: x\n  tag: \"1\"\nkeep: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("image:\n  tag: \"2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Values([]string{a, b}, []string{"image.tag=3,n=1", "extra=y"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"image": map[string]any{"repo": "x", "tag": 3}, "keep": true, "n": 1, "extra": "y"}
	// strvals picks the integer type; only the values matter to the template engine.
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	if _, err := Values([]string{filepath.Join(dir, "nope.yaml")}, nil); err == nil {
		t.Fatal("a missing values file must fail")
	}
	if _, err := Values(nil, []string{"novalue"}); err == nil {
		t.Fatal("a --set without = must fail")
	}
}

func TestLintRendersSubchartTemplatesOnTheirOwn(t *testing.T) {
	t.Parallel()
	sub := testChart("db", map[string]any{"port": 5432},
		"templates/broken.yaml", `{{ fail "db is broken" }}`,
		"templates/svc.yaml", "apiVersion: v1\nkind: Service\nmetadata:\n  name: db\nspec:\n  port: {{ .Values.port }}\n x: y\n",
	)
	ch := testChart("app", nil, "templates/broken.yaml", `{{ fail "app is broken" }}`)
	ch.SetDependencies(sub)
	got, err := Lint(ch, Options{ReleaseName: "rel", Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	var summary []string
	for _, f := range got {
		summary = append(summary, fmt.Sprintf("%s:%d", f.File, f.Line))
	}
	want := []string{"app/charts/db/templates/broken.yaml:0", "app/templates/broken.yaml:0", "app/charts/db/templates/svc.yaml:6"}
	if !reflect.DeepEqual(summary, want) {
		t.Fatalf("got %v, want %v\n%+v", summary, want, got)
	}
	if got[2].Context[5].Text != "  port: 5432" {
		t.Fatalf("subchart values must reach the subchart template: %+v", got[2].Context)
	}
	if len(ch.Dependencies()[0].Templates) != 2 || ch.Dependencies()[0].Templates[0].Name != "templates/broken.yaml" {
		t.Fatal("Lint must not modify the chart it is given")
	}
}
