package compare

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/glueops/argocd-app-chart-preflight/internal/manifests"
	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

func render(t *testing.T, stream string) []PodTemplate {
	t.Helper()
	ts, err := manifests.ParseAndReduce([]byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func fixture(t *testing.T, name string) []PodTemplate {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return render(t, string(b))
}

// deployment renders a minimal Deployment with the given container fields.
func deployment(name string, c map[string]any, initArgs []any, extraLabel string) string {
	container := map[string]any{"name": name, "image": "img:1"}
	for k, v := range c {
		container[k] = v
	}
	labels := map[string]any{"app": name}
	if extraLabel != "" {
		labels["extra"] = extraLabel
	}
	spec := map[string]any{"containers": []any{container}}
	if initArgs != nil {
		spec["initContainers"] = []any{map[string]any{"name": "init", "image": "busybox", "args": initArgs}}
	}
	doc := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": name, "labels": map[string]any{"helm.sh/chart": "app-x"}},
		"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": spec}}}
	b, _ := json.Marshal(doc)
	return "---\n" + string(b) + "\n"
}

func TestVenusRendersIdenticalAtV0171AndPR147(t *testing.T) {
	r := Compare(fixture(t, "keda-demo-newrelic-nonprod.v0.17.1.yaml"), fixture(t, "keda-demo-newrelic-nonprod.pr147.yaml"))
	if len(r.Diff) != 0 || len(r.OtherChanges) != 0 || r.Impact != "" {
		t.Fatalf("expected identical renders, got %+v", r)
	}
}

func TestV0120ToV0171ChangesLabelsNotArgs(t *testing.T) {
	r := Compare(fixture(t, "keda-demo-newrelic-nonprod.v0.12.0-rc1.yaml"), fixture(t, "keda-demo-newrelic-nonprod.v0.17.1.yaml"))
	if len(r.Diff) != 0 || r.Impact != "" {
		t.Fatalf("expected no command/args diff, got %+v", r.Diff)
	}
	paths := r.OtherChanges["Deployment/keda-demo-newrelic"]
	if len(paths) == 0 {
		t.Fatal("expected label/annotation paths")
	}
	found := false
	for _, p := range paths {
		if !strings.HasPrefix(p, "spec.template.") {
			t.Errorf("path %q not under spec.template", p)
		}
		if strings.Contains(p, "helm.sh/chart") {
			found = true
		}
	}
	if !found {
		t.Errorf("helm.sh/chart path expected in %v", paths)
	}
}

func TestChangedArgsItemBecomesKeyedFinding(t *testing.T) {
	r := Compare(render(t, deployment("web", map[string]any{"args": []any{"set -e && run", "--flag"}}, nil, "")),
		render(t, deployment("web", map[string]any{"args": []any{`"set -e && run"`, "--flag"}}, nil, "")))
	rd, ok := r.Diff["Deployment/web"]
	if !ok || len(r.Diff) != 1 {
		t.Fatalf("diff keys = %v", r.Diff)
	}
	f := rd.Containers["web"].Args["0"]
	if f.Old != "set -e && run" || f.New != `"set -e && run"` || f.Category != schema.CategoryDoubleQuoted || f.Impact != schema.ImpactBreak {
		t.Errorf("finding = %+v", f)
	}
	if _, present := rd.Containers["web"].Args["1"]; present {
		t.Error("unchanged index must be absent")
	}
	if r.Impact != schema.ImpactBreak || len(r.OtherChanges) != 0 {
		t.Errorf("impact=%q other=%v", r.Impact, r.OtherChanges)
	}
}

func TestCommandAndInitContainersWalkedImpactIsMax(t *testing.T) {
	r := Compare(render(t, deployment("web", map[string]any{"command": []any{"/bin/sh", "-c"}, "args": []any{"x"}}, []any{"--note"}, "")),
		render(t, deployment("web", map[string]any{"command": []any{"/bin/sh", "-c "}, "args": []any{"x"}}, []any{"--note #keep"}, "")))
	rd := r.Diff["Deployment/web"]
	if rd.Containers["web"].Command["1"].Category != schema.CategoryPadded {
		t.Errorf("command finding = %+v", rd.Containers["web"].Command)
	}
	if rd.InitContainers["init"].Args["0"].Category != schema.CategoryCommentTruncated {
		t.Errorf("init finding = %+v", rd.InitContainers["init"].Args)
	}
	if rd.Containers["web"].Args != nil {
		t.Error("args unchanged must be nil")
	}
	if r.Impact != schema.ImpactRoll {
		t.Errorf("impact = %q", r.Impact)
	}
}

func TestLengthChangeReportsMissingSideAsNull(t *testing.T) {
	r := Compare(render(t, deployment("web", map[string]any{"args": []any{"a", "b"}}, nil, "")), render(t, deployment("web", map[string]any{"args": []any{"a"}}, nil, "")))
	f := r.Diff["Deployment/web"].Containers["web"].Args["1"]
	if f.Old != "b" || f.New != nil || f.Category != schema.CategoryLengthChanged {
		t.Errorf("finding = %+v", f)
	}
}

func TestNullToEmptyIsImpactNone(t *testing.T) {
	r := Compare(render(t, deployment("web", map[string]any{"args": []any{nil}}, nil, "")), render(t, deployment("web", map[string]any{"args": []any{""}}, nil, "")))
	if f := r.Diff["Deployment/web"].Containers["web"].Args["0"]; f.Category != schema.CategoryNullItem || r.Impact != schema.ImpactNone {
		t.Errorf("finding=%+v impact=%q", f, r.Impact)
	}
}

func TestRetypedScalarIsNotEqual(t *testing.T) {
	r := Compare(render(t, deployment("web", map[string]any{"args": []any{8080}}, nil, "")), render(t, deployment("web", map[string]any{"args": []any{"8080"}}, nil, "")))
	if f := r.Diff["Deployment/web"].Containers["web"].Args["0"]; f.Category != schema.CategoryRetyped {
		t.Errorf("finding = %+v", f)
	}
}

func TestResourceOnOneSideGoesToOtherChanges(t *testing.T) {
	r := Compare(render(t, deployment("a", map[string]any{"args": []any{"x"}}, nil, "")),
		render(t, deployment("a", map[string]any{"args": []any{"x"}}, nil, "")+deployment("b", map[string]any{"args": []any{"y"}}, nil, "")))
	if len(r.Diff) != 0 || !reflect.DeepEqual(r.OtherChanges, map[string][]string{"Deployment/b": {"(resource added at target revision)"}}) {
		t.Errorf("got %+v", r)
	}
}

func TestOtherChangesArePathsOnly(t *testing.T) {
	r := Compare(render(t, deployment("web", map[string]any{"args": []any{"x"}}, nil, "")), render(t, deployment("web", map[string]any{"args": []any{"x"}, "image": "img:2"}, nil, "lbl")))
	if len(r.Diff) != 0 {
		t.Errorf("diff = %v", r.Diff)
	}
	want := []string{"spec.template.metadata.labels.extra", "spec.template.spec.containers[0].image"}
	if !reflect.DeepEqual(r.OtherChanges["Deployment/web"], want) {
		t.Errorf("paths = %v, want %v", r.OtherChanges["Deployment/web"], want)
	}
}

func TestDiffPaths(t *testing.T) {
	got := DiffPaths(map[string]any{"a": 1.0, "b": []any{1.0, 2.0}}, map[string]any{"a": 1.0, "b": []any{1.0, 3.0}, "c": 1.0}, "")
	if !reflect.DeepEqual(got, []string{"b[1]", "c"}) {
		t.Errorf("got %v", got)
	}
	if DiffPaths(1.0, 1.0, "") != nil {
		t.Error("equal values must yield nil")
	}
	if !reflect.DeepEqual(DiffPaths(1.0, 2.0, ""), []string{"(root)"}) {
		t.Error("root scalar diff")
	}
}
