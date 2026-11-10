package argocd

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// The local API server (see Server) takes the Argo CD namespace from the kube
// context — in a pod the default would be the agent's
// own namespace, not glueops-core. So the agent writes a one-context kubeconfig
// from the mounted ServiceAccount with the namespace set, and points the CLI at
// it. On a workstation KUBECONFIG is honoured as-is (dev), and operator mode
// writes a one-context copy of the operator's kubeconfig (WriteContextKubeconfig).

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

// WriteContextKubeconfig is operator mode's kubeconfig: the operator's own
// (KUBECONFIG or ~/.kube/config, as kubectl reads it), reduced to one context,
// with certificate files inlined and the context's namespace set to Argo CD's.
// Auth entries are copied as they are, so exec plugins (aws eks get-token and
// the like) keep working. It returns the file and the context name used.
func WriteContextKubeconfig(dir, contextName, argocdNamespace string) (path, usedContext string, err error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil {
		return "", "", fmt.Errorf("reading your kubeconfig: %w", err)
	}
	usedContext = contextName
	if usedContext == "" {
		usedContext = raw.CurrentContext
	}
	if usedContext == "" {
		return "", "", fmt.Errorf("no current kube context: pass --context or run kubectl config use-context")
	}
	kctx, ok := raw.Contexts[usedContext]
	if !ok {
		return "", "", fmt.Errorf("kube context %q not found in your kubeconfig", usedContext)
	}
	kctx.Namespace = argocdNamespace
	raw.CurrentContext = usedContext
	if err := clientcmdapi.MinifyConfig(&raw); err != nil {
		return "", "", fmt.Errorf("kube context %q: %w", usedContext, err)
	}
	if err := clientcmdapi.FlattenConfig(&raw); err != nil {
		return "", "", fmt.Errorf("kube context %q: %w", usedContext, err)
	}
	path = filepath.Join(dir, "kubeconfig")
	if err := clientcmd.WriteToFile(raw, path); err != nil {
		return "", "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", "", err
	}
	return path, usedContext, nil
}
