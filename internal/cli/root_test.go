package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
)

var helmEnv = []string{
	"HELM_NAMESPACE", "HELM_KUBECONTEXT", "KUBECONFIG", "HELM_KUBETOKEN", "HELM_KUBEAPISERVER",
	"HELM_KUBEASUSER", "HELM_KUBEASGROUPS", "HELM_KUBECAFILE", "HELM_KUBETLS_SERVER_NAME",
	"HELM_KUBEINSECURE_SKIP_TLS_VERIFY", "HELM_DRIVER", "HELM_DEBUG", "HELM_PLUGIN_NAME",
}

// cleanEnv blanks every variable the CLI reads so the ambient environment cannot leak in.
func cleanEnv(t *testing.T) {
	t.Helper()
	for _, k := range helmEnv {
		t.Setenv(k, "")
	}
}

func TestApplyHelmEnvStrings(t *testing.T) {
	cleanEnv(t)
	want := map[string]string{
		"HELM_NAMESPACE":           "prod",
		"HELM_KUBECONTEXT":         "ctx",
		"KUBECONFIG":               "/tmp/kc",
		"HELM_KUBETOKEN":           "tok",
		"HELM_KUBEAPISERVER":       "https://api",
		"HELM_KUBEASUSER":          "bob",
		"HELM_KUBECAFILE":          "/tmp/ca",
		"HELM_KUBETLS_SERVER_NAME": "sni",
	}
	for k, v := range want {
		t.Setenv(k, v)
	}
	f := genericclioptions.NewConfigFlags(true)
	applyHelmEnv(f)
	got := map[string]string{
		"HELM_NAMESPACE":           *f.Namespace,
		"HELM_KUBECONTEXT":         *f.Context,
		"KUBECONFIG":               *f.KubeConfig,
		"HELM_KUBETOKEN":           *f.BearerToken,
		"HELM_KUBEAPISERVER":       *f.APIServer,
		"HELM_KUBEASUSER":          *f.Impersonate,
		"HELM_KUBECAFILE":          *f.CAFile,
		"HELM_KUBETLS_SERVER_NAME": *f.TLSServerName,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestApplyHelmEnvEmptyKeepsDefaults(t *testing.T) {
	cleanEnv(t)
	f := genericclioptions.NewConfigFlags(true)
	applyHelmEnv(f)
	if *f.Namespace != "" || *f.Context != "" || *f.KubeConfig != "" || len(*f.ImpersonateGroup) != 0 || *f.Insecure {
		t.Fatalf("empty env must leave defaults, got ns=%q ctx=%q", *f.Namespace, *f.Context)
	}
}

func TestApplyHelmEnvGroupsSplit(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HELM_KUBEASGROUPS", "dev,ops,admins")
	f := genericclioptions.NewConfigFlags(true)
	applyHelmEnv(f)
	if want := []string{"dev", "ops", "admins"}; !reflect.DeepEqual(*f.ImpersonateGroup, want) {
		t.Fatalf("got %v, want %v", *f.ImpersonateGroup, want)
	}
}

func TestApplyHelmEnvInsecure(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{{"true", true}, {"1", true}, {"false", false}, {"bogus", false}, {"", false}} {
		t.Run("value="+tc.env, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("HELM_KUBEINSECURE_SKIP_TLS_VERIFY", tc.env)
			f := genericclioptions.NewConfigFlags(true)
			applyHelmEnv(f)
			if *f.Insecure != tc.want {
				t.Fatalf("insecure = %v, want %v", *f.Insecure, tc.want)
			}
		})
	}
}

func TestApplyHelmEnvInvalidInsecureKeepsPreset(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HELM_KUBEINSECURE_SKIP_TLS_VERIFY", "bogus")
	f := genericclioptions.NewConfigFlags(true)
	*f.Insecure = true
	applyHelmEnv(f)
	if !*f.Insecure {
		t.Fatal("invalid value must not reset the flag")
	}
}

func TestApplyHelmEnvNilFields(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HELM_NAMESPACE", "prod")
	t.Setenv("HELM_KUBEASGROUPS", "a")
	t.Setenv("HELM_KUBEINSECURE_SKIP_TLS_VERIFY", "true")
	applyHelmEnv(&genericclioptions.ConfigFlags{}) // must not panic
}

func TestNewOptionsFromEnv(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HELM_DRIVER", "configmap")
	t.Setenv("HELM_DEBUG", "true")
	t.Setenv("HELM_NAMESPACE", "from-env")
	o := NewOptions()
	if o.Storage != "configmap" || !o.Verbose || *o.Flags.Namespace != "from-env" || o.Concurrency != 8 {
		t.Fatalf("unexpected options: storage=%q verbose=%v ns=%q conc=%d", o.Storage, o.Verbose, *o.Flags.Namespace, o.Concurrency)
	}
	cleanEnv(t)
	o = NewOptions()
	if o.Storage != "" || o.Verbose {
		t.Fatalf("defaults expected, got storage=%q verbose=%v", o.Storage, o.Verbose)
	}
	t.Setenv("HELM_DEBUG", "not-a-bool")
	if NewOptions().Verbose {
		t.Fatal("invalid HELM_DEBUG must be false")
	}
}

// capturedOptions runs `args` with a NewClients stub and returns the options after flag parsing.
func capturedOptions(t *testing.T, args ...string) *Options {
	t.Helper()
	var out bytes.Buffer
	o := NewOptions()
	o.Out, o.ErrOut = &out, &out
	called := false
	o.NewClients = func(*Options) (*Clients, error) {
		called = true
		return nil, os.ErrClosed // stop right after flags are parsed
	}
	cmd := NewRootCommand(o)
	cmd.SetArgs(args)
	_ = cmd.ExecuteContext(context.Background())
	if !called {
		t.Fatalf("command %v did not reach NewClients: %s", args, out.String())
	}
	return o
}

func TestEnvIsDefaultAndFlagOverrides(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HELM_NAMESPACE", "from-env")
	t.Setenv("HELM_KUBECONTEXT", "ctx-env")
	t.Setenv("HELM_DRIVER", "configmap")

	o := capturedOptions(t, "stuck")
	if *o.Flags.Namespace != "from-env" || *o.Flags.Context != "ctx-env" || o.Storage != "configmap" {
		t.Fatalf("env defaults lost: ns=%q ctx=%q storage=%q", *o.Flags.Namespace, *o.Flags.Context, o.Storage)
	}

	o = capturedOptions(t, "stuck", "-n", "from-flag", "--context", "ctx-flag", "--storage", "secret")
	if *o.Flags.Namespace != "from-flag" || *o.Flags.Context != "ctx-flag" || o.Storage != "secret" {
		t.Fatalf("flags must override env: ns=%q ctx=%q storage=%q", *o.Flags.Namespace, *o.Flags.Context, o.Storage)
	}
}

func TestGlobalFlags(t *testing.T) {
	cleanEnv(t)
	o := capturedOptions(t, "stuck", "-o", "json", "-v", "--concurrency", "3", "--show-secrets")
	if o.Output != "json" || !o.Verbose || o.Concurrency != 3 || !o.ShowSecrets {
		t.Fatalf("flags not applied: %+v", o)
	}
	// The value must also work end to end, not only parse.
	_, err := run(t, backendCluster(t), "prod", "resources", "backend", "--concurrency", "1")
	wantProblems(t, err, true)
}

func TestDisplayName(t *testing.T) {
	orig := os.Args[0]
	t.Cleanup(func() { os.Args[0] = orig })
	for _, tc := range []struct {
		name   string
		plugin string
		arg0   string
		want   string
	}{
		{"plugin", "debug", "/usr/bin/helm-debug", "helm debug"},
		{"plugin wins over kubectl", "debug", "kubectl-helm_debug", "helm debug"},
		{"kubectl", "", "kubectl-helm_debug", "kubectl helm-debug"},
		{"kubectl path", "", "/usr/local/bin/kubectl-helm_debug", "kubectl helm-debug"},
		{"kubectl exe", "", `kubectl-helm_debug.exe`, "kubectl helm-debug"},
		{"plain", "", "helm-debug", "helm-debug"},
		{"plain path", "", "/opt/bin/helm-debug", "helm-debug"},
		{"plain exe", "", "helm-debug.exe", "helm-debug"},
		{"other binary", "", "something-else", "helm-debug"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("HELM_PLUGIN_NAME", tc.plugin)
			os.Args[0] = tc.arg0
			if got := DisplayName(); got != tc.want {
				t.Fatalf("DisplayName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRootCommandUsesDisplayName(t *testing.T) {
	cleanEnv(t)
	t.Setenv("HELM_PLUGIN_NAME", "debug")
	cmd := NewRootCommand(NewOptions())
	if got := cmd.DisplayName(); got != "helm debug" {
		t.Fatalf("display name = %q", got)
	}
}

func TestOutputValidation(t *testing.T) {
	for _, format := range []string{"", "table", "json", "yaml"} {
		t.Run("ok "+format, func(t *testing.T) {
			args := []string{"stuck"}
			if format != "" {
				args = append(args, "-o", format)
			}
			if _, err := run(t, newCluster(t), "prod", args...); err != nil {
				t.Fatalf("format %q rejected: %v", format, err)
			}
		})
	}
	for _, format := range []string{"xml", "JSON", "wide", "name"} {
		t.Run("bad "+format, func(t *testing.T) {
			_, err := run(t, newCluster(t), "prod", "stuck", "-o", format)
			if err == nil || !strings.Contains(err.Error(), "unsupported output format") || !strings.Contains(err.Error(), format) {
				t.Fatalf("want unsupported output error naming %q, got %v", format, err)
			}
		})
	}
}

func TestUnsupportedOutputRejectedBeforeClients(t *testing.T) {
	o := NewOptions()
	o.Out, o.ErrOut = &bytes.Buffer{}, &bytes.Buffer{}
	o.NewClients = func(*Options) (*Clients, error) {
		t.Error("clients must not be built for an invalid output format")
		return nil, os.ErrClosed
	}
	cmd := NewRootCommand(o)
	cmd.SetArgs([]string{"history", "x", "-o", "xml"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "unsupported output format") {
		t.Fatalf("got %v", err)
	}
}

func TestVerboseEnablesDebugLogging(t *testing.T) {
	cleanEnv(t)
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	for _, tc := range []struct {
		args []string
		want bool
	}{{[]string{"stuck"}, false}, {[]string{"stuck", "-v"}, true}} {
		capturedOptions(t, tc.args...)
		if got := slog.Default().Enabled(context.Background(), slog.LevelDebug); got != tc.want {
			t.Fatalf("%v: debug enabled = %v, want %v", tc.args, got, tc.want)
		}
	}
	t.Setenv("HELM_DEBUG", "true")
	capturedOptions(t, "stuck")
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("HELM_DEBUG=true must enable debug logging")
	}
}

func TestVersionFlag(t *testing.T) {
	var out bytes.Buffer
	o := NewOptions()
	o.Out = &out
	cmd := NewRootCommand(o)
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), Version) {
		t.Fatalf("version output %q lacks %q", out.String(), Version)
	}
}

func TestVersionCommandPrintsReleaseVersion(t *testing.T) {
	old := Version
	Version = "1.2.3"
	t.Cleanup(func() { Version = old })
	out, err := run(t, newCluster(t), "prod", "version")
	wantProblems(t, err, false)
	if !strings.Contains(out, "1.2.3") {
		t.Fatalf("got %q", out)
	}
}
