package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	"sigs.k8s.io/yaml"
)

func valuesRelease(t *testing.T, c *cluster, version int, defaults, user map[string]any) {
	t.Helper()
	rel := &rspb.Release{
		Name: "api", Namespace: "prod", Version: version, Config: user,
		Info:  &rspb.Info{Status: common.StatusDeployed, LastDeployed: now},
		Chart: &chart.Chart{Metadata: &chart.Metadata{Name: "api", Version: "1.0.0"}, Values: defaults},
	}
	key := fmt.Sprintf("sh.helm.release.v1.api.v%d", version)
	if err := driver.NewSecrets(c.kube.CoreV1().Secrets("prod")).Create(key, rel); err != nil {
		t.Fatal(err)
	}
}

func runValues(t *testing.T, c *cluster, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut, o.Now = &out, &errOut, func() time.Time { return now }
	o.NewClients = func(*Options) (*Clients, error) { return c.clients("prod"), nil }
	cmd := NewRootCommand(o)
	cmd.SetArgs(append([]string{"values"}, args...))
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func twoRevisions(t *testing.T) *cluster {
	c := newCluster(t)
	defaults := map[string]any{"replicas": 1, "image": map[string]any{"tag": "latest", "repo": "api"}}
	valuesRelease(t, c, 1, defaults, map[string]any{"replicas": 2})
	valuesRelease(t, c, 2, defaults, map[string]any{"replicas": 5, "image": map[string]any{"tag": "v2"}})
	return c
}

func TestValuesUserSuppliedLatestAsYAML(t *testing.T) {
	out, errOut, err := runValues(t, twoRevisions(t), "api")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not yaml: %v\n%s", err, out)
	}
	if got["replicas"] != float64(5) || got["image"].(map[string]any)["tag"] != "v2" || len(got) != 2 {
		t.Errorf("got %v", got)
	}
	if !strings.Contains(errOut, "secrets") {
		t.Errorf("want secrets warning on stderr, got %q", errOut)
	}
	if strings.Contains(out, "secrets") {
		t.Errorf("warning leaked to stdout: %s", out)
	}
}

func TestValuesRevision(t *testing.T) {
	out, _, err := runValues(t, twoRevisions(t), "api", "--revision", "1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "replicas: 2" {
		t.Errorf("got %q", out)
	}
}

func TestValuesAllMergesChartDefaults(t *testing.T) {
	out, _, err := runValues(t, twoRevisions(t), "api", "--all", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Image    map[string]any
		Replicas float64
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not json: %v\n%s", err, out)
	}
	if got.Replicas != 5 || got.Image["tag"] != "v2" || got.Image["repo"] != "api" {
		t.Errorf("got %+v", got)
	}
}

func TestValuesEmptyConfigIsEmptyMap(t *testing.T) {
	c := newCluster(t)
	valuesRelease(t, c, 1, nil, nil)
	out, _, err := runValues(t, c, "api", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "{}" {
		t.Errorf("got %q", out)
	}
}

func TestValuesOutputFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "values.yaml")
	out, errOut, err := runValues(t, twoRevisions(t), "api", "--output-file", path)
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("stdout should be empty, got %q", out)
	}
	if !strings.Contains(errOut, "secrets") {
		t.Errorf("want warning on stderr")
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), "replicas: 5") {
		t.Fatalf("file = %q, %v", b, err)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestValuesErrors(t *testing.T) {
	c := twoRevisions(t)
	for _, args := range [][]string{{"api", "--revision", "9"}, {"nope"}, {"api", "--revision", "-1"}} {
		if _, _, err := runValues(t, c, args...); err == nil {
			t.Errorf("%v: want error", args)
		}
	}
}

func TestValuesNeverLogsValuesWithVerbose(t *testing.T) {
	c := newCluster(t)
	valuesRelease(t, c, 1, nil, map[string]any{"password": "hunter2"})
	out, errOut, err := runValues(t, c, "api", "--verbose", "--output-file", filepath.Join(t.TempDir(), "v.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out+errOut, "hunter2") {
		t.Errorf("value leaked:\n%s\n%s", out, errOut)
	}
}

func TestValuesOutputFileTightensExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runValues(t, twoRevisions(t), "api", "-r", "2", "--output-file", path); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}
