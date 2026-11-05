package report

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

func app(name, status string) schema.App {
	a := schema.NewApp(name, "nonprod")
	a.CurrentRevision = schema.Str("v0.17.1")
	a.Status = status
	a.SyncStatus = schema.Str("Synced")
	return a
}

func set(name, status string, apps ...schema.App) schema.AppSet {
	if apps == nil {
		apps = []schema.App{}
	}
	return schema.AppSet{Name: name, SourceType: schema.Str(schema.SourceGit), CurrentRevision: schema.Str("v0.17.1"), TargetRevision: schema.Str("v0.18.0"), Status: status, Summary: SummarizeApps(apps), Apps: apps}
}

func TestStatusLadder(t *testing.T) {
	cases := []struct {
		apps []schema.App
		want string
	}{
		{[]schema.App{app("a", schema.AppOK), app("b", schema.AppOKNoopDiff)}, schema.GroupOK},
		{[]schema.App{app("a", schema.AppOK), app("b", schema.AppUnknown)}, schema.GroupUnknown},
		{[]schema.App{app("a", schema.AppUnknown), app("b", schema.AppDiff)}, schema.GroupDiff},
		{[]schema.App{app("a", schema.AppDiff), app("b", schema.AppError)}, schema.GroupError},
		{[]schema.App{app("a", schema.AppSkipped)}, schema.GroupSkipped},
		{nil, schema.GroupSkipped},
		{[]schema.App{app("a", schema.AppSkipped), app("b", schema.AppOK)}, schema.GroupOK},
	}
	for _, c := range cases {
		if got := GroupStatus(c.apps); got != c.want {
			t.Errorf("%v: got %s, want %s", c.apps, got, c.want)
		}
	}
	if RunStatus([]schema.AppSet{set("s1", schema.GroupOK), set("s2", schema.GroupSkipped)}) != schema.GroupOK {
		t.Error("ok + skipped must be ok")
	}
	if RunStatus([]schema.AppSet{set("s1", schema.GroupDiff), set("s2", schema.GroupError)}) != schema.GroupError {
		t.Error("error beats diff")
	}
	if RunStatus([]schema.AppSet{set("s1", schema.GroupUnknown), set("s2", schema.GroupOK)}) != schema.GroupUnknown {
		t.Error("unknown beats ok")
	}
	if RunStatus(nil) != schema.GroupSkipped {
		t.Error("no sets is skipped")
	}
}

func TestSummaries(t *testing.T) {
	apps := []schema.App{app("a", schema.AppOK), app("b", schema.AppOKNoopDiff), app("c", schema.AppDiff), app("d", schema.AppError), app("e", schema.AppUnknown), app("f", schema.AppSkipped)}
	if got := SummarizeApps(apps); got != (schema.AppSummary{Apps: 6, OK: 1, OKNoopDiff: 1, Diff: 1, Error: 1, Unknown: 1, Skipped: 1}) {
		t.Errorf("apps summary = %+v", got)
	}
	if got := SummarizeSets([]schema.AppSet{set("a", schema.GroupOK), set("b", schema.GroupDiff), set("c", schema.GroupSkipped)}, 7); got != (schema.RunSummary{AppSets: 3, OK: 1, Diff: 1, Skipped: 1, AppsIgnored: 7}) {
		t.Errorf("sets summary = %+v", got)
	}
}

func sample() *schema.Report {
	a := app("keda-demo-newrelic-nonprod", schema.AppDiff)
	a.ValuesFiles = []string{"apps/keda-demo-newrelic/envs/nonprod/values.yaml"}
	a.Diff["Deployment/keda-demo-newrelic"] = schema.ResourceDiff{Containers: map[string]schema.ContainerDiff{"keda-demo-newrelic": {Args: map[string]schema.Finding{
		"0": {Old: "set -e && run", New: `"set -e && run"`, Category: schema.CategoryDoubleQuoted, Impact: schema.ImpactBreak, Hint: "remove the outer quotes"},
		"2": {Old: nil, New: "", Category: schema.CategoryNullItem, Impact: schema.ImpactNone, Hint: "no rollout"},
	}}}}
	a.OtherChanges["Deployment/keda-demo-newrelic"] = []string{`spec.template.metadata.labels["helm.sh/chart"]`}
	s := set("nonprod-application-set", schema.GroupDiff, a)
	return &schema.Report{SchemaVersion: 1,
		Run:     schema.Run{ID: "20261104100000_0123abcd", CaptainDomain: "nonprod.venus.onglueops.rocks", TargetRevision: schema.Str("v0.18.0"), ArgocdCLIVersion: schema.Str("v3.2.12"), AgentVersion: "dev", AgentCommit: "local", StartedAt: "2026-11-04T10:00:00.000Z", FinishedAt: "2026-11-04T10:00:05.000Z", Status: schema.GroupDiff},
		Summary: SummarizeSets([]schema.AppSet{s}, 30), AppSets: []schema.AppSet{s}}
}

func TestToYAMLQuotingOrderAndRoundTrip(t *testing.T) {
	b, err := ToYAML(sample())
	if err != nil {
		t.Fatal(err)
	}
	y := string(b)
	for _, re := range []string{
		`(?m)^schema_version: 1$`,
		`(?m)^run:$`,
		`(?m)^\s+old: "set -e && run"$`,
		`(?m)^\s+new: "\\"set -e && run\\""$`,
		`(?m)^\s+old: null$`,
		`(?m)^\s+new: ""$`,
		`(?m)^\s+"0":$`,
		`(?m)^\s+ok-noop-diff: 0$`,
		`(?m)^\s+Deployment/keda-demo-newrelic:$`,
	} {
		if !regexp.MustCompile(re).MatchString(y) {
			t.Errorf("YAML does not match %s:\n%s", re, y)
		}
	}
	if strings.Contains(y, "&") && regexp.MustCompile(`&[a-z0-9]+\b`).MatchString(y) && strings.Contains(y, "*") {
		t.Error("no anchors/aliases expected")
	}
	// struct order preserved: schema_version, run, summary, appsets
	idx := func(s string) int { return strings.Index(y, "\n"+s+":") }
	if !(idx("run") < idx("summary") && idx("summary") < idx("appsets")) {
		t.Errorf("top-level order wrong:\n%s", y)
	}
	// round-trips to the same JSON
	var fromYAML, fromJSON any
	if err := sigsyaml.Unmarshal(b, &fromYAML); err != nil {
		t.Fatal(err)
	}
	j, _ := ToJSON(sample())
	_ = json.Unmarshal(j, &fromJSON)
	if !reflect.DeepEqual(fromYAML, fromJSON) {
		t.Errorf("YAML and JSON differ:\n%v\n%v", fromYAML, fromJSON)
	}
}

func TestFindingsFlatten(t *testing.T) {
	r := sample()
	f := Findings(&r.AppSets[0].Apps[0])
	if len(f) != 2 {
		t.Fatalf("findings = %d", len(f))
	}
	want := FindingLine{Resource: "Deployment/keda-demo-newrelic", Container: "keda-demo-newrelic", ContainerType: schema.ContainersKey, Field: schema.FieldArgs, Index: "0", Old: "set -e && run", New: `"set -e && run"`, Category: schema.CategoryDoubleQuoted, Impact: schema.ImpactBreak}
	if !reflect.DeepEqual(f[0], want) {
		t.Errorf("first = %+v", f[0])
	}
	if f[1].Index != "2" || f[1].Impact != schema.ImpactNone {
		t.Errorf("second = %+v", f[1])
	}
}
