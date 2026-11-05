package argocd

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glueops/argocd-app-chart-preflight/internal/discover"
)

// scripted is a Runner that records calls and answers by argument prefix.
type scripted struct {
	calls   [][]string
	answers map[string]Result // key: first N args joined by " " (longest match wins)
}

func (s *scripted) Run(_ context.Context, args ...string) Result {
	s.calls = append(s.calls, args)
	best, bestLen := Result{Code: 99, Stderr: []byte("no route for " + strings.Join(args, " "))}, -1
	for k, r := range s.answers {
		if strings.HasPrefix(strings.Join(args, " ")+" ", k+" ") && len(k) > bestLen {
			best, bestLen = r, len(k)
		}
	}
	return best
}

func ok(stdout string) Result { return Result{Code: 0, Stdout: []byte(stdout)} }

func TestClientCommandLines(t *testing.T) {
	s := &scripted{answers: map[string]Result{"": ok("[]")}}
	conn := []string{"--server", "127.0.0.1:1", "--plaintext"}
	c := Client{Runner: s, Conn: conn}
	ctx := context.Background()
	_, _ = c.ListApplicationSets(ctx)
	_, _ = c.ListApplications(ctx)
	c.Diff(ctx, "x", discover.SourceRef{Position: 2}, "0.18.0")
	c.Manifests(ctx, "x", nil)
	c.Manifests(ctx, "x", &Override{Ref: discover.SourceRef{Position: 1}, Revision: "v0.18.0"})
	c.Manifests(ctx, "y", &Override{Ref: discover.SourceRef{Single: true}, Revision: "0.18.0"})
	want := [][]string{
		{"appset", "list", "-o", "json"},
		{"app", "list", "-o", "json"},
		{"app", "diff", "x", "--revisions", "0.18.0", "--source-positions", "2", "--exit-code"},
		{"app", "manifests", "x"},
		{"app", "manifests", "x", "--revisions", "v0.18.0", "--source-positions", "1"},
		{"app", "manifests", "y", "--revision", "0.18.0"},
	}
	for i := range want {
		want[i] = append(want[i], conn...)
	}
	if !reflect.DeepEqual(s.calls, want) {
		t.Errorf("calls:\n%v\nwant:\n%v", s.calls, want)
	}
}

func TestListFailuresAndEmpty(t *testing.T) {
	c := Client{Runner: &scripted{answers: map[string]Result{
		"appset list": {Code: 20, Stderr: []byte(`{"level":"fatal","msg":"services is forbidden: User x cannot list"}`)},
		"app list":    ok("null"),
		"version":     ok(`{"client":{"Version":"v3.2.12+4caf270"}}`),
	}}}
	if _, err := c.ListApplicationSets(context.Background()); err == nil || !strings.Contains(err.Error(), "appset list failed: services is forbidden") {
		t.Errorf("err = %v", err)
	}
	if apps, err := c.ListApplications(context.Background()); err != nil || apps != nil {
		t.Errorf("null list: %v %v", apps, err)
	}
	if v := c.ClientVersion(context.Background()); v != "v3.2.12+4caf270" {
		t.Errorf("version = %q", v)
	}
	bad := Client{Runner: &scripted{answers: map[string]Result{"version": ok("not json")}}}
	if v := bad.ClientVersion(context.Background()); v != "" {
		t.Errorf("version on garbage = %q", v)
	}
	timedOut := Client{Runner: &scripted{answers: map[string]Result{"app list": {Code: -1, TimedOut: true}}}}
	if _, err := timedOut.ListApplications(context.Background()); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("timeout err = %v", err)
	}
}

func TestStderrReason(t *testing.T) {
	cases := map[string]string{
		"{\"level\":\"info\",\"msg\":\"starting\"}\n{\"level\":\"fatal\",\"msg\":\"boom: it broke\"}\n": "boom: it broke",
		"Error: unknown shorthand flag: 'n' in -n\n":                                                    "Error: unknown shorthand flag: 'n' in -n",
		"": "",
	}
	for in, want := range cases {
		if got := StderrReason([]byte(in)); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if got := StderrReason([]byte(strings.Repeat("x", 2000))); len(got) != 1000 {
		t.Errorf("cap: len %d", len(got))
	}
}

func TestChartRenderErrorRE(t *testing.T) {
	if !ChartRenderErrorRE.MatchString("Error: execution error at (app/templates/_helpers.tpl:12): deployment.args: every item must be a string (got map)") {
		t.Error("chart error must match")
	}
	if ChartRenderErrorRE.MatchString(`{"msg":"rpc error: code = Unknown desc = repository not found"}`) {
		t.Error("generic CLI error must not match")
	}
}

// ExecRunner against /bin/sh: exit codes, stderr capture, missing binary, timeout.
func TestExecRunner(t *testing.T) {
	r := ExecRunner{Bin: "/bin/sh", Kubeconfig: "/tmp/kc", Home: t.TempDir(), Timeout: 5 * time.Second}
	ctx := context.Background()
	if res := r.Run(ctx, "-c", "echo out; echo err >&2; exit 3"); res.Code != 3 || string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" || res.TimedOut {
		t.Errorf("exit 3: %+v", res)
	}
	if res := r.Run(ctx, "-c", "echo $KUBECONFIG $HOME"); !strings.HasPrefix(string(res.Stdout), "/tmp/kc ") {
		t.Errorf("env not passed: %q", res.Stdout)
	}
	missing := ExecRunner{Bin: filepath.Join(t.TempDir(), "nope"), Timeout: time.Second}
	if res := missing.Run(ctx, "x"); res.Code != -1 || len(res.Stderr) == 0 {
		t.Errorf("missing binary: %+v", res)
	}
	slow := ExecRunner{Bin: "/bin/sh", Timeout: 200 * time.Millisecond}
	if res := slow.Run(ctx, "-c", "sleep 5"); !res.TimedOut || res.Code != -1 {
		t.Errorf("timeout: %+v", res)
	}
}

func TestKubeconfig(t *testing.T) {
	txt := RenderKubeconfig(KubeconfigOptions{ServiceAccountDir: "/var/run/sa", ArgocdNamespace: "glueops-core"})
	for _, want := range []string{"server: https://kubernetes.default.svc", "certificate-authority: /var/run/sa/ca.crt", "tokenFile: /var/run/sa/token", "namespace: glueops-core", "current-context: in-cluster"} {
		if !strings.Contains(txt, want) {
			t.Errorf("kubeconfig lacks %q:\n%s", want, txt)
		}
	}
	if p, src, err := EnsureKubeconfig(KubeconfigOptions{Explicit: "/home/me/.kube/config", ServiceAccountDir: "/nonexistent"}); err != nil || p != "/home/me/.kube/config" || src != "explicit" {
		t.Errorf("explicit: %q %q %v", p, src, err)
	}
	sa := t.TempDir()
	_ = os.WriteFile(filepath.Join(sa, "token"), []byte("t"), 0o600)
	dir := t.TempDir()
	p, src, err := EnsureKubeconfig(KubeconfigOptions{ServiceAccountDir: sa, ArgocdNamespace: "argocd", Dir: dir})
	if err != nil || src != "in-cluster" || p != filepath.Join(dir, "kubeconfig") {
		t.Fatalf("in-cluster: %q %q %v", p, src, err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "namespace: argocd") {
		t.Errorf("written kubeconfig: %s", b)
	}
	if _, _, err := EnsureKubeconfig(KubeconfigOptions{ServiceAccountDir: "/nonexistent", Dir: dir}); err == nil || !strings.Contains(err.Error(), "no KUBECONFIG and no ServiceAccount token") {
		t.Errorf("neither: %v", err)
	}
}

// counting is a Runner that fails with a given stderr for the first n calls, then succeeds.
type counting struct {
	failures int
	stderr   string
	code     int
	calls    int
}

func (c *counting) Run(_ context.Context, _ ...string) Result {
	c.calls++
	if c.calls <= c.failures {
		return Result{Code: c.code, Stderr: []byte(c.stderr)}
	}
	return Result{Code: 0, Stdout: []byte("[]")}
}

func TestTransientFailuresAreRetried(t *testing.T) {
	bind := `{"level":"fatal","msg":"failed to listen: listen tcp 127.0.0.1:43731: bind: address already in use"}`
	r := &counting{failures: 2, stderr: bind, code: 20}
	if _, err := (Client{Runner: r}).ListApplications(context.Background()); err != nil || r.calls != 3 {
		t.Errorf("two collisions then success: err=%v calls=%d", err, r.calls)
	}
	r = &counting{failures: 5, stderr: bind, code: 20}
	if _, err := (Client{Runner: r}).ListApplications(context.Background()); err == nil || r.calls != 3 {
		t.Errorf("gives up after the retries: err=%v calls=%d", err, r.calls)
	}
	chart := &counting{failures: 5, stderr: "deployment.args: every item must be a string (got map)", code: 20}
	(Client{Runner: chart}).Manifests(context.Background(), "x", nil)
	if chart.calls != 1 {
		t.Errorf("a chart error is never retried, got %d calls", chart.calls)
	}
	diff := &counting{failures: 5, stderr: "port forward whatever", code: 1}
	(Client{Runner: diff}).Diff(context.Background(), "x", discover.SourceRef{Position: 1}, "v")
	if diff.calls != 1 {
		t.Errorf("diff exit 1 means a diff, never retried, got %d calls", diff.calls)
	}
}

func TestChartErrorReasonTrimsToTheChartSentence(t *testing.T) {
	raw := `{"level":"fatal","msg":"rpc error: code = Unknown desc = failed to execute helm template command: failed to get command args to log: ` + "`helm template . --name-template x`" + ` failed exit status 1: Error: execution error at (app/templates/deployment.yaml:49:6): deployment.args: every item must be a string (got map) - quote the item in your values file; null, map and list items are not supported\n\nUse --debug flag to render out invalid YAML"}`
	if got := ChartErrorReason([]byte(raw)); got != "deployment.args: every item must be a string (got map) - quote the item in your values file; null, map and list items are not supported" {
		t.Errorf("got %q", got)
	}
	if got := ChartErrorReason([]byte(`{"msg":"dial tcp: connection refused"}`)); got != "dial tcp: connection refused" {
		t.Errorf("fallback: %q", got)
	}
}
