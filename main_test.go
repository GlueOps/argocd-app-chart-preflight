package main

import (
	"bytes"
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
