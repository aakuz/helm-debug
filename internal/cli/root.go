// Package cli implements the helm-debug command line.
package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"

	"github.com/aakuz/helm-debug/internal/helm"
	"github.com/aakuz/helm-debug/internal/kube"
)

// ErrProblems signals that the command ran fine but found problems (exit code 2).
var ErrProblems = errors.New("problems found")

// Version is set at build time.
var Version = "dev"

// Clients bundles everything commands need to talk to the cluster.
type Clients struct {
	Store     *helm.Store
	Cluster   *kube.Cluster
	Namespace string
}

// Options holds global flags and dependencies.
type Options struct {
	Flags       *genericclioptions.ConfigFlags
	Storage     string
	Output      string
	Verbose     bool
	Concurrency int

	Out, ErrOut io.Writer
	Now         func() time.Time
	// NewClients builds cluster clients; tests replace it with fakes.
	NewClients func(o *Options) (*Clients, error)
	// Color forces colored output on or off; nil means auto-detect.
	Color *bool
}

// NewOptions returns options with defaults taken from the environment.
func NewOptions() *Options {
	o := &Options{
		Flags:       genericclioptions.NewConfigFlags(true),
		Storage:     os.Getenv("HELM_DRIVER"),
		Concurrency: 8,
		Out:         os.Stdout,
		ErrOut:      os.Stderr,
		Now:         time.Now,
		NewClients:  realClients,
	}
	applyHelmEnv(o.Flags)
	o.Verbose, _ = strconv.ParseBool(os.Getenv("HELM_DEBUG"))
	return o
}

// applyHelmEnv uses the variables helm passes to plugins as flag defaults, so
// `helm debug -n prod ...` targets the same cluster and namespace as helm itself.
func applyHelmEnv(f *genericclioptions.ConfigFlags) {
	str := map[string]*string{
		"HELM_NAMESPACE":           f.Namespace,
		"HELM_KUBECONTEXT":         f.Context,
		"KUBECONFIG":               f.KubeConfig,
		"HELM_KUBETOKEN":           f.BearerToken,
		"HELM_KUBEAPISERVER":       f.APIServer,
		"HELM_KUBEASUSER":          f.Impersonate,
		"HELM_KUBECAFILE":          f.CAFile,
		"HELM_KUBETLS_SERVER_NAME": f.TLSServerName,
	}
	for env, dst := range str {
		if v := os.Getenv(env); v != "" && dst != nil {
			*dst = v
		}
	}
	if v := os.Getenv("HELM_KUBEASGROUPS"); v != "" && f.ImpersonateGroup != nil {
		*f.ImpersonateGroup = strings.Split(v, ",")
	}
	if v, err := strconv.ParseBool(os.Getenv("HELM_KUBEINSECURE_SKIP_TLS_VERIFY")); err == nil && f.Insecure != nil {
		*f.Insecure = v
	}
}

func realClients(o *Options) (*Clients, error) {
	drv, err := helm.NormalizeDriver(o.Storage)
	if err != nil {
		return nil, err
	}
	cfg, err := o.Flags.ToRESTConfig()
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	mc, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	mapper, err := o.Flags.ToRESTMapper()
	if err != nil {
		return nil, err
	}
	ns, _, err := o.Flags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return nil, err
	}
	return &Clients{
		Store:     &helm.Store{Kube: kc, Metadata: mc, Driver: drv},
		Cluster:   &kube.Cluster{Dynamic: dc, Mapper: mapper},
		Namespace: ns,
	}, nil
}

// DisplayName is how the user invoked the tool: helm-debug, kubectl helm-debug or helm debug.
func DisplayName() string {
	if os.Getenv("HELM_PLUGIN_NAME") != "" {
		return "helm debug"
	}
	base := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if strings.HasPrefix(base, "kubectl-") {
		return "kubectl " + strings.ReplaceAll(strings.TrimPrefix(base, "kubectl-"), "_", "-")
	}
	return "helm-debug"
}

// NewRootCommand builds the command tree.
func NewRootCommand(o *Options) *cobra.Command {
	root := &cobra.Command{
		Use:           "helm-debug",
		Short:         "Debug Helm releases by reading their records straight from the cluster",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Annotations:   map[string]string{cobra.CommandDisplayNameAnnotation: DisplayName()},
		PersistentPreRunE: func(*cobra.Command, []string) error {
			level := slog.LevelWarn
			if o.Verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(o.ErrOut, &slog.HandlerOptions{Level: level})))
			switch o.Output {
			case "", "table", "json", "yaml":
				return nil
			default:
				return fmt.Errorf("unsupported output format %q (supported: table, json, yaml)", o.Output)
			}
		},
	}
	o.Flags.AddFlags(root.PersistentFlags())
	pf := root.PersistentFlags()
	pf.StringVar(&o.Storage, "storage", o.Storage, "Helm storage driver: secret or configmap (default from HELM_DRIVER, else secret)")
	pf.StringVarP(&o.Output, "output", "o", "table", "Output format: table, json or yaml")
	pf.BoolVarP(&o.Verbose, "verbose", "v", o.Verbose, "Verbose logging to stderr (also enabled by HELM_DEBUG=true)")
	pf.IntVar(&o.Concurrency, "concurrency", o.Concurrency, "Number of parallel API requests")

	root.AddCommand(
		newResourcesCommand(o),
		newDriftCommand(o),
		newFindCommand(o),
		newHistoryCommand(o),
		newStuckCommand(o),
	)
	return root
}
