package cli

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
)

const diffManifestV1 = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 2
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: removed
data:
  a: b
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: same
data:
  a: b
`

const diffManifestV2 = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 3
---
apiVersion: v1
kind: Service
metadata:
  name: added
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: same
data:
  a: b
`

func diffCluster(t *testing.T) *cluster {
	c := newCluster(t)
	c.releaseWithValues("api", "prod", 1, common.StatusSuperseded, diffManifestV1, map[string]any{"replicas": 2, "image": map[string]any{"tag": "v1"}})
	c.releaseWithValues("api", "prod", 2, common.StatusDeployed, diffManifestV2, map[string]any{"replicas": 3, "image": map[string]any{"tag": "v1"}})
	return c
}

func TestDiffJSON(t *testing.T) {
	out, err := run(t, diffCluster(t), "prod", "diff", "api", "1", "2", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		From, To int
		Manifest []struct {
			Kind, Name, Status, Diff string
		}
		Values struct {
			Status, Diff string
		}
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, o := range result.Manifest {
		got[o.Kind+"/"+o.Name] = o.Status
	}
	want := map[string]string{"Deployment/api": "changed", "ConfigMap/removed": "removed", "Service/added": "added"}
	if len(got) != len(want) {
		t.Fatalf("manifest statuses = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("manifest statuses = %v, want %v", got, want)
		}
	}
	if result.From != 1 || result.To != 2 || result.Values.Status != "changed" {
		t.Fatalf("unexpected result: %s", out)
	}
	if !strings.Contains(result.Values.Diff, "-replicas: 2") || !strings.Contains(result.Values.Diff, "+replicas: 3") {
		t.Fatalf("values diff = %q", result.Values.Diff)
	}
}

func TestDiffTable(t *testing.T) {
	out, err := run(t, diffCluster(t), "prod", "diff", "api", "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, out)
}

func TestDiffYAML(t *testing.T) {
	out, err := run(t, diffCluster(t), "prod", "diff", "api", "1", "2", "-o", "yaml")
	if err != nil || !strings.Contains(out, "status: changed") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}

func TestDiffIdenticalRevisionsExitZero(t *testing.T) {
	out, err := run(t, diffCluster(t), "prod", "diff", "api", "2", "2")
	if err != nil {
		t.Fatalf("identical revisions must exit 0: %v", err)
	}
	if !strings.Contains(out, "No differences") {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestDiffDifferencesExitZero(t *testing.T) {
	_, err := run(t, diffCluster(t), "prod", "diff", "api", "1", "2")
	if err != nil {
		t.Fatalf("differences must exit 0, got %v", err)
	}
}

func TestDiffUnknownRevision(t *testing.T) {
	_, err := run(t, diffCluster(t), "prod", "diff", "api", "1", "9")
	if err == nil || errors.Is(err, ErrProblems) {
		t.Fatalf("want execution error, got %v", err)
	}
}

func TestDiffBadRevisionArgument(t *testing.T) {
	_, err := run(t, diffCluster(t), "prod", "diff", "api", "one", "2")
	if err == nil || errors.Is(err, ErrProblems) {
		t.Fatalf("want execution error, got %v", err)
	}
}

func TestDiffMasksSecretValues(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusSuperseded, `apiVersion: v1
kind: Secret
metadata:
  name: creds
data:
  password: b2xkLXNlY3JldA==
  kept: a2VwdC1zZWNyZXQ=
stringData:
  token: old-token
`)
	c.release("api", "prod", 2, common.StatusDeployed, `apiVersion: v1
kind: Secret
metadata:
  name: creds
data:
  password: bmV3LXNlY3JldA==
  kept: a2VwdC1zZWNyZXQ=
stringData:
  token: old-token
  extra: brand-new
`)
	for _, format := range []string{"table", "json"} {
		out, err := run(t, c, "prod", "diff", "api", "1", "2", "-o", format)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"b2xkLXNlY3JldA", "bmV3LXNlY3JldA", "a2VwdC1zZWNyZXQ", "old-token", "brand-new"} {
			if strings.Contains(out, secret) {
				t.Fatalf("%s output leaks %q:\n%s", format, secret, out)
			}
		}
	}
	out, _ := run(t, c, "prod", "diff", "api", "1", "2")
	for _, want := range []string{"-  password: <masked:old>", "+  password: <masked:new>", "+  extra: <masked>"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "-  kept") || strings.Contains(out, "+  kept") || strings.Contains(out, "token") && (strings.Contains(out, "-  token") || strings.Contains(out, "+  token")) {
		t.Fatalf("unchanged secret key reported as changed:\n%s", out)
	}
}

func TestDiffMasksSecretMovedBetweenDataAndStringData(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusSuperseded, `apiVersion: v1
kind: Secret
metadata:
  name: creds
data:
  password: b2xkLXNlY3JldA==
  same: c2FtZS12YWx1ZQ==
`)
	c.release("api", "prod", 2, common.StatusDeployed, `apiVersion: v1
kind: Secret
metadata:
  name: creds
stringData:
  password: new-secret
  same: same-value
`)
	out, err := run(t, c, "prod", "diff", "api", "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"b2xkLXNlY3JldA", "new-secret", "c2FtZS12YWx1ZQ", "same-value"} {
		if strings.Contains(out, secret) {
			t.Fatalf("output leaks %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{"-  password: <masked:old>", "+  password: <masked:new>", "-  same: <masked>", "+  same: <masked>"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "same: <masked:") {
		t.Fatalf("equivalent value reported as changed:\n%s", out)
	}
}

func TestDiffParseErrorDoesNotLeakSecret(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusSuperseded, diffManifestV1)
	c.release("api", "prod", 2, common.StatusDeployed, `apiVersion: v1
kind: ConfigMap
metadata:
  name: ok
---
apiVersion: v1
kind: Secret
metadata:
  name: creds
data:
  password: hunter2-TOPSECRET
 bad: [unclosed
`)
	_, err := run(t, c, "prod", "diff", "api", "1", "2")
	if err == nil {
		t.Fatal("want parse error")
	}
	if strings.Contains(err.Error(), "TOPSECRET") || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks secret content: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid YAML in document 2") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUnifiedDiffLargeMostlyIdentical(t *testing.T) {
	const n = 30000
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "line " + strconv.Itoa(i)
	}
	oldText := strings.Join(lines, "\n") + "\n"
	lines[15000] = "changed"
	newText := strings.Join(lines, "\n") + "\n"
	start := time.Now()
	got := unifiedDiff(oldText, newText)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("unifiedDiff too slow: %v", time.Since(start))
	}
	want := "@@ -14998,7 +14998,7 @@\n line 14997\n line 14998\n line 14999\n-line 15000\n+changed\n line 15001\n line 15002\n line 15003\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestDiffExcludesHooks(t *testing.T) {
	c := newCluster(t)
	c.release("api", "prod", 1, common.StatusSuperseded, diffManifestV1, &rspb.Hook{Name: "migrate", Kind: "Job", Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: migrate\n"})
	c.release("api", "prod", 2, common.StatusDeployed, diffManifestV1)
	out, err := run(t, c, "prod", "diff", "api", "1", "2")
	if err != nil || !strings.Contains(out, "No differences") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}

func TestDiffColor(t *testing.T) {
	result := &diffResult{
		Release: "api", Namespace: "prod", From: 1, To: 2,
		Manifest: []diffObject{{Kind: "Deployment", Name: "api", Status: diffChanged, Diff: "@@ -1,1 +1,1 @@\n-replicas: 2\n+replicas: 3\n"}},
		Values:   diffValues{Status: diffUnchanged},
	}
	render := func(color bool) string {
		o := NewOptions()
		o.Color = &color
		var b strings.Builder
		printDiff(o, &b, result)
		return b.String()
	}
	colored := render(true)
	for _, want := range []string{cyan + "@@ -1,1 +1,1 @@" + reset, red + "-replicas: 2" + reset, green + "+replicas: 3" + reset, yellow + "changed" + reset} {
		if !strings.Contains(colored, want) {
			t.Errorf("colored output lacks %q:\n%q", want, colored)
		}
	}
	if plain := render(false); strings.Contains(plain, "\x1b") {
		t.Errorf("plain output contains escapes: %q", plain)
	}
}

func TestDriftStatusColor(t *testing.T) {
	color := true
	o := NewOptions()
	o.Color = &color
	var b strings.Builder
	printDrift(o, &b, &driftResult{Release: "api", Namespace: "prod", Revision: 1, Resources: []driftRow{{Status: "drifted"}, {Status: "in-sync"}}})
	if !strings.Contains(b.String(), yellow+"drifted"+reset) || !strings.Contains(b.String(), green+"in-sync"+reset) {
		t.Errorf("drift statuses not colored: %q", b.String())
	}
}
