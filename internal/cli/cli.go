// Package cli is operator mode's command line: the flags, how they become a
// run's config, and the exit code a report maps to. An operator runs it from
// the captain codespace right before bumping an ApplicationSet's chart
// revision; it uses their kube context, writes nothing to the cluster and
// sends nothing anywhere.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/glueops/argocd-app-chart-preflight/internal/config"
	"github.com/glueops/argocd-app-chart-preflight/internal/discover"
	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

// Exit codes. A script can gate a bump on ExitOK.
const (
	ExitOK        = 0 // every checked ApplicationSet is ok
	ExitNotSafe   = 1 // a set has a diff or an error: do not bump it as is
	ExitNoVerdict = 2 // a set has no verdict, nothing was checked, or the run could not start
)

// Formats for stdout.
const (
	FormatText = "text"
	FormatYAML = "yaml"
)

// Options are the parsed flags.
type Options struct {
	Target          string
	AppSets         []string          // --appset names, in order given
	Targets         map[string]string // --appset NAME=REV
	Context         string
	ArgocdNamespace string
	ArgocdBin       string
	Captain         string
	Concurrency     int
	Timeout         time.Duration
	ArgocdTimeout   time.Duration
	Diff            bool
	All             bool
	Wide            bool
	NoColor         bool
	Save            string
	Format          string
	Verbose         bool
	Version         bool
}

const usageText = `Usage: app-chart-preflight --target REVISION [flags]
       app-chart-preflight --appset NAME=REVISION [--appset NAME=REVISION ...] [flags]

Renders every Application of the ApplicationSets that use the GlueOps app chart
at a target chart revision, through this cluster's Argo CD, and prints which
apps would change or break. Run it before bumping an ApplicationSet. It uses
your current kube context, writes nothing to the cluster and sends nothing.

The target's form follows how the ApplicationSet sources the chart: 0.18.0 for
helm.gpkg.io or OCI, v0.18.0 (a tag) or a branch for the chart's git repository.

Exit code: 0 every checked set is ok; 1 a set has a diff or an error;
2 no verdict for a set, nothing checked, or the run could not start.

Flags:
`

type appSetFlag struct{ o *Options }

func (f appSetFlag) String() string { return "" }

func (f appSetFlag) Set(v string) error {
	name, rev, hasRev := strings.Cut(v, "=")
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("--appset needs a name")
	}
	f.o.AppSets = append(f.o.AppSets, name)
	if hasRev {
		if strings.TrimSpace(rev) == "" {
			return fmt.Errorf("--appset %s= has an empty revision", name)
		}
		f.o.Targets[name] = strings.TrimSpace(rev)
	}
	return nil
}

// Parse reads the flags. It returns flag.ErrHelp for -h/--help, after
// printing the usage to stderr.
func Parse(args []string, stderr io.Writer) (Options, error) {
	o := Options{Targets: map[string]string{}}
	fs := flag.NewFlagSet("app-chart-preflight", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.Target, "target", "", "chart revision to check every ApplicationSet against (0.18.0, v0.18.0 or a branch)")
	fs.Var(appSetFlag{&o}, "appset", "check only this ApplicationSet; NAME=REV also sets its target (repeatable; \"standalone\" = apps outside any set)")
	fs.StringVar(&o.Context, "context", "", "kube context (default: the current context)")
	fs.StringVar(&o.ArgocdNamespace, "argocd-namespace", "glueops-core", "namespace Argo CD runs in")
	fs.StringVar(&o.ArgocdBin, "argocd-bin", "argocd", "Argo CD CLI to use")
	fs.StringVar(&o.Captain, "captain", "", "captain domain for the header and saved report (default: the current directory or kube context name)")
	fs.IntVar(&o.Concurrency, "concurrency", 4, "apps checked in parallel")
	fs.DurationVar(&o.Timeout, "timeout", 60*time.Minute, "stop starting new checks after this long and report what finished")
	fs.DurationVar(&o.ArgocdTimeout, "argocd-timeout", 2*time.Minute, "limit for one render")
	fs.BoolVar(&o.Diff, "diff", true, "print the pod-template diff of every app that changes (--diff=false to leave it out)")
	noDiff := fs.Bool("no-diff", false, "same as --diff=false")
	fs.BoolVar(&o.All, "all", false, "also list clean and skipped apps one by one")
	fs.BoolVar(&o.Wide, "wide", false, "do not truncate values, hints and reasons")
	fs.BoolVar(&o.NoColor, "no-color", false, "no colour (also off when stdout is not a terminal, or NO_COLOR is set)")
	fs.StringVar(&o.Save, "save", "", "also write the full report as YAML to this file")
	fs.StringVar(&o.Format, "format", FormatText, "stdout format: text or yaml")
	fs.BoolVar(&o.Verbose, "verbose", false, "log progress details to stderr")
	fs.BoolVar(&o.Version, "version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if *noDiff {
		o.Diff = false
	}
	if o.Version {
		return o, nil
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q (flags only; see --help)", fs.Arg(0))
	}
	if o.Format != FormatText && o.Format != FormatYAML {
		return o, fmt.Errorf("--format must be text or yaml, got %q", o.Format)
	}
	if o.Concurrency < 1 {
		return o, errors.New("--concurrency must be at least 1")
	}
	if o.Target == "" {
		if len(o.AppSets) == 0 {
			return o, errors.New("give --target REVISION, or --appset NAME=REVISION for each set to check")
		}
		var missing []string
		for _, n := range o.AppSets {
			if _, ok := o.Targets[n]; !ok {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			return o, fmt.Errorf("no target for %s: give --target, or NAME=REVISION", strings.Join(missing, ", "))
		}
	}
	return o, nil
}

// Config is the run's config for these options. Operator mode never writes
// a ConfigMap or posts a report.
func (o Options) Config(captain string) config.Config {
	return config.Config{
		CaptainDomain:   captain,
		ArgocdNamespace: o.ArgocdNamespace,
		TargetRevision:  o.Target,
		Targets:         o.Targets,
		OnlyAppSets:     o.AppSets,
		Concurrency:     o.Concurrency,
		RunTimeout:      o.Timeout,
		ArgocdBin:       o.ArgocdBin,
		ArgocdTimeout:   o.ArgocdTimeout,
	}
}

// Captain picks the name shown for the cluster: the flag, else the current
// directory when it looks like a captain domain (operators run from the
// captain repo, named after its domain), else the kube context.
func Captain(flagValue, cwd, kubeContext string) string {
	if flagValue != "" {
		return flagValue
	}
	if base := filepath.Base(cwd); config.ValidCaptainDomain(base) {
		return base
	}
	return kubeContext
}

// Cwd is os.Getwd without the error.
func Cwd() string {
	d, _ := os.Getwd()
	return d
}

// ExitCode maps a report to the exit code.
func ExitCode(rep *schema.Report) int {
	if rep == nil {
		return ExitNoVerdict
	}
	checked := false
	unknown := false
	for _, s := range rep.AppSets {
		switch s.Status {
		case schema.GroupDiff, schema.GroupError:
			return ExitNotSafe
		case schema.GroupUnknown:
			unknown = true
		case schema.GroupOK:
			checked = true
		}
	}
	if unknown || !checked {
		return ExitNoVerdict
	}
	return ExitOK
}

// MissingAppSets lists the --appset names the report has no set for: a typo,
// or a set that does not use the app chart.
func MissingAppSets(rep *schema.Report, names []string) []string {
	have := map[string]bool{}
	if rep != nil {
		for _, s := range rep.AppSets {
			have[s.Name] = true
		}
	}
	var out []string
	for _, n := range names {
		if !have[n] && n != discover.Standalone {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// IsOperatorInvocation says whether to run operator mode: any argument, or no
// arguments outside a pod (no ServiceAccount token and no CAPTAIN_DOMAIN, so
// the job could not run anyway; the operator gets the usage).
func IsOperatorInvocation(args []string, lookup func(string) (string, bool), saTokenPath string) bool {
	if len(args) > 0 {
		return true
	}
	if v, ok := lookup("CAPTAIN_DOMAIN"); ok && strings.TrimSpace(v) != "" {
		return false
	}
	if _, err := os.Stat(saTokenPath); err == nil {
		return false
	}
	return true
}
