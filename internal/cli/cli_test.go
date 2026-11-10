package cli

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glueops/argocd-app-chart-preflight/internal/schema"
)

func TestParse(t *testing.T) {
	var stderr bytes.Buffer
	o, err := Parse([]string{"--target", "0.18.0", "--appset", "prod", "--appset", "previews=0.18.1", "--no-diff", "--wide", "--concurrency", "6", "--timeout", "30m", "--save", "r.yaml"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if o.Target != "0.18.0" || !reflect.DeepEqual(o.AppSets, []string{"prod", "previews"}) || !reflect.DeepEqual(o.Targets, map[string]string{"previews": "0.18.1"}) {
		t.Errorf("targets: %+v", o)
	}
	if o.Diff || !o.Wide || o.Concurrency != 6 || o.Timeout != 30*time.Minute || o.Save != "r.yaml" || o.Format != FormatText || o.ArgocdNamespace != "glueops-core" || o.ArgocdBin != "argocd" {
		t.Errorf("options: %+v", o)
	}
	c := o.Config("nonprod.venus.onglueops.rocks")
	if c.TargetRevision != "0.18.0" || !reflect.DeepEqual(c.OnlyAppSets, o.AppSets) || c.IngestURL != "" || c.StatusConfigMap != "" || c.RunTimeout != 30*time.Minute {
		t.Errorf("config: %+v", c)
	}

	// Defaults: diff on, 4 in parallel, an hour.
	o, err = Parse([]string{"--appset", "a=v0.18.0"}, &stderr)
	if err != nil || !o.Diff || o.Concurrency != 4 || o.Timeout != time.Hour || o.Target != "" {
		t.Errorf("defaults: %+v, %v", o, err)
	}
}

func TestParseErrors(t *testing.T) {
	for args, want := range map[string]string{
		"":                           "give --target",
		"--appset a":                 "no target for a",
		"--appset a=v1 --appset b":   "no target for b",
		"--appset =v1":               "needs a name",
		"--appset a=":                "empty revision",
		"--target 1 --format json":   "--format must be text or yaml",
		"--target 1 --concurrency 0": "at least 1",
		"--target 1 extra":           "unexpected argument",
		"--target":                   "flag needs an argument",
	} {
		var stderr bytes.Buffer
		_, err := Parse(strings.Fields(args), &stderr)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", args, err, want)
		}
	}
	var stderr bytes.Buffer
	if _, err := Parse([]string{"--help"}, &stderr); !errors.Is(err, flag.ErrHelp) || !strings.Contains(stderr.String(), "Exit code: 0") {
		t.Errorf("help: %v\n%s", err, stderr.String())
	}
	if o, err := Parse([]string{"--version"}, &stderr); err != nil || !o.Version {
		t.Errorf("version: %v", err)
	}
}

func sets(statuses ...string) *schema.Report {
	r := &schema.Report{}
	for i, s := range statuses {
		r.AppSets = append(r.AppSets, schema.AppSet{Name: string(rune('a' + i)), Status: s})
	}
	return r
}

func TestExitCode(t *testing.T) {
	for _, c := range []struct {
		rep  *schema.Report
		want int
	}{
		{sets(schema.GroupOK, schema.GroupSkipped), ExitOK},
		{sets(schema.GroupOK, schema.GroupDiff), ExitNotSafe},
		{sets(schema.GroupUnknown, schema.GroupError), ExitNotSafe},
		{sets(schema.GroupOK, schema.GroupUnknown), ExitNoVerdict},
		{sets(schema.GroupSkipped), ExitNoVerdict},
		{sets(), ExitNoVerdict},
		{nil, ExitNoVerdict},
	} {
		var names []string
		if c.rep != nil {
			for _, s := range c.rep.AppSets {
				names = append(names, s.Status)
			}
		}
		if got := ExitCode(c.rep); got != c.want {
			t.Errorf("%v: exit %d, want %d", names, got, c.want)
		}
	}
}

func TestMissingAppSets(t *testing.T) {
	r := &schema.Report{AppSets: []schema.AppSet{{Name: "prod"}}}
	if got := MissingAppSets(r, []string{"prod", "prdo", "standalone"}); !reflect.DeepEqual(got, []string{"prdo"}) {
		t.Errorf("missing = %v", got)
	}
}

func TestCaptain(t *testing.T) {
	if Captain("x.example.com", "/w/nonprod.venus.onglueops.rocks", "ctx") != "x.example.com" ||
		Captain("", "/w/nonprod.venus.onglueops.rocks", "ctx") != "nonprod.venus.onglueops.rocks" ||
		Captain("", "/home/me", "k3d-captain") != "k3d-captain" {
		t.Error("captain name resolution")
	}
}

func TestIsOperatorInvocation(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	token := filepath.Join(t.TempDir(), "token")
	if !IsOperatorInvocation([]string{"--target", "1"}, none, token) {
		t.Error("flags must mean operator mode")
	}
	if !IsOperatorInvocation(nil, none, token) {
		t.Error("no args outside a pod should print the operator usage")
	}
	if IsOperatorInvocation(nil, func(k string) (string, bool) { return "c.example.com", k == "CAPTAIN_DOMAIN" }, token) {
		t.Error("CAPTAIN_DOMAIN set means the job")
	}
	_ = os.WriteFile(token, []byte("t"), 0o600)
	if IsOperatorInvocation(nil, none, token) {
		t.Error("a ServiceAccount token means the job")
	}
}
