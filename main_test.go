package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// TestRunAlwaysExitsZeroWithOneSummary drives the real entrypoint with invalid
// config, with no cluster at all, and with a kubeconfig but a missing CLI;
// every path must return 0 and log exactly one "run summary" line with the
// expected status.
func TestRunAlwaysExitsZeroWithOneSummary(t *testing.T) {
	missingBin := filepath.Join(t.TempDir(), "argocd-does-not-exist")
	cases := map[string]struct {
		env    map[string]string
		status string
	}{
		"invalid config":  {map[string]string{}, "invalid_config"},
		"placeholder":     {map[string]string{"CAPTAIN_DOMAIN": "placeholder.onglueops.com"}, "invalid_config"},
		"bad env numbers": {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "CONCURRENCY": "-1"}, "invalid_config"},
		"non-https url":   {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "INGEST_URL": "http://x"}, "invalid_config"},
		"no cluster":      {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "INGEST_URL": "", "SERVICE_ACCOUNT_DIR": t.TempDir(), "KUBECONFIG_DIR": t.TempDir()}, "internal_error"},
		"missing cli":     {map[string]string{"CAPTAIN_DOMAIN": "nonprod.foo.onglueops.com", "INGEST_URL": "", "STATUS_CONFIGMAP": "", "KUBECONFIG": "/tmp/nonexistent-kubeconfig", "ARGOCD_BIN": missingBin, "KUBECONFIG_DIR": t.TempDir()}, "internal_error"},
	}
	for name, c := range cases {
		var out bytes.Buffer
		if code := run(envFrom(c.env), &out); code != 0 {
			t.Errorf("%s: exit code %d, want 0", name, code)
		}
		logs := out.String()
		if n := strings.Count(logs, `"msg":"run summary"`); n != 1 {
			t.Errorf("%s: expected exactly one summary line, got %d:\n%s", name, n, logs)
		}
		if !strings.Contains(logs, `"status":"`+c.status+`"`) {
			t.Errorf("%s: expected status %q in summary:\n%s", name, c.status, logs)
		}
		if !strings.Contains(logs, `"run_id":"`) || !strings.Contains(logs, `"component":"argocd-app-chart-preflight"`) {
			t.Errorf("%s: summary must carry run_id and component:\n%s", name, logs)
		}
	}
}

func TestOperatorModeEarlyExits(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	var out, errOut bytes.Buffer
	if code := runOperator([]string{"--help"}, &out, &errOut, none, false, false); code != 0 || !strings.Contains(errOut.String(), "Usage: app-chart-preflight") {
		t.Errorf("--help: exit %d\n%s", code, errOut.String())
	}
	out.Reset()
	if code := runOperator([]string{"--version"}, &out, &errOut, none, false, false); code != 0 || !strings.HasPrefix(out.String(), "app-chart-preflight dev") {
		t.Errorf("--version: exit %d, %q", code, out.String())
	}
	errOut.Reset()
	if code := runOperator([]string{"--appset", "prod"}, &out, &errOut, none, false, false); code != 2 || !strings.Contains(errOut.String(), "no target for prod") {
		t.Errorf("missing target: exit %d\n%s", code, errOut.String())
	}
	// A kube context that does not exist stops the run before anything starts.
	kc := filepath.Join(t.TempDir(), "config")
	_ = os.WriteFile(kc, []byte("apiVersion: v1\nkind: Config\ncurrent-context: \"\"\n"), 0o600)
	t.Setenv("KUBECONFIG", kc)
	errOut.Reset()
	if code := runOperator([]string{"--target", "0.18.0", "--context", "nope"}, &out, &errOut, none, false, false); code != 2 || !strings.Contains(errOut.String(), `kube context "nope" not found`) {
		t.Errorf("unknown context: exit %d\n%s", code, errOut.String())
	}
}

func TestProgress(t *testing.T) {
	var b bytes.Buffer
	p := progress(&b, false)
	for i := 1; i <= 20; i++ {
		p(i, 20)
	}
	// The first app, then every tenth of the way: 11 lines for 20 apps.
	if n := strings.Count(b.String(), "\n"); n != 11 {
		t.Errorf("non-terminal progress printed %d lines, want 11:\n%s", n, b.String())
	}
	b.Reset()
	p = progress(&b, true)
	p(1, 2)
	p(2, 2)
	if !strings.HasPrefix(b.String(), "\rchecked 1/2 apps") || !strings.HasSuffix(b.String(), "\r\x1b[K") {
		t.Errorf("terminal progress = %q", b.String())
	}
}
