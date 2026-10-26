package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
)

const secretWarning = "Warning: manifest contains Secret objects; their data is only base64-encoded, handle the output with care"

const (
	mfV1 = "# Source: api/templates/cm.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: old\n  namespace: prod\n"
	mfV2 = "---\n# Source: api/templates/cm.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: Cfg\n  namespace: prod\n" +
		"---\n# Source: api/templates/role.yaml\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: reader\n"
	secretDoc = "# Source: api/templates/secret.yaml\napiVersion: v1\nkind: Secret\nmetadata:\n  name: creds\n  namespace: prod\ndata:\n  p: aHVudGVyMg==\n"
)

func runManifests(t *testing.T, c *cluster, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut, o.Now = &out, &errOut, func() time.Time { return now }
	o.NewClients = func(*Options) (*Clients, error) { return c.clients("prod"), nil }
	cmd := NewRootCommand(o)
	cmd.SetArgs(append([]string{"manifests"}, args...))
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func manifestCluster(t *testing.T, manifest string, hooks ...*rspb.Hook) *cluster {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusSuperseded, mfV1)
	c.release("api", "prod", 2, common.StatusDeployed, manifest, hooks...)
	return c
}

func testHook() *rspb.Hook {
	return &rspb.Hook{Name: "migrate", Kind: "Job", Path: "api/templates/job.yaml", Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: migrate\n"}
}

func TestManifestsStdout(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"latest", []string{"api"}, mfV2},
		{"revision 1", []string{"api", "--revision", "1"}, mfV1},
		{"short revision", []string{"api", "-r", "1"}, mfV1},
		{"hooks", []string{"api", "--hooks"}, mfV2 + "---\n# Source: api/templates/job.yaml\n" + testHook().Manifest},
		{"no hooks by default", []string{"api"}, mfV2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, errOut, err := runManifests(t, manifestCluster(t, mfV2, testHook()), tt.args...)
			if err != nil {
				t.Fatal(err)
			}
			if out != tt.want {
				t.Errorf("got %q\nwant %q", out, tt.want)
			}
			if errOut != "" {
				t.Errorf("stderr = %q", errOut)
			}
		})
	}
}

func TestManifestsAddsTrailingNewline(t *testing.T) {
	h := &rspb.Hook{Path: "h.yaml", Manifest: "kind: Job"}
	out, _, err := runManifests(t, manifestCluster(t, "kind: ConfigMap", h), "api", "--hooks")
	if err != nil {
		t.Fatal(err)
	}
	if want := "kind: ConfigMap\n---\n# Source: h.yaml\nkind: Job\n"; out != want {
		t.Errorf("got %q want %q", out, want)
	}
}

func TestManifestsEmpty(t *testing.T) {
	out, _, err := runManifests(t, manifestCluster(t, ""), "api")
	if err != nil || out != "" {
		t.Errorf("out=%q err=%v", out, err)
	}
}

func TestManifestsOutputDir(t *testing.T) {
	manifest := mfV2 +
		"---\n# Source: x\napiVersion: v1\nkind: Service\nmetadata:\n  name: Cfg\n  namespace: prod\n" +
		"---\n# Source: y\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: Cfg\n  namespace: prod\n" +
		"---\n# Source: z\njust: text\n"
	dir := filepath.Join(t.TempDir(), "out", "m")
	out, _, err := runManifests(t, manifestCluster(t, manifest, testHook()), "api", "--hooks", "--output-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{
		"configmap-prod-cfg.yaml", "clusterrole-reader.yaml", "service-prod-cfg.yaml", "configmap-prod-cfg-2.yaml",
		"document-5.yaml", "job-migrate.yaml",
	}
	var wantOut string
	for _, n := range wantNames {
		wantOut += filepath.Join(dir, n) + "\n"
	}
	if out != wantOut {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, wantOut)
	}
	checks := map[string]string{
		"configmap-prod-cfg.yaml":   "# Source: api/templates/cm.yaml\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: Cfg\n  namespace: prod\n",
		"configmap-prod-cfg-2.yaml": "# Source: y\n",
		"document-5.yaml":           "# Source: z\njust: text\n",
		"job-migrate.yaml":          "# Source: api/templates/job.yaml\napiVersion: batch/v1",
	}
	for n, want := range checks {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil || !strings.HasPrefix(string(b), want) || !strings.HasSuffix(string(b), "\n") {
			t.Errorf("%s = %q, %v", n, b, err)
		}
		if fi, _ := os.Stat(filepath.Join(dir, n)); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", n, fi.Mode().Perm())
		}
	}
	if es, _ := os.ReadDir(dir); len(es) != len(wantNames) {
		t.Errorf("%d files, want %d", len(es), len(wantNames))
	}
}

func TestManifestsOutputDirWithoutHooksSkipsHook(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := runManifests(t, manifestCluster(t, mfV2, testHook()), "api", "--output-dir", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "job-migrate.yaml")); err == nil {
		t.Error("hook written without --hooks")
	}
}

func TestManifestsOutputDirTightensExistingMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "clusterrole-reader.yaml")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runManifests(t, manifestCluster(t, mfV2), "api", "--output-dir", dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestManifestsOutputDirStaysInside(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "out")
	manifest := "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ../../etc/passwd\n  namespace: a/b\n" +
		"---\napiVersion: v1\nkind: ..\nmetadata:\n  name: a/b\n"
	out, _, err := runManifests(t, manifestCluster(t, manifest), "api", "--output-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Fields(out) {
		if filepath.Dir(line) != dir {
			t.Errorf("%q escapes %q", line, dir)
		}
		if _, err := os.Stat(line); err != nil {
			t.Error(err)
		}
	}
	var n int
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if n != 2 {
		t.Errorf("%d files under root, want 2", n)
	}
}

func TestManifestsSecretWarning(t *testing.T) {
	hookSecret := &rspb.Hook{Path: "api/templates/hs.yaml", Manifest: "apiVersion: v1\nkind: Secret\nmetadata:\n  name: h\n"}
	tests := []struct {
		name string
		mf   string
		hook []*rspb.Hook
		args []string
		warn bool
	}{
		{"secret in manifest", mfV2 + "---\n" + secretDoc, nil, []string{"api"}, true},
		{"no secret", mfV2, nil, []string{"api"}, false},
		{"secret in output dir", secretDoc, nil, []string{"api", "--output-dir", t.TempDir()}, true},
		{"secret only in hooks, without flag", mfV2, []*rspb.Hook{hookSecret}, []string{"api"}, false},
		{"secret only in hooks, with flag", mfV2, []*rspb.Hook{hookSecret}, []string{"api", "--hooks"}, true},
		{"secret in older revision only", mfV2, nil, []string{"api", "-r", "1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, errOut, err := runManifests(t, manifestCluster(t, tt.mf, tt.hook...), tt.args...)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(errOut, secretWarning); (got == 1) != tt.warn || got > 1 {
				t.Errorf("warning count = %d, want warn=%v; stderr %q", got, tt.warn, errOut)
			}
			if strings.Contains(errOut, "aHVudGVyMg") || strings.Contains(out, secretWarning) {
				t.Errorf("leak: out=%q err=%q", out, errOut)
			}
		})
	}
}

func TestManifestsErrors(t *testing.T) {
	c := manifestCluster(t, mfV2)
	for _, args := range [][]string{{"api", "--revision", "9"}, {"nope"}, {"api", "--revision", "-1"}} {
		if _, _, err := runManifests(t, c, args...); err == nil {
			t.Errorf("%v: want error", args)
		}
	}
}
