package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/aakuz/helm-debug/internal/kube"
)

type argoSource struct {
	RepoURL        string `json:"repoURL,omitempty"`
	Chart          string `json:"chart,omitempty"`
	Path           string `json:"path,omitempty"`
	TargetRevision string `json:"targetRevision,omitempty"`
}

type argoRow struct {
	Group     string `json:"group,omitempty"`
	Version   string `json:"version,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Hook      bool   `json:"hook,omitempty"`
	Sync      string `json:"sync,omitempty"`
	Health    string `json:"health,omitempty"`
	Live      string `json:"live"`
	Tracking  string `json:"tracking,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Error     string `json:"error,omitempty"`
}

type argoResult struct {
	Application      string       `json:"application"`
	Namespace        string       `json:"namespace"`
	Sources          []argoSource `json:"sources,omitempty"`
	Sync             string       `json:"sync,omitempty"`
	Revision         string       `json:"revision,omitempty"`
	Health           string       `json:"health,omitempty"`
	HealthMessage    string       `json:"healthMessage,omitempty"`
	OperationPhase   string       `json:"operationPhase,omitempty"`
	OperationMessage string       `json:"operationMessage,omitempty"`
	Resources        []argoRow    `json:"resources"`
}

func newArgoCommand(o *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "argo",
		Short: "Inspect ArgoCD Applications",
	}
	cmd.AddCommand(newArgoResourcesCommand(o))
	return cmd
}

func newArgoResourcesCommand(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "resources APP",
		Short: "List the resources an ArgoCD Application tracks and check them against the cluster",
		Long: `Read an ArgoCD Application (argoproj.io) from the namespace given by -n (usually argocd)
and list the resources it tracks: sync and health come from the Application status, LIVE is a
get of each object, TRACKING checks that the object carries Argo tracking metadata of this
Application (annotation argocd.argoproj.io/tracking-id, else label app.kubernetes.io/instance).

Exits with code 2 when the Application is not Synced or not Healthy, or a non-hook object is
OutOfSync, Degraded or Missing, absent, or not tracked by the Application.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := o.NewClients(o)
			if err != nil {
				return err
			}
			res, err := runArgoResources(cmd, o, c, args[0])
			if err != nil {
				return err
			}
			if err := o.print(res, func(w io.Writer) { printArgoResources(o, w, res) }); err != nil {
				return err
			}
			if res.problem() {
				return ErrProblems
			}
			return nil
		},
	}
}

func (r argoRow) problem() bool {
	if r.Hook {
		return false
	}
	return r.Sync == "OutOfSync" || r.Health == "Degraded" || r.Health == "Missing" ||
		r.Live != kube.StatusPresent || r.Tracking != kube.OwnershipOK
}

func (r *argoResult) problem() bool {
	if r.Sync != "Synced" || r.Health != "Healthy" {
		return true
	}
	for _, row := range r.Resources {
		if row.problem() {
			return true
		}
	}
	return false
}

func runArgoResources(cmd *cobra.Command, o *Options, c *Clients, app string) (*argoResult, error) {
	mapping, err := c.Cluster.Mapper.RESTMapping(schema.GroupKind{Group: "argoproj.io", Kind: "Application"})
	if meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("the Application CRD (argoproj.io) is not installed in the cluster")
	}
	if err != nil {
		return nil, err
	}
	obj, err := c.Cluster.Dynamic.Resource(mapping.Resource).Namespace(c.Namespace).Get(cmd.Context(), app, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("application %s/%s not found", c.Namespace, app)
	}
	if err != nil {
		return nil, err
	}

	str := func(path ...string) string {
		s, _, _ := unstructured.NestedString(obj.Object, path...)
		return s
	}
	res := &argoResult{
		Application:      app,
		Namespace:        c.Namespace,
		Sync:             str("status", "sync", "status"),
		Revision:         str("status", "sync", "revision"),
		Health:           str("status", "health", "status"),
		HealthMessage:    str("status", "health", "message"),
		OperationPhase:   str("status", "operationState", "phase"),
		OperationMessage: str("status", "operationState", "message"),
	}

	sources, _, _ := unstructured.NestedSlice(obj.Object, "spec", "sources")
	if single, ok, _ := unstructured.NestedMap(obj.Object, "spec", "source"); ok {
		sources = append(sources, single)
	}
	for _, s := range sources {
		m, _ := s.(map[string]any)
		field := func(k string) string { v, _ := m[k].(string); return v }
		res.Sources = append(res.Sources, argoSource{RepoURL: field("repoURL"), Chart: field("chart"), Path: field("path"), TargetRevision: field("targetRevision")})
	}

	items, _, _ := unstructured.NestedSlice(obj.Object, "status", "resources")
	res.Resources = make([]argoRow, len(items))
	parallel(len(items), o.Concurrency, func(i int) {
		m, _ := items[i].(map[string]any)
		field := func(k string) string { v, _ := m[k].(string); return v }
		row := argoRow{Group: field("group"), Version: field("version"), Kind: field("kind"), Namespace: field("namespace"), Name: field("name"), Sync: field("status")}
		row.Hook, _ = m["hook"].(bool)
		if h, ok := m["health"].(map[string]any); ok {
			row.Health, _ = h["status"].(string)
		}
		defer func() { res.Resources[i] = row }()

		apiVersion := row.Version
		if row.Group != "" {
			apiVersion = row.Group + "/" + row.Version
		}
		resolved, err := c.Cluster.Resolve(apiVersion, row.Kind)
		if err != nil {
			row.Live, row.Error = kube.StatusUnknownKind, err.Error()
			return
		}
		if !resolved.Namespaced {
			row.Namespace = ""
		}
		live := c.Cluster.Get(cmd.Context(), resolved, row.Namespace, row.Name)
		row.Live, row.Error = live.Status, live.Error
		if live.Status == kube.StatusPresent {
			row.Tracking, row.Owner = kube.ArgoTracking(app, c.Namespace, live.Object.GetAnnotations(), live.Object.GetLabels())
		}
	})
	return res, nil
}

func printArgoResources(o *Options, w io.Writer, res *argoResult) {
	fmt.Fprintf(w, "Application %s/%s\n", res.Namespace, res.Application)
	for _, s := range res.Sources {
		src := s.RepoURL
		if s.Chart != "" {
			src += " chart " + s.Chart
		}
		if s.Path != "" {
			src += " path " + s.Path
		}
		fmt.Fprintf(w, "Source:    %s @ %s\n", src, s.TargetRevision)
	}
	line := func(label, status, detail string) {
		s := o.paint(status, statusColor(status))
		if detail != "" {
			s += " " + detail
		}
		fmt.Fprintf(w, "%-10s %s\n", label+":", s)
	}
	line("Sync", res.Sync, res.Revision)
	line("Health", res.Health, res.HealthMessage)
	if res.OperationPhase != "" {
		line("Operation", res.OperationPhase, res.OperationMessage)
	}
	fmt.Fprintln(w)

	t := newTable(w)
	t.add("KIND", "NAMESPACE", "NAME", "HOOK", "SYNC", "HEALTH", "LIVE", "TRACKING")
	for _, r := range res.Resources {
		hook := ""
		if r.Hook {
			hook = "hook"
		}
		tracking := r.Tracking
		if r.Owner != "" {
			tracking += " (" + r.Owner + ")"
		}
		t.add(r.Kind, r.Namespace, r.Name, hook, o.paint(r.Sync, statusColor(r.Sync)), o.paint(r.Health, statusColor(r.Health)),
			o.paint(r.Live, statusColor(r.Live)), o.paint(tracking, statusColor(r.Tracking)))
	}
	t.flush()
	sep := "\n"
	for _, r := range res.Resources {
		if r.Error != "" {
			fmt.Fprintf(w, "%s%s %s/%s: %s\n", sep, o.paint("error:", red), r.Kind, r.Name, r.Error)
			sep = ""
		}
	}
}
