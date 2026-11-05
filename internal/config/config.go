// Package config reads the agent's environment-driven configuration and
// validates it once at startup. Unlike the lake jobs this agent writes no S3:
// it runs inside a GlueOps captain, renders through that cluster's Argo CD,
// and POSTs its report to the inventory receiver, which lands the rows.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultIngestURL is the production receiver, one endpoint for every environment.
const DefaultIngestURL = "https://inventory-agent.observability.glueopshosted.com/v1/preflight"

// DefaultStatusConfigMap is the status ConfigMap written in the pod's namespace.
const DefaultStatusConfigMap = "app-chart-preflight-status"

var captainDomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// Config is the validated configuration.
type Config struct {
	// CaptainDomain is the cluster identity; it keys every row and log line.
	CaptainDomain string
	// ArgocdNamespace is where Argo CD runs; the written kubeconfig's context points here.
	ArgocdNamespace string
	// TargetRevision every ApplicationSet is checked against unless Targets overrides it.
	TargetRevision string
	// Targets is the per-ApplicationSet override; an empty value skips that set.
	Targets map[string]string
	// Concurrency is the number of parallel per-app checks.
	Concurrency int
	// ArgocdBin is the CLI copied into the image (overridable for dev).
	ArgocdBin string
	// ArgocdTimeout bounds each CLI call; the repo-server render is the slow part.
	ArgocdTimeout time.Duration
	// ArgocdDiffText also attaches Argo's text diff (pod-template resources) to diff apps.
	ArgocdDiffText bool
	// IngestURL is the receiver; "" means no POST (set-but-empty in the environment).
	IngestURL string
	// IngestTimeout bounds the POST.
	IngestTimeout time.Duration
	// StatusConfigMap is the status ConfigMap name; "" disables the write.
	StatusConfigMap string
	// StatusNamespace overrides the pod's own namespace for the ConfigMap.
	StatusNamespace string
	// Kubeconfig is an explicit kubeconfig (dev from a workstation); the ConfigMap write is skipped.
	Kubeconfig string
	// KubeconfigDir is where the in-cluster kubeconfig and the CLI's HOME live (an emptyDir).
	KubeconfigDir string
	// ServiceAccountDir is the mounted ServiceAccount (token, ca.crt, namespace).
	ServiceAccountDir string
	DevMode           bool
	LogLevel          string
}

// Load reads and validates the environment via lookup (os.LookupEnv in production).
func Load(lookup func(string) (string, bool)) (Config, error) {
	get := func(k, def string) string {
		if v, ok := lookup(k); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}
	var errs []error
	c := Config{
		CaptainDomain:     strings.ToLower(get("CAPTAIN_DOMAIN", "")),
		ArgocdNamespace:   get("ARGOCD_NAMESPACE", "glueops-core"),
		TargetRevision:    get("TARGET_REVISION", ""),
		ArgocdBin:         get("ARGOCD_BIN", "/argocd"),
		StatusNamespace:   get("STATUS_NAMESPACE", ""),
		Kubeconfig:        get("KUBECONFIG", ""),
		KubeconfigDir:     get("KUBECONFIG_DIR", "/tmp"),
		ServiceAccountDir: get("SERVICE_ACCOUNT_DIR", "/var/run/secrets/kubernetes.io/serviceaccount"),
		LogLevel:          get("LOG_LEVEL", "info"),
	}
	if !captainDomainRE.MatchString(c.CaptainDomain) || strings.Contains(c.CaptainDomain, "placeholder") {
		errs = append(errs, fmt.Errorf("CAPTAIN_DOMAIN must be a DNS name (got %q)", c.CaptainDomain))
	}
	var err error
	if c.Targets, err = ParseTargets(get("TARGETS_JSON", "")); err != nil {
		errs = append(errs, err)
	}
	if c.Concurrency, err = intEnv(lookup, "CONCURRENCY", 4, 1); err != nil {
		errs = append(errs, err)
	}
	if c.ArgocdTimeout, err = durationEnv(lookup, "ARGOCD_TIMEOUT_MS", 120*time.Second); err != nil {
		errs = append(errs, err)
	}
	if c.IngestTimeout, err = durationEnv(lookup, "INGEST_TIMEOUT_MS", 30*time.Second); err != nil {
		errs = append(errs, err)
	}
	if c.ArgocdDiffText, err = boolEnv(lookup, "ARGOCD_DIFF_TEXT", false); err != nil {
		errs = append(errs, err)
	}
	if c.DevMode, err = boolEnv(lookup, "DEV_MODE", false); err != nil {
		errs = append(errs, err)
	}
	// INGEST_URL unset ⇒ the default receiver; set but empty ⇒ no POST (inventory-agent's contract).
	if v, ok := lookup("INGEST_URL"); !ok {
		c.IngestURL = DefaultIngestURL
	} else if u := strings.TrimSpace(v); u != "" {
		if !strings.HasPrefix(u, "https://") {
			errs = append(errs, errors.New("INGEST_URL must be https://"))
		}
		c.IngestURL = u
	}
	if v, ok := lookup("STATUS_CONFIGMAP"); !ok {
		c.StatusConfigMap = DefaultStatusConfigMap
	} else {
		c.StatusConfigMap = strings.TrimSpace(v)
	}
	return c, errors.Join(errs...)
}

// ParseTargets reads TARGETS_JSON: {"<applicationset name>": "<revision or empty to skip>"}.
func ParseTargets(raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("TARGETS_JSON must be a JSON object of {applicationset: revision}: %w", err)
	}
	for k, v := range parsed {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("TARGETS_JSON[%q] must be a string", k)
		}
		out[k] = strings.TrimSpace(s)
	}
	return out, nil
}

func intEnv(lookup func(string) (string, bool), key string, def, min int) (int, error) {
	v, ok := lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < min {
		return 0, fmt.Errorf("%s must be an integer >= %d, got %q", key, min, v)
	}
	return n, nil
}

func durationEnv(lookup func(string) (string, bool), key string, def time.Duration) (time.Duration, error) {
	ms, err := intEnv(lookup, key, int(def/time.Millisecond), 1000)
	if err != nil {
		return 0, err
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func boolEnv(lookup func(string) (string, bool), key string, def bool) (bool, error) {
	v, ok := lookup(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("%s must be a boolean, got %q", key, v)
}
