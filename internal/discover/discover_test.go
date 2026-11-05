package discover

import (
	"reflect"
	"strings"
	"testing"

	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

var venusValuesSrc = map[string]any{"repoURL": "https://github.com/development-tenant-venus/deployment-configurations", "targetRevision": "main", "ref": "values"}

func gitChartSrc(rev string) map[string]any {
	return map[string]any{
		"repoURL": "https://github.com/GlueOps/project-template-helm-chart-app.git", "path": ".", "targetRevision": rev,
		"helm": map[string]any{"valueFiles": []any{"$values/common/common-values.yaml", "$values/env-overlays/nonprod/env-values.yaml", "$values/apps/keda-demo-newrelic/base/base-values.yaml", "$values/apps/keda-demo-newrelic/envs/nonprod/values.yaml"}},
	}
}

func helmChartSrc(rev string) map[string]any {
	return map[string]any{"repoURL": "https://helm.gpkg.io/project-template", "chart": "app", "targetRevision": rev, "helm": map[string]any{"valueFiles": []any{"$values/apps/x/envs/prod/values.yaml"}}}
}

type appOpts struct {
	name, appSet, ownerRef, project, sync string
	sources                               []any
	source                                map[string]any
}

func application(o appOpts) map[string]any {
	meta := map[string]any{"name": o.name, "namespace": "glueops-core", "labels": map[string]any{}}
	if o.appSet != "" {
		meta["labels"] = map[string]any{AppSetLabel: o.appSet}
	}
	if o.ownerRef != "" {
		meta["ownerReferences"] = []any{map[string]any{"kind": "ApplicationSet", "name": o.ownerRef}}
	}
	project := o.project
	if project == "" {
		project = "nonprod"
	}
	spec := map[string]any{"project": project, "destination": map[string]any{"namespace": "nonprod"}}
	if o.sources != nil {
		spec["sources"] = o.sources
	}
	if o.source != nil {
		spec["source"] = o.source
	}
	sync := o.sync
	if sync == "" {
		sync = "Synced"
	}
	return map[string]any{"metadata": meta, "spec": spec, "status": map[string]any{"sync": map[string]any{"status": sync}}}
}

func applicationSet(name string, chartSrc map[string]any) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name, "namespace": "glueops-core"}, "spec": map[string]any{"generators": []any{map[string]any{"git": map[string]any{}}},
		"template": map[string]any{"spec": map[string]any{"project": "nonprod", "sources": []any{chartSrc, venusValuesSrc}}}}}
}

func TestDetectAppChart(t *testing.T) {
	cases := map[string]struct {
		src  map[string]any
		want string
	}{
		"helm repo":            {map[string]any{"repoURL": "https://helm.gpkg.io/project-template", "chart": "app"}, schema.SourceHelm},
		"incubating helm repo": {map[string]any{"repoURL": "https://incubating-helm.gpkg.io/project-template", "chart": "app"}, schema.SourceHelm},
		"oci bare host":        {map[string]any{"repoURL": "ghcr.repo.gpkg.io/glueops/project-template-helm-chart-app", "chart": "app"}, schema.SourceOCI},
		"oci scheme + chart":   {map[string]any{"repoURL": "oci://ghcr.io/glueops/project-template-helm-chart-app", "chart": "app"}, schema.SourceOCI},
		"oci native source":    {map[string]any{"repoURL": "oci://ghcr.repo.gpkg.io/glueops/project-template-helm-chart-app/app", "path": "."}, schema.SourceOCI},
		"git https .git":       {map[string]any{"repoURL": "https://github.com/GlueOps/project-template-helm-chart-app.git", "path": "."}, schema.SourceGit},
		"git https":            {map[string]any{"repoURL": "https://github.com/GlueOps/project-template-helm-chart-app", "path": "."}, schema.SourceGit},
		"git ssh":              {map[string]any{"repoURL": "git@github.com:GlueOps/project-template-helm-chart-app.git"}, schema.SourceGit},
		"service chart":        {map[string]any{"repoURL": "https://helm.gpkg.io/project-template", "chart": "service"}, ""},
		"platform repo":        {map[string]any{"repoURL": "https://helm.gpkg.io/platform", "chart": "app"}, ""},
		"tenant values":        {map[string]any{"repoURL": "https://github.com/development-tenant-venus/deployment-configurations", "ref": "values"}, ""},
		"traefik":              {map[string]any{"repoURL": "https://traefik.github.io/charts", "chart": "traefik"}, ""},
		"git with chart:":      {map[string]any{"repoURL": "https://github.com/GlueOps/project-template-helm-chart-app.git", "chart": "app"}, ""},
		"nil":                  {nil, ""},
	}
	for name, c := range cases {
		if got := DetectAppChart(c.src); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestChartSourceOf(t *testing.T) {
	c := ChartSourceOf(map[string]any{"sources": []any{venusValuesSrc, gitChartSrc("v0.17.1")}})
	want := &ChartSource{Type: schema.SourceGit, Ref: SourceRef{Position: 2}, Revision: "v0.17.1",
		ValuesFiles: []string{"common/common-values.yaml", "env-overlays/nonprod/env-values.yaml", "apps/keda-demo-newrelic/base/base-values.yaml", "apps/keda-demo-newrelic/envs/nonprod/values.yaml"}}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("multi: got %+v, want %+v", c, want)
	}
	if got := c.Ref.RevisionArgs("v0.18.0"); !reflect.DeepEqual(got, []string{"--revisions", "v0.18.0", "--source-positions", "2"}) {
		t.Errorf("revision args = %v", got)
	}
	s := ChartSourceOf(map[string]any{"source": helmChartSrc("0.17.1")})
	if s == nil || !s.Ref.Single || s.Type != schema.SourceHelm || !reflect.DeepEqual(s.ValuesFiles, []string{"apps/x/envs/prod/values.yaml"}) {
		t.Errorf("single: got %+v", s)
	}
	if got := s.Ref.RevisionArgs("0.18.0"); !reflect.DeepEqual(got, []string{"--revision", "0.18.0"}) {
		t.Errorf("single revision args = %v", got)
	}
	if ChartSourceOf(map[string]any{"sources": []any{venusValuesSrc}}) != nil || ChartSourceOf(map[string]any{}) != nil {
		t.Error("non-app-chart specs must yield nil")
	}
}

func TestValidateTarget(t *testing.T) {
	ok := map[[2]string]bool{{schema.SourceHelm, "0.18.0"}: true, {schema.SourceOCI, "0.18.0-rc1"}: true, {schema.SourceGit, "v0.18.0"}: true, {schema.SourceGit, "fix/container-args-quoting"}: true}
	for k, want := range ok {
		if got := ValidateTarget(k[0], k[1]) == ""; got != want {
			t.Errorf("%v: valid=%v", k, got)
		}
	}
	if r := ValidateTarget(schema.SourceGit, "0.18.0"); !strings.Contains(r, "needs a git ref such as v0.18.0") {
		t.Errorf("git/semver reason = %q", r)
	}
	if r := ValidateTarget(schema.SourceHelm, "v0.18.0"); !strings.Contains(r, "needs X.Y.Z (e.g. 0.18.0)") {
		t.Errorf("helm/v reason = %q", r)
	}
	if r := ValidateTarget(schema.SourceOCI, "main"); !strings.Contains(r, "not a chart version") {
		t.Errorf("oci/branch reason = %q", r)
	}
	if ValidateTarget(schema.SourceHelm, "") != "no target revision" {
		t.Error("empty target")
	}
}

func TestToApp(t *testing.T) {
	a := ToApp(application(appOpts{name: "a", appSet: "s1", ownerRef: "s2", sources: []any{gitChartSrc("v1"), venusValuesSrc}, sync: "OutOfSync"}))
	if a.AppSet != "s1" || *a.SyncStatus != "OutOfSync" || a.Chart.Revision != "v1" || a.Namespace != "nonprod" {
		t.Errorf("by label: %+v", a)
	}
	if b := ToApp(application(appOpts{name: "b", ownerRef: "s2", sources: []any{gitChartSrc("v1")}})); b.AppSet != "s2" {
		t.Errorf("by owner: %+v", b)
	}
	if c := ToApp(application(appOpts{name: "c", sources: []any{venusValuesSrc}})); c.Chart != nil {
		t.Errorf("no chart: %+v", c)
	}
	if ToApp(map[string]any{"metadata": map[string]any{}}) != nil {
		t.Error("nameless app must be nil")
	}
}

func TestDiscoverGroups(t *testing.T) {
	out := Discover(Input{
		ApplicationSets: []map[string]any{applicationSet("nonprod-application-set", gitChartSrc("v0.17.1")), applicationSet("previews-application-set", helmChartSrc("0.17.1")),
			{"metadata": map[string]any{"name": "other-set"}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"sources": []any{map[string]any{"repoURL": "https://traefik.github.io/charts", "chart": "traefik"}}}}}}},
		Applications: []map[string]any{
			application(appOpts{name: "b-app", appSet: "nonprod-application-set", sources: []any{gitChartSrc("v0.17.1"), venusValuesSrc}}),
			application(appOpts{name: "a-app", appSet: "nonprod-application-set", sources: []any{gitChartSrc("v0.17.1"), venusValuesSrc}}),
			application(appOpts{name: "pr-1", appSet: "previews-application-set", sources: []any{helmChartSrc("0.17.1"), venusValuesSrc}}),
			application(appOpts{name: "hand-made", sources: []any{helmChartSrc("0.16.1"), venusValuesSrc}}),
			application(appOpts{name: "traefik", appSet: "other-set", sources: []any{map[string]any{"repoURL": "https://traefik.github.io/charts", "chart": "traefik"}}}),
			application(appOpts{name: "platform-thing", source: map[string]any{"repoURL": "https://helm.gpkg.io/platform", "chart": "x"}}),
		},
		TargetRevision: "0.18.0",
		Targets:        map[string]string{"nonprod-application-set": "v0.18.0"},
	})
	var names []string
	for _, g := range out.Groups {
		names = append(names, g.Name)
	}
	if want := []string{"nonprod-application-set", "previews-application-set", Standalone}; !reflect.DeepEqual(names, want) {
		t.Fatalf("groups = %v", names)
	}
	np, pv, sa := out.Groups[0], out.Groups[1], out.Groups[2]
	if np.Apps[0].Name != "a-app" || np.Apps[1].Name != "b-app" {
		t.Errorf("apps not sorted: %v", np.Apps)
	}
	if *np.SourceType != schema.SourceGit || *np.CurrentRevision != "v0.17.1" || *np.TargetRevision != "v0.18.0" || np.UnknownReason != "" || np.SkipReason != "" {
		t.Errorf("nonprod group = %+v", np)
	}
	if *pv.TargetRevision != "0.18.0" {
		t.Errorf("previews target = %v", pv.TargetRevision)
	}
	if sa.SourceType != nil || sa.CurrentRevision != nil || len(sa.Apps) != 1 || sa.Apps[0].Name != "hand-made" {
		t.Errorf("standalone = %+v", sa)
	}
	if out.Ignored != 2 {
		t.Errorf("ignored = %d, want 2 (traefik set + platform app)", out.Ignored)
	}
}

func TestDiscoverSkipAndUnknown(t *testing.T) {
	out := Discover(Input{ApplicationSets: []map[string]any{applicationSet("s-skip", gitChartSrc("v0.17.1")), applicationSet("s-bad", gitChartSrc("v0.17.1"))}, TargetRevision: "0.18.0", Targets: map[string]string{"s-skip": ""}})
	bad, skip := out.Groups[0], out.Groups[1]
	if skip.Name != "s-skip" || skip.SkipReason != "skipped by targets override" || skip.TargetRevision != nil {
		t.Errorf("skip = %+v", skip)
	}
	if bad.Name != "s-bad" || !strings.Contains(bad.UnknownReason, "looks like a chart version") {
		t.Errorf("bad = %+v", bad)
	}
	none := Discover(Input{ApplicationSets: []map[string]any{applicationSet("s", gitChartSrc("v0.17.1"))}, Applications: []map[string]any{application(appOpts{name: "x", sources: []any{gitChartSrc("v0.17.1")}})}})
	for _, g := range none.Groups {
		if g.SkipReason != "no target revision configured" || g.UnknownReason != "" {
			t.Errorf("no target: %+v", g)
		}
	}
}

func TestValidateTargetHintOnlyWhenItLooksLikeAVersion(t *testing.T) {
	if r := ValidateTarget(schema.SourceHelm, "fix/container-args-quoting"); strings.Contains(r, "e.g.") {
		t.Errorf("a branch must not be suggested as a version: %q", r)
	}
	if r := ValidateTarget(schema.SourceOCI, "v0.18.0"); !strings.Contains(r, "(e.g. 0.18.0)") {
		t.Errorf("vX.Y.Z should suggest X.Y.Z: %q", r)
	}
}
