package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	"sigs.k8s.io/yaml"
)

const (
	notesV1     = "Thank you for installing api v1.\n"
	notesV2     = "Visit http://api.prod\n"
	unsafeNotes = "\x1b]0;pwned\x07Visit \x1b[31mhttp://x\x1b[0m‮"
)

func notesRelease(t *testing.T, c *cluster, version int, notes string) {
	t.Helper()
	rel := &rspb.Release{
		Name: "api", Namespace: "prod", Version: version,
		Info:  &rspb.Info{Status: common.StatusDeployed, LastDeployed: now, Notes: notes},
		Chart: &chart.Chart{Metadata: &chart.Metadata{Name: "api", Version: "1.0.0"}},
	}
	key := fmt.Sprintf("sh.helm.release.v1.api.v%d", version)
	if err := driver.NewSecrets(c.kube.CoreV1().Secrets("prod")).Create(key, rel); err != nil {
		t.Fatal(err)
	}
}

func runNotes(t *testing.T, c *cluster, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut, o.Now = &out, &errOut, func() time.Time { return now }
	o.NewClients = func(*Options) (*Clients, error) { return c.clients("prod"), nil }
	cmd := NewRootCommand(o)
	cmd.SetArgs(append([]string{"notes"}, args...))
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func notesCluster(t *testing.T, latest string) *cluster {
	c := newCluster(t)
	notesRelease(t, c, 1, notesV1)
	notesRelease(t, c, 2, latest)
	return c
}

func TestNotesStdout(t *testing.T) {
	tests := []struct {
		name   string
		latest string
		args   []string
		want   string
	}{
		{"latest", notesV2, []string{"api"}, notesV2},
		{"revision 1", notesV2, []string{"api", "--revision", "1"}, notesV1},
		{"short revision", notesV2, []string{"api", "-r", "1"}, notesV1},
		{"explicit table output", notesV2, []string{"api", "-o", "table"}, notesV2},
		{"empty notes print nothing", "", []string{"api"}, ""},
		{"adds trailing newline", "Visit http://api.prod", []string{"api"}, "Visit http://api.prod\n"},
		{"strips escapes and bidi", unsafeNotes, []string{"api"}, "Visit http://x\n"},
		{"keeps newlines and tabs", "a\tb\nc\n", []string{"api"}, "a\tb\nc\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, errOut, err := runNotes(t, notesCluster(t, tt.latest), tt.args...)
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

func TestNotesStructured(t *testing.T) {
	type notesOut struct {
		Release   string `json:"release"`
		Namespace string `json:"namespace"`
		Revision  int    `json:"revision"`
		Notes     string `json:"notes"`
	}
	tests := []struct {
		name   string
		latest string
		args   []string
		want   notesOut
	}{
		{"json latest", notesV2, []string{"api", "-o", "json"}, notesOut{"api", "prod", 2, notesV2}},
		{"yaml revision 1", notesV2, []string{"api", "-o", "yaml", "--revision", "1"}, notesOut{"api", "prod", 1, notesV1}},
		{"json sanitized", unsafeNotes, []string{"api", "-o", "json"}, notesOut{"api", "prod", 2, "Visit http://x"}},
		{"yaml sanitized", unsafeNotes, []string{"api", "-o", "yaml"}, notesOut{"api", "prod", 2, "Visit http://x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, errOut, err := runNotes(t, notesCluster(t, tt.latest), tt.args...)
			if err != nil {
				t.Fatal(err)
			}
			var got notesOut
			// sigs.k8s.io/yaml reads JSON too and honours the json tags.
			if err := yaml.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("not parseable: %v\n%s", err, out)
			}
			if got != tt.want {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
			if errOut != "" {
				t.Errorf("stderr = %q", errOut)
			}
		})
	}
}

func TestNotesJSONKeys(t *testing.T) {
	out, _, err := runNotes(t, notesCluster(t, notesV2), "api", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not json: %v\n%s", err, out)
	}
	for _, k := range []string{"release", "namespace", "revision", "notes"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q in %s", k, out)
		}
	}
	if len(got) != 4 {
		t.Errorf("want exactly 4 keys, got %v", got)
	}
}

func TestNotesErrors(t *testing.T) {
	c := notesCluster(t, notesV2)
	for _, args := range [][]string{{"nope"}, {"api", "--revision", "9"}, {"api", "--revision", "-1"}} {
		if _, _, err := runNotes(t, c, args...); err == nil {
			t.Errorf("%v: want error", args)
		}
	}
}
