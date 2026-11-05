package argocd

import (
	"fmt"
	"os"
	"path/filepath"
)

// `argocd --core` runs Argo CD's API server in-process and takes the Argo CD
// namespace from the kube context — in a pod the default would be the agent's
// own namespace, not glueops-core. So the agent writes a one-context kubeconfig
// from the mounted ServiceAccount with the namespace set, and points the CLI at
// it. On a workstation KUBECONFIG is honoured as-is (dev).

// KubeconfigOptions for EnsureKubeconfig.
type KubeconfigOptions struct {
	Explicit          string // KUBECONFIG from the environment; used as-is when set
	ServiceAccountDir string
	ArgocdNamespace   string
	Dir               string // where the in-cluster kubeconfig is written (an emptyDir)
	APIServer         string // default https://kubernetes.default.svc
}

// RenderKubeconfig returns the in-cluster kubeconfig text.
func RenderKubeconfig(o KubeconfigOptions) string {
	server := o.APIServer
	if server == "" {
		server = "https://kubernetes.default.svc"
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
  - name: in-cluster
    cluster:
      server: %s
      certificate-authority: %s
users:
  - name: service-account
    user:
      tokenFile: %s
contexts:
  - name: in-cluster
    context:
      cluster: in-cluster
      user: service-account
      namespace: %s
current-context: in-cluster
`, server, filepath.Join(o.ServiceAccountDir, "ca.crt"), filepath.Join(o.ServiceAccountDir, "token"), o.ArgocdNamespace)
}

// EnsureKubeconfig returns the kubeconfig path the CLI should use and whether
// it is the explicit one or a freshly written in-cluster one.
func EnsureKubeconfig(o KubeconfigOptions) (path string, source string, err error) {
	if o.Explicit != "" {
		return o.Explicit, "explicit", nil
	}
	if _, err := os.Stat(filepath.Join(o.ServiceAccountDir, "token")); err != nil {
		return "", "", fmt.Errorf("no KUBECONFIG and no ServiceAccount token at %s: cannot reach the cluster", o.ServiceAccountDir)
	}
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return "", "", err
	}
	path = filepath.Join(o.Dir, "kubeconfig")
	if err := os.WriteFile(path, []byte(RenderKubeconfig(o)), 0o600); err != nil {
		return "", "", err
	}
	return path, "in-cluster", nil
}
