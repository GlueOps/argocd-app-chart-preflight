package argocd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

const operatorKubeconfig = `apiVersion: v1
kind: Config
clusters:
  - name: captain
    cluster:
      server: https://captain.example:6443
      certificate-authority: ca.crt
  - name: other
    cluster: {server: https://other.example}
users:
  - name: eks
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1beta1
        command: aws
        args: [eks, get-token, --cluster-name, captain]
  - name: other
    user: {token: other-token}
contexts:
  - name: captain
    context: {cluster: captain, user: eks, namespace: default}
  - name: other
    context: {cluster: other, user: other}
current-context: other
`

func TestWriteContextKubeconfig(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "ca.crt"), []byte("CA-PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	kc := filepath.Join(src, "config")
	if err := os.WriteFile(kc, []byte(operatorKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kc)
	out := t.TempDir()

	path, used, err := WriteContextKubeconfig(out, "captain", "glueops-core")
	if err != nil {
		t.Fatal(err)
	}
	if used != "captain" {
		t.Errorf("context = %q", used)
	}
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "captain" || len(cfg.Contexts) != 1 || len(cfg.Clusters) != 1 || len(cfg.AuthInfos) != 1 {
		t.Fatalf("not minified: %+v", cfg)
	}
	if ns := cfg.Contexts["captain"].Namespace; ns != "glueops-core" {
		t.Errorf("namespace = %q", ns)
	}
	if ex := cfg.AuthInfos["eks"].Exec; ex == nil || ex.Command != "aws" || strings.Join(ex.Args, " ") != "eks get-token --cluster-name captain" {
		t.Errorf("exec auth not kept: %+v", ex)
	}
	if string(cfg.Clusters["captain"].CertificateAuthorityData) != "CA-PEM" {
		t.Error("certificate file not inlined")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode())
	}

	// Default is the current context; an unknown one is an error naming it.
	if _, used, err := WriteContextKubeconfig(t.TempDir(), "", "glueops-core"); err != nil || used != "other" {
		t.Errorf("current context: %q, %v", used, err)
	}
	if _, _, err := WriteContextKubeconfig(t.TempDir(), "nope", "glueops-core"); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("unknown context error = %v", err)
	}
}
