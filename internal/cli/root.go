// Package cli implements the helm-debug command line.
package cli

import (
	"context"
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
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"

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
	// Logs reads container logs; nil disables log output.
	Logs kube.LogReader
}

// Options holds global flags and dependencies.
type Options struct {
	Flags       *genericclioptions.ConfigFlags
	Storage     string
	Output      string
	Verbose     bool
	Concurrency int
	// ShowSecrets prints Secret data values instead of masking them.
	ShowSecrets bool

	Out, ErrOut io.Writer
	Now         func() time.Time
	// Sleep pauses between polls of --wait; tests replace it to advance a fake clock.
	Sleep func(ctx context.Context, d time.Duration) error
	// NewClients builds cluster clients; tests replace it with fakes.
	NewClients func(o *Options) (*Clients, error)
	// Color forces colored output on or off; nil means auto-detect.
	Color *bool
	// Progress forces the progress line on stderr on or off; nil means show it when stderr is a terminal.
	Progress *bool
}

// NewOptions returns options with defaults taken from the environment.
func NewOptions() *Options {
	flags := genericclioptions.NewConfigFlags(true).WithWrapConfigFn(func(cfg *rest.Config) *rest.Config {
		cfg.WarningHandlerWithContext = deprecationWarningFilter{}
		return cfg
	})
	o := &Options{
		Flags:       flags,
		Storage:     os.Getenv("HELM_DRIVER"),
		Concurrency: 8,
		Out:         os.Stdout,
		ErrOut:      os.Stderr,
		Now:         time.Now,
		Sleep:       sleepContext,
		NewClients:  realClients,
	}
	applyHelmEnv(o.Flags)
	o.Verbose, _ = strconv.ParseBool(os.Getenv("HELM_DEBUG"))
	return o
}

type deprecationWarningFilter struct{}

func (deprecationWarningFilter) HandleWarningHeaderWithContext(ctx context.Context, code int, agent, message string) {
	if strings.Contains(message, " is deprecated ") {
		return
	}
	rest.WarningLogger{}.HandleWarningHeaderWithContext(ctx, code, agent, message)
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
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Clients{
		Store:     &helm.Store{Kube: kc, Metadata: mc, Driver: drv},
		Cluster:   &kube.Cluster{Dynamic: dc, Mapper: mapper, Discovery: disc, Metadata: mc},
		Namespace: ns,
		Logs:      kube.PodLogReader(kc),
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

// newVersionCommand exists next to --version because the scratch container image has no shell,
// so `docker run IMAGE version` is the way to check it.
func newVersionCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the helm-debug version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(o.Out, "%s version %s\n", cmd.Root().DisplayName(), Version)
		},
	}
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
	pf.BoolVar(&o.ShowSecrets, "show-secrets", false, "Show Secret data values instead of masking them (base64-encoded as stored; stringData is folded into data)")

	root.AddCommand(
		newVersionCommand(o),
		newResourcesCommand(o),
		newDriftCommand(o),
		newDiffCommand(o),
		newFindCommand(o),
		newHistoryCommand(o),
		newStuckCommand(o),
		newHealthCommand(o),
		newLostFoundCommand(o),
		newDeprecatedCommand(o),
		newAuditCommand(o),
		newHooksCommand(o),
		newListCommand(o),
		newValuesCommand(o),
		newFluxCommand(o),
	)
	return root
}
