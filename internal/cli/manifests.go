package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	releaseutil "helm.sh/helm/v4/pkg/release/v1/util"
	"sigs.k8s.io/yaml"
)

const secretManifestWarning = "Warning: manifest contains Secret objects; their data is only base64-encoded, handle the output with care"

func newManifestsCommand(o *Options) *cobra.Command {
	var revision int
	var hooks bool
	var outputDir string
	cmd := &cobra.Command{
		Use:   "manifests RELEASE",
		Short: "Show the stored rendered manifest of a release revision",
		Long: `Show the rendered manifest stored in a release revision, newest revision by default,
like helm get manifest. This is what Helm stored at install or upgrade time, not the live
state of the cluster: use drift for that. Nothing is changed. Output is always YAML, -o is ignored.

With --hooks the hook manifests are appended, like helm get hooks. With --output-dir every
YAML document is written to its own file named <kind>-<namespace>-<name>.yaml (mode 0600,
the namespace is left out when the document has none) and the written paths are listed.

Manifests may contain Secrets: a warning is printed to stderr and content is never logged.`,
		Example: `  helm-debug manifests backend
  helm-debug manifests backend --revision 3 --hooks --output-dir ./backend-r3`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if revision < 0 {
				return fmt.Errorf("invalid revision %d: must not be negative", revision)
			}
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			rel, err := c.Store.Revision(args[0], c.Namespace, revision)
			if err != nil {
				return err
			}
			var docs []string
			for _, d := range sortedDocs(rel.Manifest) {
				docs = append(docs, withNewline(d))
			}
			stdout := withNewline(rel.Manifest)
			if hooks {
				for _, hk := range rel.Hooks {
					h := withNewline("# Source: " + hk.Path + "\n" + hk.Manifest)
					docs = append(docs, h)
					stdout += "---\n" + h
				}
			}
			for _, d := range docs {
				if docHead(d).Kind == "Secret" {
					fmt.Fprintln(o.ErrOut, secretManifestWarning)
					break
				}
			}
			if outputDir == "" {
				_, err = fmt.Fprint(o.Out, stdout)
				return err
			}
			return writeDocs(o.Out, outputDir, docs)
		},
	}
	cmd.Flags().IntVarP(&revision, "revision", "r", 0, "Release revision to inspect (default: latest)")
	cmd.Flags().BoolVar(&hooks, "hooks", false, "Also show the hook manifests")
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Write one file per YAML document (mode 0600) into this directory instead of stdout")
	return cmd
}

func sortedDocs(manifest string) []string {
	split := releaseutil.SplitManifests(manifest)
	keys := make([]string, 0, len(split))
	for k := range split {
		keys = append(keys, k)
	}
	sort.Sort(releaseutil.BySplitManifestsOrder(keys))
	docs := make([]string, len(keys))
	for i, k := range keys {
		docs[i] = split[k]
	}
	return docs
}

// withNewline keeps empty input empty so an empty manifest produces no output.
func withNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

type manifestHead struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

// docHead returns the zero value for a document that is not parseable YAML.
func docHead(doc string) manifestHead {
	var h manifestHead
	if err := yaml.Unmarshal([]byte(doc), &h); err != nil {
		return manifestHead{}
	}
	return h
}

func writeDocs(out io.Writer, dir string, docs []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	used := map[string]bool{}
	for i, d := range docs {
		base := docFileName(d, i+1)
		// Sanitizing and lowercasing can map distinct objects (or a hook and an object) to one name.
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d.yaml", strings.TrimSuffix(base, ".yaml"), n)
		}
		used[name] = true
		path := filepath.Join(dir, name)
		if err := writePrivateFile(path, []byte(d)); err != nil {
			return err
		}
		fmt.Fprintln(out, path)
	}
	return nil
}

// docFileName builds the name from untrusted manifest data, so every component is reduced to [a-z0-9._-]
// and the result can never contain a path separator.
func docFileName(doc string, pos int) string {
	h := docHead(doc)
	if h.Kind == "" || h.Metadata.Name == "" {
		return fmt.Sprintf("document-%d.yaml", pos)
	}
	parts := []string{h.Kind}
	if h.Metadata.Namespace != "" {
		parts = append(parts, h.Metadata.Namespace)
	}
	parts = append(parts, h.Metadata.Name)
	for i, p := range parts {
		parts[i] = sanitizeName(p)
	}
	return strings.Join(parts, "-") + ".yaml"
}

func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, strings.ToLower(s))
}
