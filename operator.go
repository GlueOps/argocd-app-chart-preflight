package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/glueops/argocd-app-chart-preflight/internal/agent"
	"github.com/glueops/argocd-app-chart-preflight/internal/argocd"
	"github.com/glueops/argocd-app-chart-preflight/internal/cli"
	"github.com/glueops/argocd-app-chart-preflight/internal/logging"
	"github.com/glueops/argocd-app-chart-preflight/internal/manifests"
	"github.com/glueops/argocd-app-chart-preflight/internal/report"
	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
	"github.com/glueops/argocd-app-chart-preflight/internal/textreport"
)

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// runOperator is operator mode: parse the flags, run the preflight with the
// operator's kube context, print the report, return the exit code. Nothing is
// written to the cluster and nothing is sent.
func runOperator(args []string, stdout, stderr io.Writer, lookup func(string) (string, bool), stdoutTTY, stderrTTY bool) int {
	o, err := cli.Parse(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return cli.ExitOK
	}
	if err != nil {
		fmt.Fprintf(stderr, "app-chart-preflight: %v\n", err)
		return cli.ExitNoVerdict
	}
	if o.Version {
		fmt.Fprintf(stdout, "app-chart-preflight %s (%s)\n", version, commit)
		return cli.ExitOK
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "app-chart-preflight: "+format+"\n", a...)
		return cli.ExitNoVerdict
	}

	work, err := os.MkdirTemp("", "app-chart-preflight-")
	if err != nil {
		return fail("%v", err)
	}
	defer os.RemoveAll(work) // holds a copy of the kubeconfig
	kcPath, kubeContext, err := argocd.WriteContextKubeconfig(work, o.Context, o.ArgocdNamespace)
	if err != nil {
		return fail("%v", err)
	}
	cfg := o.Config(cli.Captain(o.Captain, cli.Cwd(), kubeContext))

	level := "error"
	if o.Verbose {
		level = "info"
	}
	log := logging.New(level, stderr)
	signalled, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalled, cfg.RunTimeout)
	defer cancel()

	start := time.Now()
	fmt.Fprintf(stderr, "Checking context %s (Argo CD in %s) with %s...\n", kubeContext, cfg.ArgocdNamespace, cfg.ArgocdBin)
	deps, stop, err := connect(ctx, cfg, kcPath, work, log)
	if err != nil {
		return fail("%v", err)
	}
	defer stop()
	renders := textreport.Renders{}
	deps.Renders = func(appSet, app string, current, target []manifests.PodTemplate) {
		renders[textreport.Key(appSet, app)] = textreport.Pair{Current: current, Target: target}
	}
	deps.Progress = progress(stderr, stderrTTY)

	rep, err := agent.Preflight(ctx, cfg, deps, log, agent.NewRunID(start), start)
	if err != nil {
		return fail("could not list ApplicationSets and Applications: %v", err)
	}
	if missing := cli.MissingAppSets(rep, o.AppSets); len(missing) > 0 {
		fmt.Fprintf(stderr, "app-chart-preflight: no ApplicationSet that uses the app chart is named %s\n", strings.Join(missing, ", "))
	}
	if o.Save != "" {
		if err := saveYAML(o.Save, rep); err != nil {
			fmt.Fprintf(stderr, "app-chart-preflight: saving the report: %v\n", err)
		} else {
			fmt.Fprintf(stderr, "Full report saved to %s\n", o.Save)
		}
	}
	if o.Format == cli.FormatYAML {
		err = report.WriteYAML(stdout, rep)
	} else {
		_, noColor := lookup("NO_COLOR")
		err = textreport.Write(stdout, rep, renders, textreport.Options{
			Color: stdoutTTY && !o.NoColor && !noColor, Wide: o.Wide, All: o.All, Diff: o.Diff,
			Context: kubeContext, Duration: time.Since(start),
		})
	}
	if err != nil {
		return fail("writing the report: %v", err)
	}
	code := cli.ExitCode(rep)
	if len(cli.MissingAppSets(rep, o.AppSets)) > 0 && code == cli.ExitOK {
		code = cli.ExitNoVerdict
	}
	return code
}

// progress reports "checked N/M" on stderr: one line rewritten in place on a
// terminal, else a line every tenth of the way.
func progress(w io.Writer, tty bool) func(done, total int) {
	last := -1
	return func(done, total int) {
		if tty {
			fmt.Fprintf(w, "\rchecked %d/%d apps", done, total)
			if done == total {
				fmt.Fprint(w, "\r\x1b[K")
			}
			return
		}
		if step := done * 10 / total; step != last || done == total {
			last = step
			fmt.Fprintf(w, "checked %d/%d apps\n", done, total)
		}
	}
}

func saveYAML(path string, rep *schema.Report) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := report.WriteYAML(f, rep); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
