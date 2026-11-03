package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeChart writes a chart named demo with the given values.yaml and name/content template pairs into a temp dir.
func writeChart(t *testing.T, values string, templates ...string) string {
	t.Helper()
	dir := t.TempDir()
	files := append([]string{"Chart.yaml", "apiVersion: v2\nname: demo\nversion: 0.1.0\n", "values.yaml", values}, templates...)
	for i := 0; i < len(files); i += 2 {
		path := filepath.Join(dir, files[i])
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(files[i+1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const lintConfigMap = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-{{ .Release.Namespace }}
data:
  a: {{ .Values.a }}
`

func TestLintClean(t *testing.T) {
	t.Parallel()
	dir := writeChart(t, "a: b\n", "templates/cm.yaml", lintConfigMap)
	out, err := run(t, nil, "", "lint", dir)
	if err != nil {
		t.Fatal(err)
	}
	if out != "No problems found in chart demo\n" {
		t.Fatalf("output:\n%s", out)
	}
}

func TestLintTable(t *testing.T) {
	t.Parallel()
	dir := writeChart(t, "a: b\n",
		"templates/cm.yaml", lintConfigMap+" bad: {{ .Values.a }}\n",
		"templates/req.yaml", `{{ required "image.tag is required" .Values.image }}`,
	)
	out, err := run(t, nil, "", "lint", dir)
	wantProblems(t, err, true)
	golden(t, out)
}

func TestLintJSONUsesValuesReleaseAndNamespace(t *testing.T) {
	t.Parallel()
	dir := writeChart(t, "a: b\n", "templates/cm.yaml", lintConfigMap+"{{ if .Values.broken }} bad: x{{ end }}\n")
	vals := filepath.Join(t.TempDir(), "v.yaml")
	if err := os.WriteFile(vals, []byte("broken: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, nil, "", "lint", dir, "-f", vals, "--set", "a=c", "--release-name", "rel", "-n", "prod", "-o", "json")
	wantProblems(t, err, true)
	var res struct {
		Chart    string `json:"chart"`
		Findings []struct {
			File    string `json:"file"`
			Line    int    `json:"line"`
			Error   string `json:"error"`
			Context []struct {
				Number  int    `json:"number"`
				Text    string `json:"text"`
				Failing bool   `json:"failing"`
			} `json:"context"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if res.Chart != "demo" || len(res.Findings) != 1 {
		t.Fatalf("result: %+v", res)
	}
	f := res.Findings[0]
	if f.File != "demo/templates/cm.yaml" || f.Line != 6 || !f.Context[5].Failing {
		t.Fatalf("finding: %+v", f)
	}
	if f.Context[3].Text != "  name: rel-prod" || f.Context[5].Text != "  a: c" {
		t.Fatalf("release name, namespace and --set must reach the render: %+v", f.Context)
	}
}

func TestLintExecutionErrors(t *testing.T) {
	t.Parallel()
	dir := writeChart(t, "", "templates/cm.yaml", lintConfigMap)
	for name, args := range map[string][]string{
		"missing chart":       {"lint", filepath.Join(dir, "nope")},
		"missing values file": {"lint", dir, "-f", filepath.Join(dir, "nope.yaml")},
		"bad --set":           {"lint", dir, "--set", "a"},
	} {
		_, err := run(t, nil, "", args...)
		if err == nil || strings.Contains(err.Error(), ErrProblems.Error()) {
			t.Errorf("%s: want an execution error, got %v", name, err)
		}
	}
}
