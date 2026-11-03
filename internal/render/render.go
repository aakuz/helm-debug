// Package render renders charts client-side with the Helm SDK and checks the output, without a cluster.
package render

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"

	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/engine"
	"helm.sh/helm/v4/pkg/strvals"
)

// Options are the release settings a chart is rendered with, as for helm template.
type Options struct {
	ReleaseName string
	Namespace   string
	// Values are user-supplied values (-f, --set), merged over the chart's defaults.
	Values map[string]any
	// ShowSecrets keeps Secret data values in context lines instead of masking them.
	ShowSecrets bool
}

// Load reads a chart directory or archive.
func Load(chartPath string) (*chart.Chart, error) {
	return loader.Load(chartPath)
}

// Values merges local values files and then --set expressions, later ones winning, like helm template -f/--set.
// Remote values files are not supported: nothing is fetched over the network.
func Values(files, sets []string) (map[string]any, error) {
	vals := map[string]any{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		m, err := loader.LoadValues(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", f, err)
		}
		vals = loader.MergeMaps(vals, m)
	}
	for _, s := range sets {
		if err := strvals.ParseInto(s, vals); err != nil {
			return nil, fmt.Errorf("failed parsing --set data: %w", err)
		}
	}
	return vals, nil
}

// TemplateError is a template that failed to render.
type TemplateError struct {
	File string
	Err  error
}

// Render renders every template of ch. Templates are rendered without a cluster, so lookup returns empty.
//
// text/template stops at the first failing template, which hides the rest. When the whole chart does not
// render, every template is rendered on its own so all failures are reported; the output of the templates that
// did render is returned along with the failures.
func Render(ch *chart.Chart, o Options) (map[string]string, []TemplateError, error) {
	vals, err := util.ToRenderValuesWithSchemaValidation(ch, o.Values, common.ReleaseOptions{
		Name: o.ReleaseName, Namespace: o.Namespace, Revision: 1, IsInstall: true,
	}, common.DefaultCapabilities, false)
	if err != nil {
		return nil, nil, err
	}
	out, err := engine.Render(ch, vals)
	if err == nil {
		return out, nil, nil
	}
	out = map[string]string{}
	var failed []TemplateError
	for _, name := range templateNames(ch, ch.Name()) {
		got, err := engine.Render(only(ch, ch.Name(), name), vals)
		if err != nil {
			failed = append(failed, TemplateError{File: name, Err: err})
			continue
		}
		maps.Copy(out, got)
	}
	return out, failed, nil
}

// templateNames lists the templates the engine executes, by the names it reports them under.
func templateNames(ch *chart.Chart, prefix string) []string {
	var names []string
	for _, f := range ch.Templates {
		if !isPartial(f.Name) {
			names = append(names, path.Join(prefix, f.Name))
		}
	}
	for _, dep := range ch.Dependencies() {
		names = append(names, templateNames(dep, path.Join(prefix, "charts", dep.Name()))...)
	}
	slices.Sort(names)
	return names
}

// only returns a copy of ch in which every executable template except keep is renamed to a partial: the engine
// still parses it, so the blocks it defines stay available to keep, but does not execute it.
func only(ch *chart.Chart, prefix, keep string) *chart.Chart {
	c := *ch
	c.Templates = make([]*common.File, len(ch.Templates))
	for i, f := range ch.Templates {
		cp := *f
		if !isPartial(f.Name) && path.Join(prefix, f.Name) != keep {
			cp.Name = path.Join(path.Dir(f.Name), "_helm-debug-skip-"+path.Base(f.Name))
		}
		c.Templates[i] = &cp
	}
	var deps []*chart.Chart
	for _, dep := range ch.Dependencies() {
		deps = append(deps, only(dep, path.Join(prefix, "charts", dep.Name()), keep))
	}
	c.SetDependencies(deps...)
	return &c
}

func isPartial(name string) bool { return path.Base(name)[0] == '_' }
