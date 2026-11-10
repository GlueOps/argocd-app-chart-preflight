package textreport

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/glueops/argocd-app-chart-preflight/internal/manifests"
	"github.com/glueops/argocd-app-chart-preflight/internal/report"
	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func render(t *testing.T, name string) []manifests.PodTemplate {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := manifests.ParseAndReduce(b)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// setArg replaces args[i] of the first container of every template (a deep copy).
func setArg(ts []manifests.PodTemplate, i int, v string) []manifests.PodTemplate {
	out := make([]manifests.PodTemplate, len(ts))
	for n, t := range ts {
		cp := manifests.PodTemplate{Kind: t.Kind, Name: t.Name, Template: deepCopy(t.Template).(map[string]any)}
		spec := cp.Template["spec"].(map[string]any)
		c := spec["containers"].([]any)[0].(map[string]any)
		args, _ := c["args"].([]any)
		for len(args) <= i {
			args = append(args, "")
		}
		args[i] = v
		c["args"] = args
		out[n] = cp
	}
	return out
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = deepCopy(e)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, e := range x {
			s[i] = deepCopy(e)
		}
		return s
	}
	return v
}

// The real venus renders at v0.17.1 and at the #147 branch differ only in
// chart-version labels and annotations, which the diff leaves out.
func TestPodTemplateDiffLeavesChartKeysOut(t *testing.T) {
	cur, tgt := render(t, "keda-demo-newrelic-nonprod.v0.17.1.yaml"), render(t, "keda-demo-newrelic-nonprod.pr147.yaml")
	if len(cur) == 0 {
		t.Fatal("no pod templates in the fixture")
	}
	if d := PodTemplateDiff(Pair{Current: cur, Target: tgt}, "v0.17.1", "fix/container-args-quoting"); d != "" {
		t.Errorf("chart-only bump produced a diff:\n%s", d)
	}
	changed := setArg(tgt, 0, `"echo a: b"`)
	d := PodTemplateDiff(Pair{Current: setArg(cur, 0, "echo a: b"), Target: changed}, "v0.17.1", "0.18.0")
	for _, want := range []string{"--- " + cur[0].Key() + " @ v0.17.1", "+++ " + cur[0].Key() + " @ 0.18.0", "-    - 'echo a: b'", `+    - '"echo a: b"'`} {
		if !strings.Contains(d, want) {
			t.Errorf("diff lacks %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "helm.sh/chart") {
		t.Errorf("chart label leaked into the diff:\n%s", d)
	}
}

func finding(old, new any, cat, impact, hint string) schema.Finding {
	return schema.Finding{Old: old, New: new, Category: cat, Impact: impact, Hint: hint}
}

func app(name, status string, mod func(*schema.App)) schema.App {
	a := schema.NewApp(name, "nonprod")
	a.Status = status
	a.SyncStatus = schema.Str("Synced")
	a.CurrentRevision = schema.Str("v0.17.1")
	a.ValuesFiles = []string{"common/common-values.yaml", "apps/" + name + "/envs/nonprod/values.yaml"}
	if mod != nil {
		mod(&a)
	}
	return a
}

func set(name, src, cur, tgt string, apps ...schema.App) schema.AppSet {
	s := schema.AppSet{Name: name, Apps: apps, Summary: report.SummarizeApps(apps), Status: report.GroupStatus(apps)}
	if src != "" {
		s.SourceType, s.CurrentRevision = schema.Str(src), schema.Str(cur)
	}
	s.TargetRevision = schema.Str(tgt)
	return s
}

func sample() (*schema.Report, Renders) {
	quoted := app("keda-demo-sqs", schema.AppDiff, func(a *schema.App) {
		a.Diff = map[string]schema.ResourceDiff{"Deployment/keda-demo-sqs": {Containers: map[string]schema.ContainerDiff{"app": {Args: map[string]schema.Finding{
			"0": finding("echo a: b", `"echo a: b"`, schema.CategoryDoubleQuoted, schema.ImpactBreak, "remove the outer pair of quotes before upgrading")}}}}}
	})
	padded := app("worker", schema.AppDiff, func(a *schema.App) {
		a.SyncStatus = schema.Str("OutOfSync")
		a.Diff = map[string]schema.ResourceDiff{"CronJob/worker": {
			Containers:     map[string]schema.ContainerDiff{"worker": {Args: map[string]schema.Finding{"1": finding("padded", "  padded  ", schema.CategoryPadded, schema.ImpactRoll, "leading/trailing spaces are now preserved")}}},
			InitContainers: map[string]schema.ContainerDiff{"setup": {Command: map[string]schema.Finding{"0": finding(nil, "", schema.CategoryNullItem, schema.ImpactNone, "no rollout")}}}}}
		a.OtherChanges = map[string][]string{"CronJob/worker": {"spec.template.spec.containers[worker].env"}}
	})
	long := app("long-arg", schema.AppDiff, func(a *schema.App) {
		v := "python -m worker --queue=very-long-queue-name-that-goes-on --concurrency=8 #tuned"
		a.Diff = map[string]schema.ResourceDiff{"Deployment/long-arg": {Containers: map[string]schema.ContainerDiff{"app": {Args: map[string]schema.Finding{
			"0": finding(strings.Split(v, " #")[0], v, schema.CategoryCommentTruncated, schema.ImpactRoll, "the text after ' #' was a YAML comment before and is now part of the argument")}}}}}
	})
	broken := app("keda-demo-map", schema.AppError, func(a *schema.App) {
		a.Reason = schema.Str("deployment.args: every item must be a string (got map) - quote the item in your values file")
	})
	lost := app("flaky", schema.AppUnknown, func(a *schema.App) {
		a.Reason = schema.Str("argocd app manifests (target) failed: dial tcp: connection refused")
	})
	platform := app("glueops-alerts", schema.AppSkipped, func(a *schema.App) {
		a.Reason = schema.Str(`target "v0.18.0" is a git ref; this app sources the chart from a Helm repository and needs X.Y.Z`)
	})
	platform2 := platform
	platform2.Name = "cluster-info-page"
	sets := []schema.AppSet{
		set("nonprod-application-set", schema.SourceGit, "v0.17.1", "v0.18.0", app("back-end", schema.AppOK, nil), app("front-end", schema.AppOKNoopDiff, nil)),
		set("prod-application-set", schema.SourceGit, "v0.17.1", "v0.18.0", quoted, padded, long, app("api", schema.AppOK, nil), broken),
		set("previews-application-set", schema.SourceHelm, "0.17.1", "0.18.0", lost, app("pr-1", schema.AppOK, nil)),
		set("standalone", "", "", "v0.18.0", platform, platform2),
	}
	rep := &schema.Report{SchemaVersion: 1, Run: schema.Run{ID: "r", CaptainDomain: "nonprod.venus.onglueops.rocks", TargetRevision: schema.Str("v0.18.0"), ArgocdCLIVersion: schema.Str("v3.4.12+590e562"), Status: report.RunStatus(sets)}, AppSets: sets}
	rep.Summary = report.SummarizeSets(sets, 18)
	tpl := func(kind, name string, args ...any) manifests.PodTemplate {
		return manifests.PodTemplate{Kind: kind, Name: name, Template: map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"app": name, "helm.sh/chart": "app-0.17.1"}},
			"spec":     map[string]any{"containers": []any{map[string]any{"name": "app", "image": "img:1", "args": args}}}}}
	}
	renders := Renders{
		Key("prod-application-set", "keda-demo-sqs"): {
			Current: []manifests.PodTemplate{tpl("Deployment", "keda-demo-sqs", "echo a: b")},
			Target:  []manifests.PodTemplate{tpl("Deployment", "keda-demo-sqs", `"echo a: b"`)},
		},
	}
	return rep, renders
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/textreport -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs; got:\n%s", name, got)
	}
}

func TestWriteGolden(t *testing.T) {
	for name, o := range map[string]Options{
		"default.txt":  {Diff: true, Context: "k3d-captain", Duration: 7*time.Minute + 12*time.Second},
		"all-wide.txt": {Diff: true, All: true, Wide: true, Context: "k3d-captain"},
		"no-diff.txt":  {},
	} {
		t.Run(name, func(t *testing.T) {
			rep, renders := sample()
			var b bytes.Buffer
			if err := Write(&b, rep, renders, o); err != nil {
				t.Fatal(err)
			}
			golden(t, name, b.Bytes())
		})
	}
}

// Colour changes nothing but escape codes: the columns still line up.
func TestColourOnlyAddsEscapes(t *testing.T) {
	rep, renders := sample()
	var plainOut, coloured bytes.Buffer
	_ = Write(&plainOut, rep, renders, Options{Diff: true})
	_ = Write(&coloured, rep, renders, Options{Diff: true, Color: true})
	if !strings.Contains(coloured.String(), "\x1b[31;1m") {
		t.Fatal("no colour in coloured output")
	}
	if got := ansi.ReplaceAllString(coloured.String(), ""); got != plainOut.String() {
		t.Errorf("coloured output differs from plain once escapes are stripped:\n%s", got)
	}
}

func TestValue(t *testing.T) {
	for v, want := range map[any]string{"  padded  ": `"  padded  "`, `"q"`: `"\"q\""`, "": `""`, true: "true", 8080.0: "8080"} {
		if got := Value(v); got != want {
			t.Errorf("Value(%#v) = %s, want %s", v, got, want)
		}
	}
	if Value(nil) != "(none)" || Value(map[string]any{"a": "b"}) != `{"a":"b"}` {
		t.Error("nil or map value rendered wrong")
	}
}
