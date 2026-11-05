// Command argocd-app-chart-preflight is GlueOps' per-captain pre-bump gate for
// the `app` Helm chart. One run renders every tenant Application at a target
// chart revision through the cluster's own Argo CD (one local API server per
// run, Argo CD's own credentials), reports per ApplicationSet whether the bump is clean and, if
// not, the exact old/new command/args items per container, writes the report
// to a ConfigMap, POSTs it to the receiver, logs one summary line and exits 0
// no matter what.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/glueops/argocd-app-chart-preflight/internal/agent"
	"github.com/glueops/argocd-app-chart-preflight/internal/argocd"
	"github.com/glueops/argocd-app-chart-preflight/internal/config"
	"github.com/glueops/argocd-app-chart-preflight/internal/kube"
	"github.com/glueops/argocd-app-chart-preflight/internal/logging"
	"github.com/glueops/argocd-app-chart-preflight/internal/report"
	"github.com/glueops/argocd-app-chart-preflight/internal/send"
)

// version and commit are set at build time: -ldflags="-X main.version=v1.2.3 -X main.commit=abc".
var (
	version = "dev"
	commit  = "local"
)

func main() {
	os.Exit(run(os.LookupEnv, os.Stdout))
}

// run is the whole process: it always returns 0 and always ends with exactly
// one "run summary" log line, whichever path the run takes.
func run(lookup func(string) (string, bool), out io.Writer) (code int) {
	level, _ := lookup("LOG_LEVEL")
	log := logging.New(level, out)
	summarized := false
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			log.Error("unrecovered panic", "reason", logging.ReasonInternalError, "panic", fmt.Sprint(r))
			if !summarized {
				agent.LogSummary(log, agent.Summary{RunID: agent.NewRunID(time.Now()), Status: agent.StatusInternalError, Reason: logging.ReasonInternalError, Error: "panic: " + fmt.Sprint(r), Duration: time.Since(start)})
			}
		}
		code = 0
	}()
	log.Info("build", "version", version, "commit", commit)

	cfg, err := config.Load(lookup)
	if err != nil {
		log.Error("invalid configuration, nothing checked", "reason", logging.ReasonInvalidConfig, "error", err.Error())
		agent.LogSummary(log, agent.Summary{RunID: agent.NewRunID(time.Now()), Status: agent.StatusInvalidConfig, Reason: logging.ReasonInvalidConfig, Error: err.Error(), Duration: time.Since(start)})
		summarized = true
		return 0
	}
	log.Info("config loaded", "captain_domain", cfg.CaptainDomain, "target", cfg.TargetRevision, "targets", cfg.Targets, "concurrency", cfg.Concurrency, "ingest", cfg.IngestURL != "", "configmap", cfg.StatusConfigMap, "dev_mode", cfg.DevMode)

	// A deadline or a SIGTERM (node drain, eviction) stops new work; agent.Run
	// still writes the report for what finished, on a context of its own.
	signalled, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalled, cfg.RunTimeout)
	defer cancel()

	kcPath, kcSource, err := argocd.EnsureKubeconfig(argocd.KubeconfigOptions{Explicit: cfg.Kubeconfig, ServiceAccountDir: cfg.ServiceAccountDir, ArgocdNamespace: cfg.ArgocdNamespace, Dir: cfg.KubeconfigDir})
	if err != nil {
		log.Error("cannot reach the cluster", "reason", logging.ReasonArgocdUnavailable, "error", err.Error())
		agent.LogSummary(log, agent.Summary{RunID: agent.NewRunID(time.Now()), Status: agent.StatusInternalError, Reason: logging.ReasonArgocdUnavailable, Error: err.Error(), Duration: time.Since(start)})
		summarized = true
		return 0
	}
	home, err := os.MkdirTemp(cfg.KubeconfigDir, "argocd-home-")
	if err != nil {
		home = filepath.Join(cfg.KubeconfigDir, "argocd-home")
		_ = os.MkdirAll(home, 0o700)
	}
	server := &argocd.Server{Bin: cfg.ArgocdBin, Kubeconfig: kcPath, Home: home, Log: log}
	if err := server.Start(ctx); err != nil {
		log.Error("local argocd server unavailable", "reason", logging.ReasonArgocdUnavailable, "error", err.Error())
		agent.LogSummary(log, agent.Summary{RunID: agent.NewRunID(time.Now()), Status: agent.StatusInternalError, Reason: logging.ReasonArgocdUnavailable, Error: err.Error(), Duration: time.Since(start)})
		summarized = true
		return 0
	}
	defer server.Stop()
	deps := agent.Deps{
		Argocd: argocd.Client{
			Runner:   argocd.ExecRunner{Bin: cfg.ArgocdBin, Kubeconfig: kcPath, Home: home, Timeout: cfg.ArgocdTimeout},
			Conn:     server.Conn(),
			Recovery: server,
			Retries:  3,
			Backoff:  3 * time.Second,
		},
		Version: version,
		Commit:  commit,
	}
	log.Info("argocd ready", "kubeconfig", kcSource, "bin", cfg.ArgocdBin)

	// The status ConfigMap is written only from inside a cluster (never with an
	// explicit dev kubeconfig, whose user would have no "own namespace").
	if cfg.StatusConfigMap != "" && cfg.Kubeconfig == "" {
		client, err := kube.NewClient("argocd-app-chart-preflight/"+version, 30*time.Second)
		if err != nil {
			log.Warn("kubernetes client unavailable; the status configmap will not be written", "reason", logging.ReasonKubeClientUnavailable, "error", err.Error())
		} else {
			deps.Kube = client
		}
	}
	if cfg.IngestURL != "" {
		enc := send.EncodingGzip
		if !cfg.IngestGzip {
			enc = send.EncodingIdentity
		}
		deps.Sender = send.New(send.Options{URL: cfg.IngestURL, Timeout: cfg.IngestTimeout, Retries: 2, UserAgent: "argocd-app-chart-preflight/" + version, Encoding: enc})
	}

	code, rep := agent.Run(ctx, cfg, deps, log)
	summarized = true
	if cfg.DevMode && rep != nil {
		_ = report.WriteYAML(out, rep)
	}
	return code
}
