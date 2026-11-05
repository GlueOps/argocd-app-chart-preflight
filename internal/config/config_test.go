package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

const domain = "nonprod.venus.onglueops.rocks"

func TestDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"CAPTAIN_DOMAIN": " Nonprod.Venus.onglueops.rocks "}))
	if err != nil {
		t.Fatal(err)
	}
	if c.CaptainDomain != domain || c.ArgocdNamespace != "glueops-core" || c.TargetRevision != "" || len(c.Targets) != 0 || c.Concurrency != 2 || c.RunTimeout != 14*time.Minute ||
		c.ArgocdBin != "/argocd" || c.ArgocdTimeout != 120*time.Second || c.ArgocdDiffText || c.IngestURL != DefaultIngestURL || c.IngestTimeout != 30*time.Second ||
		c.StatusConfigMap != DefaultStatusConfigMap || c.StatusNamespace != "" || c.Kubeconfig != "" || c.KubeconfigDir != "/tmp" || c.DevMode || c.LogLevel != "info" {
		t.Errorf("defaults = %+v", c)
	}
}

func TestCaptainDomain(t *testing.T) {
	for _, bad := range []string{"", "not a domain", "placeholder.example.com"} {
		if _, err := Load(env(map[string]string{"CAPTAIN_DOMAIN": bad})); err == nil || !strings.Contains(err.Error(), "CAPTAIN_DOMAIN") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestIngestURLSemantics(t *testing.T) {
	if c, _ := Load(env(map[string]string{"CAPTAIN_DOMAIN": domain, "INGEST_URL": ""})); c.IngestURL != "" {
		t.Errorf("set-but-empty must disable the POST, got %q", c.IngestURL)
	}
	if c, _ := Load(env(map[string]string{"CAPTAIN_DOMAIN": domain, "INGEST_URL": "https://x.example/v1/preflight"})); c.IngestURL != "https://x.example/v1/preflight" {
		t.Errorf("explicit url = %q", c.IngestURL)
	}
	if _, err := Load(env(map[string]string{"CAPTAIN_DOMAIN": domain, "INGEST_URL": "http://x.example"})); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http url: err = %v", err)
	}
}

func TestOtherSettings(t *testing.T) {
	c, err := Load(env(map[string]string{"CAPTAIN_DOMAIN": domain, "STATUS_CONFIGMAP": "", "KUBECONFIG": "/home/me/.kube/config", "DEV_MODE": "true", "ARGOCD_DIFF_TEXT": "yes", "CONCURRENCY": "3", "RUN_TIMEOUT_MS": "3300000", "ARGOCD_TIMEOUT_MS": "5000", "TARGETS_JSON": `{"a": " v0.18.0 ", "b": ""}`}))
	if err != nil {
		t.Fatal(err)
	}
	if c.StatusConfigMap != "" || c.Kubeconfig != "/home/me/.kube/config" || !c.DevMode || !c.ArgocdDiffText || c.Concurrency != 3 || c.RunTimeout != 55*time.Minute || c.ArgocdTimeout != 5*time.Second {
		t.Errorf("settings = %+v", c)
	}
	if c.Targets["a"] != "v0.18.0" || c.Targets["b"] != "" || len(c.Targets) != 2 {
		t.Errorf("targets = %v", c.Targets)
	}
	for k, v := range map[string]string{"CONCURRENCY": "0", "RUN_TIMEOUT_MS": "abc", "ARGOCD_TIMEOUT_MS": "abc", "DEV_MODE": "maybe", "TARGETS_JSON": "[", "INGEST_TIMEOUT_MS": "10"} {
		if _, err := Load(env(map[string]string{"CAPTAIN_DOMAIN": domain, k: v})); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%q: err = %v", k, v, err)
		}
	}
	if _, err := ParseTargets(`{"a": 1}`); err == nil || !strings.Contains(err.Error(), "must be a string") {
		t.Errorf("non-string target: %v", err)
	}
	if _, err := ParseTargets(`[]`); err == nil {
		t.Error("array must be rejected")
	}
}

func TestIngestGzip(t *testing.T) {
	if c, _ := Load(env(map[string]string{"CAPTAIN_DOMAIN": domain})); !c.IngestGzip {
		t.Error("INGEST_GZIP must default to true")
	}
	if c, _ := Load(env(map[string]string{"CAPTAIN_DOMAIN": domain, "INGEST_GZIP": "false"})); c.IngestGzip {
		t.Error("INGEST_GZIP=false must disable gzip")
	}
}
