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
	c := Client{Runner: s}
	ctx := context.Background()
	_, _ = c.ListApplicationSets(ctx)
	_, _ = c.ListApplications(ctx)
	c.Diff(ctx, "x", discover.SourceRef{Position: 2}, "0.18.0")
	c.Manifests(ctx, "x", nil)
	c.Manifests(ctx, "x", &Override{Ref: discover.SourceRef{Position: 1}, Revision: "v0.18.0"})
	c.Manifests(ctx, "y", &Override{Ref: discover.SourceRef{Single: true}, Revision: "0.18.0"})
	want := [][]string{
		{"appset", "list", "--core", "-o", "json"},
		{"app", "list", "--core", "-o", "json"},
		{"app", "diff", "x", "--core", "--revisions", "0.18.0", "--source-positions", "2", "--exit-code"},
		{"app", "manifests", "x", "--core"},
		{"app", "manifests", "x", "--core", "--revisions", "v0.18.0", "--source-positions", "1"},
		{"app", "manifests", "y", "--core", "--revision", "0.18.0"},
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
