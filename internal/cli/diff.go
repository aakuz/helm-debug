package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	releaseutil "helm.sh/helm/v4/pkg/release/v1/util"
	"sigs.k8s.io/yaml"
)

const (
	diffAdded     = "added"
	diffRemoved   = "removed"
	diffChanged   = "changed"
	diffUnchanged = "unchanged"
	diffContext   = 3
)

type diffObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Diff       string `json:"diff"`
}

type diffValues struct {
	Status string `json:"status"`
	Diff   string `json:"diff,omitempty"`
}

type diffResult struct {
	Release   string       `json:"release"`
	Namespace string       `json:"namespace"`
	From      int          `json:"from"`
	To        int          `json:"to"`
	Manifest  []diffObject `json:"manifest"`
	Values    diffValues   `json:"values"`
}

func newDiffCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "diff RELEASE REV1 REV2",
		Short: "Compare manifests and user values between two revisions",
		Long: `Compare the stored manifests and user-supplied values of two revisions.
Objects are matched by apiVersion, kind, namespace and name and reported as
added, removed or changed. Hooks are excluded. Secret data and stringData
values are masked: a changed value is shown as <masked:old> and <masked:new>.
User values are shown as stored. Exits with code 0 whether or not the
revisions differ.`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			from, err := parseRevision(args[1])
			if err != nil {
				return err
			}
			to, err := parseRevision(args[2])
			if err != nil {
				return err
			}
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			a, err := c.Store.Revision(args[0], c.Namespace, from)
			if err != nil {
				return err
			}
			b, err := c.Store.Revision(args[0], c.Namespace, to)
			if err != nil {
				return err
			}
			result := &diffResult{Release: b.Name, Namespace: b.Namespace, From: a.Version, To: b.Version}
			if result.Manifest, err = diffManifests(a.Manifest, b.Manifest); err != nil {
				return err
			}
			oldValues, err := yaml.Marshal(a.Config)
			if err != nil {
				return err
			}
			newValues, err := yaml.Marshal(b.Config)
			if err != nil {
				return err
			}
			result.Values = diffValues{Status: diffUnchanged}
			if d := unifiedDiff(string(oldValues), string(newValues)); d != "" {
				result.Values = diffValues{Status: diffChanged, Diff: d}
			}
			return o.print(result, func(w io.Writer) { printDiff(w, result) })
		},
	}
}

func parseRevision(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid revision %q: must be a positive integer", s)
	}
	return n, nil
}

type objectKey struct{ apiVersion, kind, namespace, name string }

func parseObjects(manifest string) (map[objectKey]map[string]any, error) {
	objects := map[objectKey]map[string]any{}
	docs := releaseutil.SplitManifests(manifest)
	names := make([]string, 0, len(docs))
	for name := range docs {
		names = append(names, name)
	}
	sort.Sort(releaseutil.BySplitManifestsOrder(names))
	for i, name := range names {
		var obj map[string]any
		// The parser error is dropped on purpose: it can quote Secret content.
		if err := yaml.Unmarshal([]byte(docs[name]), &obj); err != nil {
			return nil, fmt.Errorf("parsing release manifest: invalid YAML in document %d", i+1)
		}
		key := objectKey{apiVersion: str(obj["apiVersion"]), kind: str(obj["kind"])}
		if md, ok := obj["metadata"].(map[string]any); ok {
			key.namespace, key.name = str(md["namespace"]), str(md["name"])
		}
		if key.apiVersion == "" || key.kind == "" || key.name == "" {
			return nil, fmt.Errorf("release manifest object is missing apiVersion, kind or metadata.name")
		}
		objects[key] = obj
	}
	return objects, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func diffManifests(oldManifest, newManifest string) ([]diffObject, error) {
	oldObjects, err := parseObjects(oldManifest)
	if err != nil {
		return nil, err
	}
	newObjects, err := parseObjects(newManifest)
	if err != nil {
		return nil, err
	}
	keys := map[objectKey]bool{}
	for k := range oldObjects {
		keys[k] = true
	}
	for k := range newObjects {
		keys[k] = true
	}
	sorted := make([]objectKey, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		if a.namespace != b.namespace {
			return a.namespace < b.namespace
		}
		if a.name != b.name {
			return a.name < b.name
		}
		return a.apiVersion < b.apiVersion
	})
	rows := []diffObject{}
	for _, k := range sorted {
		oldObj, hadOld := oldObjects[k]
		newObj, hadNew := newObjects[k]
		if k.kind == "Secret" {
			maskSecret(oldObj, newObj)
		}
		oldText, newText := "", ""
		if hadOld {
			b, err := yaml.Marshal(oldObj)
			if err != nil {
				return nil, err
			}
			oldText = string(b)
		}
		if hadNew {
			b, err := yaml.Marshal(newObj)
			if err != nil {
				return nil, err
			}
			newText = string(b)
		}
		row := diffObject{APIVersion: k.apiVersion, Kind: k.kind, Namespace: k.namespace, Name: k.name}
		switch {
		case !hadOld:
			row.Status = diffAdded
		case !hadNew:
			row.Status = diffRemoved
		case oldText != newText:
			row.Status = diffChanged
		default:
			continue
		}
		row.Diff = unifiedDiff(oldText, newText)
		rows = append(rows, row)
	}
	return rows, nil
}

// maskSecret replaces every data/stringData value in both objects (either may
// be nil). A value that differs between the two becomes <masked:old> or
// <masked:new>, any other value becomes <masked>, so no secret material is
// printed but a change is still visible.
func maskSecret(oldObj, newObj map[string]any) {
	oldEff, newEff := effectiveSecretData(oldObj), effectiveSecretData(newObj)
	changed := map[string]bool{}
	for k, ov := range oldEff {
		if nv, ok := newEff[k]; ok && nv != ov {
			changed[k] = true
		}
	}
	for _, section := range []string{"data", "stringData"} {
		oldData, _ := oldObj[section].(map[string]any)
		newData, _ := newObj[section].(map[string]any)
		for k := range oldData {
			oldData[k] = "<masked>"
			if changed[k] {
				oldData[k] = "<masked:old>"
			}
		}
		for k := range newData {
			newData[k] = "<masked>"
			if changed[k] {
				newData[k] = "<masked:new>"
			}
		}
	}
}

// effectiveSecretData returns the base64 payload per key as the API would
// store it: stringData entries override data entries.
func effectiveSecretData(obj map[string]any) map[string]string {
	eff := map[string]string{}
	data, _ := obj["data"].(map[string]any)
	for k, v := range data {
		eff[k] = fmt.Sprint(v)
	}
	stringData, _ := obj["stringData"].(map[string]any)
	for k, v := range stringData {
		eff[k] = base64.StdEncoding.EncodeToString([]byte(fmt.Sprint(v)))
	}
	return eff
}

type diffLine struct {
	op   byte // ' ', '-' or '+'
	text string
}

// unifiedDiff returns a unified-style diff of two texts with hunk headers, or
// "" when they are equal.
func unifiedDiff(oldText, newText string) string {
	fullA, fullB := splitLines(oldText), splitLines(newText)
	// Trim the common prefix and suffix so the LCS table only covers the middle.
	prefix := 0
	for prefix < len(fullA) && prefix < len(fullB) && fullA[prefix] == fullB[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(fullA)-prefix && suffix < len(fullB)-prefix && fullA[len(fullA)-1-suffix] == fullB[len(fullB)-1-suffix] {
		suffix++
	}
	a, b := fullA[prefix:len(fullA)-suffix], fullB[prefix:len(fullB)-suffix]
	// Longest common subsequence table.
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	lines := make([]diffLine, 0, len(fullA)+len(fullB)-prefix-suffix)
	for _, t := range fullA[:prefix] {
		lines = append(lines, diffLine{' ', t})
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			lines = append(lines, diffLine{' ', a[i]})
			i++
			j++
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]):
			lines = append(lines, diffLine{'-', a[i]})
			i++
		default:
			lines = append(lines, diffLine{'+', b[j]})
			j++
		}
	}
	for _, t := range fullA[len(fullA)-suffix:] {
		lines = append(lines, diffLine{' ', t})
	}
	var out strings.Builder
	for start := 0; start < len(lines); {
		// Find the next change, then extend the hunk while changes are close together.
		first := start
		for first < len(lines) && lines[first].op == ' ' {
			first++
		}
		if first == len(lines) {
			break
		}
		hunkStart := max(first-diffContext, start)
		last := first
		for end := first; end < len(lines); end++ {
			if lines[end].op != ' ' {
				last = end
			} else if end-last > 2*diffContext {
				break
			}
		}
		hunkEnd := min(last+diffContext+1, len(lines))
		oldStart, newStart := 1, 1
		for _, l := range lines[:hunkStart] {
			if l.op != '+' {
				oldStart++
			}
			if l.op != '-' {
				newStart++
			}
		}
		var oldN, newN int
		var body strings.Builder
		for _, l := range lines[hunkStart:hunkEnd] {
			if l.op != '+' {
				oldN++
			}
			if l.op != '-' {
				newN++
			}
			body.WriteByte(l.op)
			body.WriteString(l.text)
			body.WriteByte('\n')
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n%s", hunkRange(oldStart, oldN), hunkRange(newStart, newN), body.String())
		start = hunkEnd
	}
	return out.String()
}

func hunkRange(start, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d,0", start-1)
	}
	return fmt.Sprintf("%d,%d", start, n)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func printDiff(w io.Writer, result *diffResult) {
	fmt.Fprintf(w, "Release %s/%s revisions %d -> %d\n", result.Namespace, result.Release, result.From, result.To)
	if len(result.Manifest) == 0 && result.Values.Status == diffUnchanged {
		fmt.Fprintln(w, "\nNo differences.")
		return
	}
	fmt.Fprintln(w, "\nManifest:")
	if len(result.Manifest) == 0 {
		fmt.Fprintln(w, "  No differences.")
	} else {
		t := newTable(w)
		t.add("KIND", "NAMESPACE", "NAME", "STATUS")
		for _, row := range result.Manifest {
			t.add(row.Kind, row.Namespace, row.Name, row.Status)
		}
		t.flush()
		for _, row := range result.Manifest {
			ns := ""
			if row.Namespace != "" {
				ns = row.Namespace + "/"
			}
			fmt.Fprintf(w, "\n--- %s %s%s (%s)\n%s", row.Kind, ns, row.Name, row.Status, row.Diff)
		}
	}
	fmt.Fprintln(w, "\nValues:")
	if result.Values.Status == diffUnchanged {
		fmt.Fprintln(w, "  No differences.")
		return
	}
	fmt.Fprint(w, result.Values.Diff)
}
