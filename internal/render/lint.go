package render

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	chart "helm.sh/helm/v4/pkg/chart/v2"
	"sigs.k8s.io/yaml"
)

// contextLines is how many rendered lines are shown on each side of the failing one.
const contextLines = 5

// Finding is one problem in the rendered chart.
type Finding struct {
	File string `json:"file"`
	// Line is the failing line of the rendered template output, 0 when unknown.
	Line    int    `json:"line,omitempty"`
	Error   string `json:"error"`
	Context []Line `json:"context,omitempty"`
}

// Line is a line of rendered template output.
type Line struct {
	Number  int    `json:"number"`
	Text    string `json:"text"`
	Failing bool   `json:"failing,omitempty"`
}

// Lint renders ch and reports every template that fails to render, every rendered document that is not valid YAML
// and every document with an empty apiVersion, kind or metadata.name. As in helm lint, only .yaml and .yml output
// is checked as manifests, so NOTES.txt and other text templates are skipped. The error is returned only when the chart cannot be rendered at all, for example when the values do not
// match the chart's schema.
func Lint(ch *chart.Chart, o Options) ([]Finding, error) {
	out, failed, err := Render(ch, o)
	if err != nil {
		return nil, err
	}
	findings := []Finding{}
	for _, f := range failed {
		findings = append(findings, Finding{File: f.File, Error: f.Err.Error()})
	}
	for _, file := range slices.Sorted(maps.Keys(out)) {
		if ext := path.Ext(file); ext != ".yaml" && ext != ".yml" {
			continue
		}
		findings = append(findings, lintFile(file, out[file], o.ShowSecrets)...)
	}
	return findings, nil
}

// yamlLine finds the line number in a go-yaml error such as "yaml: line 7: did not find expected key".
var yamlLine = regexp.MustCompile(`line (\d+):`)

func lintFile(file, rendered string, showSecrets bool) []Finding {
	all := strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	docs := splitDocs(all)
	// Context lines come from shown, the documents are parsed from all.
	shown := all
	if !showSecrets {
		shown = maskSecrets(all, docs)
	}
	var findings []Finding
	for _, d := range docs {
		text := strings.Join(all[d.from:d.to], "\n")
		var obj map[string]any
		if err := yaml.Unmarshal([]byte(text), &obj); err != nil {
			f := Finding{File: file, Error: err.Error()}
			if m := yamlLine.FindStringSubmatchIndex(f.Error); m != nil {
				n, _ := strconv.Atoi(f.Error[m[2]:m[3]])
				f.Line = d.from + n
				f.Context = context(shown, f.Line)
				// The parser counts lines from the start of the document; make the message agree with Line.
				f.Error = f.Error[:m[0]] + fmt.Sprintf("line %d:", f.Line) + f.Error[m[1]:]
			}
			findings = append(findings, f)
			continue
		}
		for _, field := range emptyFields(obj) {
			n := d.from + fieldLine(all[d.from:d.to], field)
			findings = append(findings, Finding{File: file, Line: n, Error: field + " is empty", Context: context(shown, n)})
		}
	}
	return findings
}

// emptyFields lists the identifying fields an object lacks. Helm reports these later with an unrelated error
// (helm/helm#8847), usually after a misspelled value rendered as an empty string. A document with no content is
// skipped, as Helm does.
func emptyFields(obj map[string]any) []string {
	if obj == nil {
		return nil
	}
	var empty []string
	for _, k := range []string{"apiVersion", "kind"} {
		if s, _ := obj[k].(string); s == "" {
			empty = append(empty, k)
		}
	}
	kind, _ := obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	generateName, _ := meta["generateName"].(string)
	if !strings.HasSuffix(kind, "List") && name == "" && generateName == "" {
		empty = append(empty, "metadata.name")
	}
	return empty
}

// fieldLine finds the 1-based line of field in a document, or its first content line when the key is absent.
func fieldLine(lines []string, field string) int {
	inMeta := false
	// childIndent is the indentation of metadata's keys, so a "name:" nested deeper (in labels) is not taken.
	childIndent := -1
	first := 0
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if first == 0 {
			first = i + 1
		}
		indent := len(l) - len(strings.TrimLeft(l, " "))
		top := indent == 0
		switch {
		case field == "metadata.name" && top:
			inMeta, childIndent = strings.HasPrefix(l, "metadata:"), -1
		case field == "metadata.name" && inMeta:
			if childIndent < 0 {
				childIndent = indent
			}
			if indent == childIndent && strings.HasPrefix(trimmed, "name:") {
				return i + 1
			}
		}
		if top && strings.HasPrefix(l, field+":") {
			return i + 1
		}
	}
	return max(first, 1)
}

var secretKind = regexp.MustCompile(`^kind:\s*["']?Secret["']?\s*(#.*)?$`)

// maskSecrets returns a copy of all in which the values under data and stringData of every Secret document are
// replaced by <masked>. It works on lines, because the document being reported may not parse.
func maskSecrets(all []string, docs []doc) []string {
	out := slices.Clone(all)
	for _, d := range docs {
		if !slices.ContainsFunc(all[d.from:d.to], secretKind.MatchString) {
			continue
		}
		inData := false
		childIndent := -1
		for i := d.from; i < d.to; i++ {
			l := all[i]
			trimmed := strings.TrimSpace(l)
			indent := len(l) - len(strings.TrimLeft(l, " "))
			if trimmed == "" {
				continue
			}
			if indent == 0 {
				key, value, _ := strings.Cut(l, ":")
				inData, childIndent = key == "data" || key == "stringData", -1
				if inData && strings.TrimSpace(value) != "" {
					out[i] = key + ": <masked>"
				}
				continue
			}
			if !inData {
				continue
			}
			if childIndent < 0 {
				childIndent = indent
			}
			// A key indented less than its siblings is usually the broken entry itself, so it is masked the same way.
			if key, _, ok := strings.Cut(trimmed, ":"); ok && indent <= childIndent {
				out[i] = l[:indent] + key + ": <masked>"
			} else {
				out[i] = l[:indent] + "<masked>"
			}
		}
	}
	return out
}

// doc is a YAML document as a half-open range of 0-based line indexes.
type doc struct{ from, to int }

func splitDocs(all []string) []doc {
	var docs []doc
	from := 0
	for i, l := range all {
		if isSeparator(l) {
			docs = append(docs, doc{from, i})
			from = i + 1
		}
	}
	return append(docs, doc{from, len(all)})
}

func isSeparator(l string) bool {
	l = strings.TrimRight(l, " \t\r")
	return l == "---" || strings.HasPrefix(l, "--- ")
}

// context returns the lines around the 1-based line n.
func context(all []string, n int) []Line {
	var out []Line
	for i := max(1, n-contextLines); i <= min(len(all), n+contextLines); i++ {
		out = append(out, Line{Number: i, Text: all[i-1], Failing: i == n})
	}
	return out
}
