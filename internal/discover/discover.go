// Package discover turns `argocd appset list` + `argocd app list` output into
// the groups the run checks. The unit of a bump is one ApplicationSet's chart
// targetRevision, so apps are grouped by the set that generated them; apps
// nobody generated (hand-written in a captain's manifests/) form "standalone",
// which has no template to bump and is flagged as such in the report.
package discover

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

// SourceRef says where the app chart sits in an Application: a multi-source
// position (1-based, what --source-positions takes) or the single `source`.
type SourceRef struct {
	Single   bool
	Position int
}

// RevisionArgs renders the CLI flags that override the chart source's revision.
func (r SourceRef) RevisionArgs(revision string) []string {
	if r.Single {
		return []string{"--revision", revision}
	}
	return []string{"--revisions", revision, "--source-positions", fmt.Sprint(r.Position)}
}

// ChartSource is the app-chart source of an Application or a set's template.
type ChartSource struct {
	Type        string // schema.SourceHelm | SourceOCI | SourceGit
	Ref         SourceRef
	Revision    string
	ValuesFiles []string
}

// App is one Application as the run sees it.
type App struct {
	Name       string
	Namespace  string // destination namespace (where the pods run)
	Project    string
	SyncStatus *string
	Chart      *ChartSource // nil when the app does not use the app chart
	AppSet     string       // "" when no ApplicationSet generated it
}

// Group is one ApplicationSet, or Standalone.
type Group struct {
	Name            string
	SourceType      *string // nil for Standalone (nothing to bump)
	CurrentRevision *string
	TargetRevision  *string
	SkipReason      string // set ⇒ every app is skipped
	UnknownReason   string // set ⇒ every app is unknown
	Apps            []App
}

// Standalone is the group for owner-less app-chart Applications.
const Standalone = "standalone"

// AppSetLabel is set by Argo CD on every generated Application.
const AppSetLabel = "argocd.argoproj.io/application-set-name"

// Where the chart is published; the three forms a captain's ApplicationSet may use.
var (
	helmRepoRE  = regexp.MustCompile(`(?i)^https?://(incubating-)?helm\.gpkg\.io/project-template/?$`)
	ociRepoRE   = regexp.MustCompile(`(?i)^(oci://)?[a-z0-9.-]+(:[0-9]+)?/glueops/project-template-helm-chart-app/?$`)
	ociNativeRE = regexp.MustCompile(`(?i)^oci://[a-z0-9.-]+(:[0-9]+)?/glueops/project-template-helm-chart-app/app/?$`)
	gitRepoRE   = regexp.MustCompile(`(?i)^(https?://|git@|ssh://)?[^/]*github\.com[/:]glueops/project-template-helm-chart-app(\.git)?/?$`)
	semverRE    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
	valuesRefRE = regexp.MustCompile(`^\$[^/]+/`)
)

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// DetectAppChart recognises the app chart in a source; "" when it is something else.
func DetectAppChart(source map[string]any) string {
	if source == nil {
		return ""
	}
	repo := strings.TrimSpace(str(source, "repoURL"))
	chart := str(source, "chart")
	switch {
	case chart == "app" && helmRepoRE.MatchString(repo):
		return schema.SourceHelm
	case chart == "app" && ociRepoRE.MatchString(repo):
		return schema.SourceOCI
	case chart == "" && ociNativeRE.MatchString(repo):
		return schema.SourceOCI
	case chart == "" && gitRepoRE.MatchString(repo):
		return schema.SourceGit
	}
	return ""
}

func valuesFilesOf(source map[string]any) []string {
	out := []string{}
	vf, _ := obj(source["helm"])["valueFiles"].([]any)
	for _, f := range vf {
		if s, ok := f.(string); ok {
			out = append(out, valuesRefRE.ReplaceAllString(s, ""))
		}
	}
	return out
}

// ChartSourceOf finds the app-chart source in an Application (or set template) spec.
func ChartSourceOf(spec map[string]any) *ChartSource {
	if sources, ok := spec["sources"].([]any); ok {
		for i, s := range sources {
			src := obj(s)
			if typ := DetectAppChart(src); typ != "" {
				return &ChartSource{Type: typ, Ref: SourceRef{Position: i + 1}, Revision: str(src, "targetRevision"), ValuesFiles: valuesFilesOf(src)}
			}
		}
		return nil
	}
	if single := obj(spec["source"]); single != nil {
		if typ := DetectAppChart(single); typ != "" {
			return &ChartSource{Type: typ, Ref: SourceRef{Single: true}, Revision: str(single, "targetRevision"), ValuesFiles: valuesFilesOf(single)}
		}
	}
	return nil
}

// ValidateTarget returns why a target cannot be the right kind of revision for
// the source type, or "" when it fits.
func ValidateTarget(sourceType, target string) string {
	if target == "" {
		return "no target revision"
	}
	if sourceType == schema.SourceGit {
		if semverRE.MatchString(target) {
			return fmt.Sprintf("target %q looks like a chart version; a git-sourced chart needs a git ref such as v%s", target, target)
		}
		return ""
	}
	if !semverRE.MatchString(target) {
		return fmt.Sprintf("target %q is not a chart version; a %s-sourced chart needs X.Y.Z (e.g. %s)", target, sourceType, strings.TrimPrefix(target, "v"))
	}
	return ""
}

// ToApp reads one Application object; nil when it has no name.
func ToApp(raw map[string]any) *App {
	meta := obj(raw["metadata"])
	name := str(meta, "name")
	if name == "" {
		return nil
	}
	appSet := str(obj(meta["labels"]), AppSetLabel)
	if appSet == "" {
		if owners, ok := meta["ownerReferences"].([]any); ok {
			for _, o := range owners {
				if om := obj(o); str(om, "kind") == "ApplicationSet" && str(om, "name") != "" {
					appSet = str(om, "name")
				}
			}
		}
	}
	spec := obj(raw["spec"])
	ns := str(obj(spec["destination"]), "namespace")
	if ns == "" {
		ns = str(meta, "namespace")
	}
	var sync *string
	if s := str(obj(obj(raw["status"])["sync"]), "status"); s != "" {
		sync = schema.Str(s)
	}
	return &App{Name: name, Namespace: ns, Project: str(spec, "project"), SyncStatus: sync, Chart: ChartSourceOf(spec), AppSet: appSet}
}

// Input to Discover.
type Input struct {
	ApplicationSets []map[string]any
	Applications    []map[string]any
	TargetRevision  string
	Targets         map[string]string // per-set override; "" skips the set
}

// Output of Discover. Ignored counts Applications that do not use the app
// chart and belong to no app-chart set (platform apps).
type Output struct {
	Groups  []Group
	Ignored int
}

// Discover groups Applications by ApplicationSet.
func Discover(in Input) Output {
	groups := map[string]*Group{}
	for _, raw := range in.ApplicationSets {
		name := str(obj(raw["metadata"]), "name")
		if name == "" {
			continue
		}
		chart := ChartSourceOf(obj(obj(obj(raw["spec"])["template"])["spec"]))
		if chart == nil {
			continue // a set for something other than the app chart
		}
		target, overridden := in.Targets[name]
		if !overridden {
			target = in.TargetRevision
		}
		g := &Group{Name: name, SourceType: schema.Str(chart.Type), CurrentRevision: schema.Str(chart.Revision), Apps: []App{}}
		if target != "" {
			g.TargetRevision = schema.Str(target)
			g.UnknownReason = ValidateTarget(chart.Type, target)
		} else if overridden {
			g.SkipReason = "skipped by targets override"
		} else {
			g.SkipReason = "no target revision configured"
		}
		groups[name] = g
	}

	standalone := &Group{Name: Standalone, Apps: []App{}}
	if in.TargetRevision != "" {
		standalone.TargetRevision = schema.Str(in.TargetRevision)
	} else {
		standalone.SkipReason = "no target revision configured"
	}
	var ignored int
	for _, raw := range in.Applications {
		app := ToApp(raw)
		if app == nil {
			continue
		}
		if owner := groups[app.AppSet]; app.AppSet != "" && owner != nil {
			owner.Apps = append(owner.Apps, *app)
		} else if app.Chart != nil {
			standalone.Apps = append(standalone.Apps, *app)
		} else {
			ignored++
		}
	}

	out := Output{Ignored: ignored}
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out.Groups = append(out.Groups, *groups[n])
	}
	if len(standalone.Apps) > 0 {
		out.Groups = append(out.Groups, *standalone)
	}
	for i := range out.Groups {
		sort.Slice(out.Groups[i].Apps, func(a, b int) bool { return out.Groups[i].Apps[a].Name < out.Groups[i].Apps[b].Name })
	}
	return out
}
