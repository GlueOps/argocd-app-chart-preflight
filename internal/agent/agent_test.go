package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/glueops/argocd-app-chart-preflight/internal/argocd"
	"github.com/glueops/argocd-app-chart-preflight/internal/config"
	"github.com/glueops/argocd-app-chart-preflight/internal/discover"
	"github.com/glueops/argocd-app-chart-preflight/internal/logging"
	"github.com/glueops/argocd-app-chart-preflight/internal/manifests"
	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
	"github.com/glueops/argocd-app-chart-preflight/internal/send"
)

// ---- a scripted argocd ---------------------------------------------------------------------

type route struct {
	match func(args []string) bool
	reply func(args []string) argocd.Result
}

type scripted struct {
	routes []route
	mu     sync.Mutex
	calls  [][]string
}

func (s *scripted) Run(_ context.Context, args ...string) argocd.Result {
	s.mu.Lock()
	s.calls = append(s.calls, args)
	s.mu.Unlock()
	for _, r := range s.routes {
		if r.match(args) {
			return r.reply(args)
		}
	}
	return argocd.Result{Code: 99, Stderr: []byte("fake argocd: no route for " + strings.Join(args, " "))}
}

func (s *scripted) called(sub, app string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if len(c) > 2 && c[0] == "app" && c[1] == sub && c[2] == app {
			return true
		}
	}
	return false
}

func is(prefix ...string) func([]string) bool {
	return func(args []string) bool {
		if len(args) < len(prefix) {
			return false
		}
		for i, p := range prefix {
			if args[i] != p {
				return false
			}
		}
		return true
	}
}

func hasApp(sub, app string) func([]string) bool {
	return func(args []string) bool { return len(args) > 2 && args[0] == "app" && args[1] == sub && args[2] == app }
}

func withRevision(args []string) bool {
	for _, a := range args {
		if a == "--revisions" || a == "--revision" {
			return true
		}
	}
	return false
}

func ok(stdout string) func([]string) argocd.Result {
	return func([]string) argocd.Result { return argocd.Result{Code: 0, Stdout: []byte(stdout)} }
}

func fail(code int, stderr, stdout string) func([]string) argocd.Result {
	return func([]string) argocd.Result {
		return argocd.Result{Code: code, Stderr: []byte(stderr), Stdout: []byte(stdout)}
	}
}

func timeout() func([]string) argocd.Result {
	return func([]string) argocd.Result { return argocd.Result{Code: -1, TimedOut: true} }
}

func jsonList(items ...map[string]any) string {
	b, _ := json.Marshal(items)
	return string(b)
}

// ---- fixtures -------------------------------------------------------------------------------

var venusValuesSrc = map[string]any{"repoURL": "https://github.com/development-tenant-venus/deployment-configurations", "targetRevision": "main", "ref": "values"}

func gitChartSrc(rev string) map[string]any {
	return map[string]any{"repoURL": "https://github.com/GlueOps/project-template-helm-chart-app.git", "path": ".", "targetRevision": rev,
		"helm": map[string]any{"valueFiles": []any{"$values/common/common-values.yaml", "$values/env-overlays/nonprod/env-values.yaml", "$values/apps/keda-demo-newrelic/base/base-values.yaml", "$values/apps/keda-demo-newrelic/envs/nonprod/values.yaml"}}}
}

func helmChartSrc(rev string) map[string]any {
	return map[string]any{"repoURL": "https://helm.gpkg.io/project-template", "chart": "app", "targetRevision": rev, "helm": map[string]any{"valueFiles": []any{"$values/apps/x/envs/prod/values.yaml"}}}
}

func application(name, appSet string, sources []any, single map[string]any, sync string) map[string]any {
	meta := map[string]any{"name": name, "namespace": "glueops-core"}
	if appSet != "" {
		meta["labels"] = map[string]any{discover.AppSetLabel: appSet}
	}
	spec := map[string]any{"project": "nonprod", "destination": map[string]any{"namespace": "nonprod"}}
	if sources != nil {
		spec["sources"] = sources
	}
	if single != nil {
		spec["source"] = single
	}
	if sync == "" {
		sync = "Synced"
	}
	return map[string]any{"metadata": meta, "spec": spec, "status": map[string]any{"sync": map[string]any{"status": sync}}}
}

func applicationSet(name string, chartSrc map[string]any) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"sources": []any{chartSrc, venusValuesSrc}}}}}
}

func deployment(name string, args []any) string {
	doc := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": name, "image": "img", "args": args}}}}}}
	b, _ := json.Marshal(doc)
	return "---\n" + string(b) + "\n"
}

func testdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const set = "nonprod-application-set"

func cfg(target string) config.Config {
	return config.Config{CaptainDomain: "nonprod.venus.onglueops.rocks", TargetRevision: target, Targets: map[string]string{}, Concurrency: 2, ArgocdTimeout: time.Second}
}

func deps(s *scripted) Deps {
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	return Deps{Argocd: argocd.Client{Runner: s}, Version: "test", Commit: "abc", Now: func() time.Time { t0 = t0.Add(time.Second); return t0 }}
}

func byName(sets []schema.AppSet, name string) *schema.AppSet {
	for i := range sets {
		if sets[i].Name == name {
			return &sets[i]
		}
	}
	return nil
}

func appByName(s *schema.AppSet, name string) *schema.App {
	for i := range s.Apps {
		if s.Apps[i].Name == name {
			return &s.Apps[i]
		}
	}
	return nil
}

// ---- tests ----------------------------------------------------------------------------------

func TestPreflightVerdicts(t *testing.T) {
	apps := []map[string]any{
		application("clean", set, []any{gitChartSrc("v0.17.1"), venusValuesSrc}, nil, ""),
		application("quoted", set, []any{gitChartSrc("v0.17.1"), venusValuesSrc}, nil, ""),
		application("mapitem", set, []any{gitChartSrc("v0.17.1"), venusValuesSrc}, nil, "OutOfSync"),
		application("flaky", set, []any{gitChartSrc("v0.17.1"), venusValuesSrc}, nil, ""),
		application("drifted", set, []any{gitChartSrc("v0.16.1"), venusValuesSrc}, nil, ""),
		application("noop", set, []any{gitChartSrc("v0.17.1"), venusValuesSrc}, nil, ""),
		application("platform", "", nil, map[string]any{"repoURL": "https://helm.gpkg.io/platform", "chart": "x"}, ""),
		// a platform Application that uses the app chart from helm.gpkg.io: standalone, and a git-ref target is the wrong kind for it
		application("glueops-alerts", "", nil, helmChartSrc("0.13.0"), ""),
	}
	s := &scripted{routes: []route{
		{is("version"), ok(`{"client":{"Version":"v3.2.12"}}`)},
		{is("appset", "list"), ok(jsonList(applicationSet(set, gitChartSrc("v0.17.1"))))},
		{is("app", "list"), ok(jsonList(apps...))},
		{hasApp("manifests", "clean"), ok(deployment("clean", []any{"x"}))},
		{hasApp("manifests", "quoted"), func(a []string) argocd.Result {
			if withRevision(a) {
				return argocd.Result{Code: 0, Stdout: []byte(deployment("quoted", []any{`"echo a: b"`, "x"}))}
			}
			return argocd.Result{Code: 0, Stdout: []byte(deployment("quoted", []any{"echo a: b", "x"}))}
		}},
		{hasApp("manifests", "mapitem"), func(a []string) argocd.Result {
			if withRevision(a) {
				return argocd.Result{Code: 20, Stderr: []byte(`{"level":"fatal","msg":"rpc error: failed to execute helm template: deployment.args: every item must be a string (got map)"}`)}
			}
			return argocd.Result{Code: 0, Stdout: []byte(deployment("mapitem", []any{"map[a:b]"}))}
		}},
		{hasApp("manifests", "flaky"), fail(20, `{"level":"fatal","msg":"dial tcp: connection refused"}`, "")},
		{hasApp("manifests", "noop"), func(a []string) argocd.Result {
			if withRevision(a) {
				return argocd.Result{Code: 0, Stdout: []byte(deployment("noop", []any{""}))}
			}
			return argocd.Result{Code: 0, Stdout: []byte(deployment("noop", []any{nil}))}
		}},
	}}
	rep, err := Preflight(context.Background(), cfg("v0.18.0"), deps(s), logging.New("info", io.Discard), "20261104100000_0123abcd", time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Run.Status != schema.GroupError || rep.Run.ID != "20261104100000_0123abcd" || *rep.Run.ArgocdCLIVersion != "v3.2.12" || rep.Run.StartedAt != "2026-11-04T10:00:00.000Z" {
		t.Errorf("run = %+v", rep.Run)
	}
	if rep.Summary != (schema.RunSummary{AppSets: 2, Error: 1, Skipped: 1, AppsIgnored: 1}) {
		t.Errorf("summary = %+v", rep.Summary)
	}
	g := byName(rep.AppSets, set)
	if g == nil || *g.SourceType != schema.SourceGit || *g.CurrentRevision != "v0.17.1" || *g.TargetRevision != "v0.18.0" || g.Status != schema.GroupError {
		t.Fatalf("set = %+v", g)
	}
	if g.Summary != (schema.AppSummary{Apps: 6, OK: 1, OKNoopDiff: 1, Diff: 1, Error: 1, Unknown: 2}) {
		t.Errorf("set summary = %+v", g.Summary)
	}
	if a := appByName(g, "clean"); a.Status != schema.AppOK || len(a.Diff) != 0 {
		t.Errorf("clean = %+v", a)
	}
	q := appByName(g, "quoted")
	if q.Status != schema.AppDiff || q.Diff["Deployment/quoted"].Containers["quoted"].Args["0"].Category != schema.CategoryDoubleQuoted || q.ArgocdDiff != nil {
		t.Errorf("quoted = %+v", q)
	}
	if want := []string{"common/common-values.yaml", "env-overlays/nonprod/env-values.yaml", "apps/keda-demo-newrelic/base/base-values.yaml", "apps/keda-demo-newrelic/envs/nonprod/values.yaml"}; strings.Join(q.ValuesFiles, ",") != strings.Join(want, ",") {
		t.Errorf("values_files = %v", q.ValuesFiles)
	}
	if m := appByName(g, "mapitem"); m.Status != schema.AppError || !strings.Contains(*m.Reason, "every item must be a string (got map)") || *m.SyncStatus != "OutOfSync" {
		t.Errorf("mapitem = %+v", m)
	}
	if f := appByName(g, "flaky"); f.Status != schema.AppUnknown || *f.Reason != "argocd app manifests (current) failed: dial tcp: connection refused" {
		t.Errorf("flaky = %+v", f)
	}
	if d := appByName(g, "drifted"); d.Status != schema.AppUnknown || !strings.Contains(*d.Reason, "revision drift") {
		t.Errorf("drifted = %+v", d)
	}
	if n := appByName(g, "noop"); n.Status != schema.AppOKNoopDiff {
		t.Errorf("noop = %+v", n)
	}
	sa := byName(rep.AppSets, discover.Standalone)
	if sa == nil || sa.Status != schema.GroupSkipped || sa.Apps[0].Name != "glueops-alerts" || sa.Apps[0].Namespace != "nonprod" || !strings.Contains(*sa.Apps[0].Reason, "needs X.Y.Z") {
		t.Errorf("standalone = %+v", sa)
	}
	if s.called("diff", "clean") || s.called("diff", "quoted") {
		t.Error("app diff runs only for the optional text diff")
	}
	if s.called("manifests", "drifted") || s.called("manifests", "platform") || s.called("manifests", "glueops-alerts") {
		t.Error("drifted, platform and wrong-kind-target apps never hit the CLI")
	}
}

func TestPreflightGroupSkipUnknownAndStandalone(t *testing.T) {
	s := &scripted{routes: []route{
		{is("version"), ok("{}")},
		{is("appset", "list"), ok(jsonList(applicationSet("git-set", gitChartSrc("v0.17.1")), applicationSet("helm-set", helmChartSrc("0.17.1"))))},
		{is("app", "list"), ok(jsonList(application("g1", "git-set", []any{gitChartSrc("v0.17.1"), venusValuesSrc}, nil, ""), application("h1", "helm-set", []any{helmChartSrc("0.17.1"), venusValuesSrc}, nil, ""), application("solo", "", []any{helmChartSrc("0.17.1"), venusValuesSrc}, nil, "")))},
		{hasApp("manifests", "solo"), ok(deployment("solo", []any{"x"}))},
		{hasApp("manifests", "h1"), ok(deployment("h1", []any{"x"}))},
	}}
	c := cfg("0.18.0")
	c.Targets = map[string]string{"git-set": ""}
	rep, err := Preflight(context.Background(), c, deps(s), logging.New("info", io.Discard), "20261104100000_0123abcd", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if g := byName(rep.AppSets, "git-set"); g.Status != schema.GroupSkipped || *g.Reason != "skipped by targets override" || g.Apps[0].Status != schema.AppSkipped {
		t.Errorf("git-set = %+v", g)
	}
	if byName(rep.AppSets, "helm-set").Status != schema.GroupOK || byName(rep.AppSets, discover.Standalone).Status != schema.GroupOK || rep.Run.Status != schema.GroupOK {
		t.Errorf("statuses: %+v", rep.AppSets)
	}
	if rep.Summary != (schema.RunSummary{AppSets: 3, OK: 2, Skipped: 1}) {
		t.Errorf("summary = %+v", rep.Summary)
	}
	rep2, _ := Preflight(context.Background(), cfg("0.18.0"), deps(s), logging.New("info", io.Discard), "20261104100000_0123abcd", time.Now())
	if g := byName(rep2.AppSets, "git-set"); g.Status != schema.GroupUnknown || !strings.Contains(*g.Apps[0].Reason, "needs a git ref such as v0.18.0") || rep2.Run.Status != schema.GroupUnknown {
		t.Errorf("wrong-kind target: %+v run=%s", g, rep2.Run.Status)
	}
	if s.called("manifests", "g1") {
		t.Error("an unknown set never renders")
	}
}

func TestPreflightTimeoutsGarbledAndDiffText(t *testing.T) {
	s := &scripted{routes: []route{
		{is("version"), ok("{}")},
		{is("appset", "list"), ok(jsonList(applicationSet(set, gitChartSrc("v0.17.1"))))},
		{is("app", "list"), ok(jsonList(application("slow", set, []any{gitChartSrc("v0.17.1"), venusValuesSrc}, nil, ""), application("garbled", set, []any{gitChartSrc("v0.17.1"), venusValuesSrc}, nil, ""), application("texty", set, []any{gitChartSrc("v0.17.1"), venusValuesSrc}, nil, "")))},
		{hasApp("manifests", "slow"), timeout()},
		{hasApp("manifests", "garbled"), func(a []string) argocd.Result {
			if withRevision(a) {
				return argocd.Result{Code: 0, Stdout: []byte("a: b: c")}
			}
			return argocd.Result{Code: 0, Stdout: []byte(deployment("garbled", []any{"x"}))}
		}},
		{hasApp("diff", "texty"), func([]string) argocd.Result {
			return argocd.Result{Code: 1, Stdout: []byte(testdata(t, "keda-demo-newrelic-nonprod.argocd-diff.txt"))}
		}},
		{hasApp("manifests", "texty"), func(a []string) argocd.Result {
			if withRevision(a) {
				return argocd.Result{Code: 0, Stdout: []byte(deployment("keda-demo-newrelic", []any{"x "}))}
			}
			return argocd.Result{Code: 0, Stdout: []byte(deployment("keda-demo-newrelic", []any{"x"}))}
		}},
	}}
	c := cfg("v0.18.0")
	c.ArgocdDiffText = true
	rep, err := Preflight(context.Background(), c, deps(s), logging.New("info", io.Discard), "20261104100000_0123abcd", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	g := byName(rep.AppSets, set)
	if a := appByName(g, "slow"); a.Status != schema.AppUnknown || *a.Reason != "argocd app manifests (current) timed out" {
		t.Errorf("slow = %+v", a)
	}
	if a := appByName(g, "garbled"); a.Status != schema.AppUnknown || !strings.Contains(*a.Reason, "could not parse rendered manifests") {
		t.Errorf("garbled = %+v", a)
	}
	a := appByName(g, "texty")
	if a.Status != schema.AppDiff || a.ArgocdDiff == nil || !strings.HasPrefix(*a.ArgocdDiff, "===== apps/Deployment /keda-demo-newrelic ======") || strings.Contains(*a.ArgocdDiff, "/Service ") {
		t.Errorf("texty = %+v", a)
	}
}

func TestPodTemplateDiffText(t *testing.T) {
	ts, _ := manifests.ParseAndReduce([]byte(testdata(t, "keda-demo-newrelic-nonprod.v0.17.1.yaml")))
	txt := PodTemplateDiffText(testdata(t, "keda-demo-newrelic-nonprod.argocd-diff.txt"), ts, 64*1024)
	if txt == nil || !strings.Contains(*txt, "Deployment /keda-demo-newrelic") || strings.Contains(*txt, "ConfigMap") || strings.Contains(*txt, "ScaledObject") {
		t.Errorf("text = %v", txt)
	}
	if PodTemplateDiffText("===== /Service nonprod/x ======\n1c1\n< a\n> b", ts, 1024) != nil {
		t.Error("non-pod-template resource must be dropped")
	}
	big := PodTemplateDiffText("===== apps/Deployment nonprod/keda-demo-newrelic ======\n"+strings.Repeat("x", 100), ts, 50)
	if big == nil || !strings.HasSuffix(*big, "…truncated") {
		t.Error("cap must truncate")
	}
}

func TestRunWritesConfigMapPostsYAMLAndLogsOneSummary(t *testing.T) {
	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != send.ContentTypeYAML || r.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("headers = %v", r.Header)
		}
		zr, _ := gzip.NewReader(r.Body)
		b, _ := io.ReadAll(zr)
		received = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	s := &scripted{routes: []route{
		{is("version"), ok("{}")},
		{is("appset", "list"), ok(jsonList(applicationSet(set, gitChartSrc("v0.17.1"))))},
		{is("app", "list"), ok(jsonList(application("clean", set, []any{gitChartSrc("v0.17.1"), venusValuesSrc}, nil, "")))},
		{hasApp("manifests", "clean"), ok(deployment("clean", []any{"x"}))},
	}}
	c := cfg("v0.18.0")
	c.IngestURL = srv.URL
	c.IngestGzip = true
	c.StatusConfigMap = "app-chart-preflight-status"
	c.StatusNamespace = "preflight"
	d := deps(s)
	d.Kube = fake.NewClientset()
	d.Sender = send.New(send.Options{URL: srv.URL, Timeout: time.Second})
	var out bytes.Buffer
	code, rep := Run(context.Background(), c, d, logging.New("info", &out))
	if code != 0 || rep == nil || rep.Run.Status != schema.GroupOK {
		t.Fatalf("code=%d rep=%+v", code, rep)
	}
	logs := out.String()
	if n := strings.Count(logs, `"msg":"run summary"`); n != 1 {
		t.Errorf("summary lines = %d:\n%s", n, logs)
	}
	for _, want := range []string{`"status":"ok"`, `"configmap":"created"`, `"ingest":"accepted (202)"`, `"component":"argocd-app-chart-preflight"`, `"msg":"appset"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
	if !strings.HasPrefix(received, "schema_version: 1\n") || !strings.Contains(received, `captain_domain: "nonprod.venus.onglueops.rocks"`) {
		t.Errorf("receiver got:\n%s", received)
	}
}

// A run stopped part-way (deadline or SIGTERM) must still report: finished apps
// keep their verdict, the rest are unknown with the reason, and the ConfigMap
// and POST go out even though the run's context is over.
func TestStoppedRunStillReports(t *testing.T) {
	for name, tc := range map[string]struct {
		stop   func() (context.Context, context.CancelFunc)
		reason string
	}{
		"deadline": {func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Hour)
		}, "the run reached its time limit"},
		"sigterm": {func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, "stopped before this app finished"},
	} {
		t.Run(name, func(t *testing.T) {
			var received atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Store(true)
				w.WriteHeader(http.StatusAccepted)
			}))
			defer srv.Close()
			ctx, cancel := tc.stop()
			defer cancel()
			if name == "deadline" {
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(200*time.Millisecond))
				defer cancel()
			}
			s := &scripted{routes: []route{
				{is("version"), ok("{}")},
				{is("appset", "list"), ok(jsonList(applicationSet(set, gitChartSrc("v0.17.1"))))},
				{is("app", "list"), ok(jsonList(application("done", set, []any{gitChartSrc("v0.17.1"), venusValuesSrc}, nil, ""), application("late", set, []any{gitChartSrc("v0.17.1"), venusValuesSrc}, nil, "")))},
				{hasApp("manifests", "done"), ok(deployment("done", []any{"x"}))},
				{hasApp("manifests", "late"), func([]string) argocd.Result {
					if name == "sigterm" {
						cancel()
					} else {
						<-ctx.Done()
					}
					return argocd.Result{Code: -1}
				}},
			}}
			c := cfg("v0.18.0")
			c.Concurrency = 1
			c.IngestURL = srv.URL
			c.StatusConfigMap = "app-chart-preflight-status"
			c.StatusNamespace = "preflight"
			d := deps(s)
			kube := fake.NewClientset()
			d.Kube = kube
			d.Sender = send.New(send.Options{URL: srv.URL, Timeout: time.Second})
			var out bytes.Buffer
			_, rep := Run(ctx, c, d, logging.New("info", &out))
			if rep == nil {
				t.Fatalf("no report:\n%s", out.String())
			}
			g := byName(rep.AppSets, set)
			if a := appByName(g, "done"); a.Status != schema.AppOK {
				t.Errorf("finished app lost its verdict: %+v", a)
			}
			if a := appByName(g, "late"); a.Status != schema.AppUnknown || !strings.Contains(*a.Reason, tc.reason) {
				t.Errorf("late = %s %q", a.Status, deref(a.Reason))
			}
			if rep.Run.Status != schema.GroupUnknown {
				t.Errorf("a partial run must not pass the gate: %s", rep.Run.Status)
			}
			if !received.Load() {
				t.Errorf("POST did not go out:\n%s", out.String())
			}
			if _, err := kube.CoreV1().ConfigMaps("preflight").Get(context.Background(), "app-chart-preflight-status", metav1.GetOptions{}); err != nil {
				t.Errorf("configmap not written: %v", err)
			}
		})
	}
}

func TestRunArgocdUnavailableStillExitsZero(t *testing.T) {
	s := &scripted{routes: []route{{is("appset", "list"), fail(20, `{"level":"fatal","msg":"services is forbidden"}`, "")}}}
	var out bytes.Buffer
	code, rep := Run(context.Background(), cfg("v0.18.0"), deps(s), logging.New("info", &out))
	if code != 0 || rep != nil {
		t.Fatalf("code=%d rep=%v", code, rep)
	}
	if !strings.Contains(out.String(), `"status":"internal_error"`) || !strings.Contains(out.String(), `"reason":"argocd_unavailable"`) || strings.Count(out.String(), `"msg":"run summary"`) != 1 {
		t.Errorf("logs:\n%s", out.String())
	}
}

func TestNewRunID(t *testing.T) {
	id := NewRunID(time.Date(2026, 11, 4, 10, 0, 0, 12345, time.UTC))
	if !strings.HasPrefix(id, "20261104100000_") || len(id) != 23 {
		t.Errorf("id = %q", id)
	}
}
