package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const schemaPath = "../../schema/report.schema.json"

func loadSchema(t *testing.T) jsonSchema {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(schemaPath))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var s jsonSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	return s
}

// SampleReport is a fully populated report exercising every field: a diff app
// with findings in containers and initContainers, an error app, a skipped
// standalone group, nulls where the schema allows them.
func SampleReport() Report {
	diffApp := NewApp("keda-demo-newrelic-nonprod", "nonprod")
	diffApp.CurrentRevision = Str("v0.17.1")
	diffApp.Status = AppDiff
	diffApp.SyncStatus = Str("Synced")
	diffApp.ValuesFiles = []string{"common/common-values.yaml", "apps/keda-demo-newrelic/envs/nonprod/values.yaml"}
	diffApp.Diff["Deployment/keda-demo-newrelic"] = ResourceDiff{
		Containers: map[string]ContainerDiff{"keda-demo-newrelic": {
			Args:    map[string]Finding{"0": {Old: "a", New: "\"a\"", Category: CategoryDoubleQuoted, Impact: ImpactBreak, Hint: "remove the outer quotes"}},
			Command: map[string]Finding{"1": {Old: float64(8080), New: "8080", Category: CategoryRetyped, Impact: ImpactRoll, Hint: "h"}},
		}},
		InitContainers: map[string]ContainerDiff{"init": {Args: map[string]Finding{"0": {Old: nil, New: "", Category: CategoryNullItem, Impact: ImpactNone, Hint: "h"}}}},
	}
	diffApp.OtherChanges["Deployment/keda-demo-newrelic"] = []string{"spec.template.metadata.labels.helm.sh/chart"}
	diffApp.ArgocdDiff = Str("===== apps/Deployment nonprod/keda-demo-newrelic ======\n1c1\n< a\n---\n> b")

	errApp := NewApp("keda-demo-sqs-nonprod", "nonprod")
	errApp.CurrentRevision = Str("v0.17.1")
	errApp.Status = AppError
	errApp.Reason = Str("deployment.args: every item must be a string (got map)")

	set := AppSet{Name: "nonprod-application-set", SourceType: Str(SourceGit), CurrentRevision: Str("v0.17.1"), TargetRevision: Str("v0.18.0"), Status: GroupError,
		Summary: AppSummary{Apps: 2, Diff: 1, Error: 1}, Apps: []App{diffApp, errApp}}
	standalone := AppSet{Name: "standalone", TargetRevision: Str("v0.18.0"), Status: GroupSkipped, Reason: Str("no target revision configured"), Apps: []App{}}
	return Report{
		SchemaVersion: SchemaVersion,
		Run: Run{ID: "20261104100000_0123abcd", CaptainDomain: "nonprod.venus.onglueops.rocks", TargetRevision: Str("v0.18.0"), ArgocdCLIVersion: Str("v3.2.12"),
			AgentVersion: "v0.1.0", AgentCommit: "abc", StartedAt: "2026-11-04T10:00:00.000Z", FinishedAt: "2026-11-04T10:00:05.000Z", Status: GroupError},
		Summary: RunSummary{AppSets: 2, Error: 1, Skipped: 1, AppsIgnored: 30},
		AppSets: []AppSet{set, standalone},
	}
}

func toJSONValue(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestSampleReportValidates(t *testing.T) {
	s := loadSchema(t)
	if errs := validateJSON(s, s, toJSONValue(t, SampleReport()), "report"); len(errs) > 0 {
		t.Fatalf("sample report does not validate:\n%s", strings.Join(errs, "\n"))
	}
}

func TestSchemaIsClosedAtEveryLevel(t *testing.T) {
	s := loadSchema(t)
	mutations := map[string]func(m map[string]any){
		"root":    func(m map[string]any) { m["extra"] = 1 },
		"run":     func(m map[string]any) { m["run"].(map[string]any)["extra"] = 1 },
		"appset":  func(m map[string]any) { m["appsets"].([]any)[0].(map[string]any)["extra"] = 1 },
		"app":     func(m map[string]any) { app(m)["extra"] = 1 },
		"finding": func(m map[string]any) { finding(m)["extra"] = 1 },
		"status":  func(m map[string]any) { app(m)["status"] = "clean" },
		"impact":  func(m map[string]any) { finding(m)["impact"] = "huge" },
	}
	for name, mutate := range mutations {
		m := toJSONValue(t, SampleReport()).(map[string]any)
		mutate(m)
		if errs := validateJSON(s, s, m, "report"); len(errs) == 0 {
			t.Errorf("%s: mutated report still validates", name)
		}
	}
}

func app(m map[string]any) map[string]any {
	return m["appsets"].([]any)[0].(map[string]any)["apps"].([]any)[0].(map[string]any)
}

func finding(m map[string]any) map[string]any {
	return app(m)["diff"].(map[string]any)["Deployment/keda-demo-newrelic"].(map[string]any)["containers"].(map[string]any)["keda-demo-newrelic"].(map[string]any)["args"].(map[string]any)["0"].(map[string]any)
}

// Every property the schema declares must be emitted somewhere in the sample;
// additionalProperties:false (tested above) covers the reverse. Objects with
// optional keys (resource and container diffs) are covered by the union of
// their occurrences.
func TestEveryDeclaredPropertyIsEmitted(t *testing.T) {
	s := loadSchema(t)
	seen := map[string]map[string]bool{} // schema node key (its property list) → keys seen
	var walk func(node jsonSchema, v any)
	walk = func(node jsonSchema, v any) {
		if ref, ok := node["$ref"].(string); ok {
			target, err := resolveRef(s, ref)
			if err != nil {
				t.Fatal(err)
			}
			node = target
		}
		switch val := v.(type) {
		case map[string]any:
			if props, ok := node["properties"].(map[string]any); ok {
				names := make([]string, 0, len(props))
				for k := range props {
					names = append(names, k)
				}
				sort.Strings(names)
				id := strings.Join(names, ",")
				if seen[id] == nil {
					seen[id] = map[string]bool{}
				}
				for k, child := range val {
					sub, declared := props[k]
					if !declared {
						t.Errorf("key %q is not in the schema (%s)", k, id)
						continue
					}
					seen[id][k] = true
					walk(sub.(jsonSchema), child)
				}
			} else if ap, ok := node["additionalProperties"].(map[string]any); ok {
				for _, child := range val {
					walk(ap, child)
				}
			}
		case []any:
			if items, ok := node["items"].(map[string]any); ok {
				for _, el := range val {
					walk(items, el)
				}
			}
		}
	}
	walk(s, toJSONValue(t, SampleReport()))
	for id, keys := range seen {
		for _, p := range strings.Split(id, ",") {
			if !keys[p] {
				t.Errorf("schema property %q (object %s) is never emitted by the sample", p, id)
			}
		}
	}
	if len(seen) < 9 {
		t.Errorf("expected at least 9 object shapes walked (report, run, summary, appset, appset summary, app, resource diff, container diff, finding), got %d", len(seen))
	}
}

// The Go structs are the source of truth for field names: every json tag must
// exist in the schema's properties for that object.
func TestStructTagsMatchSchema(t *testing.T) {
	s := loadSchema(t)
	defs := s["$defs"].(map[string]any)
	cases := map[reflect.Type]map[string]any{
		reflect.TypeOf(Report{}):        s,
		reflect.TypeOf(Run{}):           s["properties"].(map[string]any)["run"].(map[string]any),
		reflect.TypeOf(RunSummary{}):    s["properties"].(map[string]any)["summary"].(map[string]any),
		reflect.TypeOf(AppSet{}):        defs["appset"].(map[string]any),
		reflect.TypeOf(AppSummary{}):    defs["appset"].(map[string]any)["properties"].(map[string]any)["summary"].(map[string]any),
		reflect.TypeOf(App{}):           defs["app"].(map[string]any),
		reflect.TypeOf(ResourceDiff{}):  defs["resource_diff"].(map[string]any),
		reflect.TypeOf(ContainerDiff{}): defs["container_diff"].(map[string]any),
		reflect.TypeOf(Finding{}):       defs["finding"].(map[string]any),
	}
	for typ, node := range cases {
		props := node["properties"].(map[string]any)
		var tags []string
		for i := 0; i < typ.NumField(); i++ {
			tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if tag == "" || tag == "-" {
				t.Errorf("%s.%s has no json tag", typ.Name(), typ.Field(i).Name)
				continue
			}
			tags = append(tags, tag)
			if _, ok := props[tag]; !ok {
				t.Errorf("%s.%s json tag %q is not in the schema", typ.Name(), typ.Field(i).Name, tag)
			}
		}
		if len(tags) != len(props) {
			t.Errorf("%s has %d tagged fields, schema object has %d properties", typ.Name(), len(tags), len(props))
		}
	}
}
