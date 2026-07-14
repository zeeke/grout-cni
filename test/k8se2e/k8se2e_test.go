//go:build k8se2e

package k8se2e_test

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	groutNamespace = "grout-system"
	groutContainer = "grout"

	// CNI binary baked into the node image, mounted read-only into the grout
	// pod at /host so the suite can assert it was shipped.
	hostCNIBinary = "/host/opt/cni/bin/grout-cni"

	apiTimeout = 30 * time.Second
)

// The grout DaemonSet (deploy/grout-daemonset.yaml) is deployed by the
// `make kind-e2e` target before these tests run. The suite inspects grout's
// runtime state with client-go: listing nodes/pods and exec-ing into the grout
// pods.

// k8sClient builds a clientset and REST config from the ambient kubeconfig.
func k8sClient(t *testing.T) (*kubernetes.Clientset, *rest.Config) {
	t.Helper()
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, &clientcmd.ConfigOverrides{})
	rc, err := cfg.ClientConfig()
	require.NoError(t, err, "building REST config from kubeconfig")
	cs, err := kubernetes.NewForConfig(rc)
	require.NoError(t, err, "building clientset")
	return cs, rc
}

func timeoutContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), apiTimeout)
}

// nodeNames returns the sorted list of node names in the cluster.
func nodeNames(t *testing.T, cs *kubernetes.Clientset) []string {
	t.Helper()
	ctx, cancel := timeoutContext(t)
	defer cancel()
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	require.NoError(t, err, "listing nodes")
	names := make([]string, 0, len(nodes.Items))
	for _, n := range nodes.Items {
		names = append(names, n.Name)
	}
	require.NotEmpty(t, names, "no nodes found")
	sort.Strings(names)
	return names
}

// groutPods returns a node-name -> pod-name map for the grout DaemonSet.
func groutPods(t *testing.T, cs *kubernetes.Clientset) map[string]string {
	t.Helper()
	ctx, cancel := timeoutContext(t)
	defer cancel()
	pods, err := cs.CoreV1().Pods(groutNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=grout"})
	require.NoError(t, err, "listing grout pods")
	out := make(map[string]string, len(pods.Items))
	for _, p := range pods.Items {
		out[p.Spec.NodeName] = p.Name
	}
	require.NotEmpty(t, out, "no grout pods found in namespace %q", groutNamespace)
	return out
}

// execInGrout runs a command in the grout container of a pod and returns stdout.
// The exec is bounded by apiTimeout so a stuck command cannot hang the suite.
func execInGrout(t *testing.T, cs *kubernetes.Clientset, rc *rest.Config, pod string, command ...string) (string, error) {
	t.Helper()
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(groutNamespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: groutContainer,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(rc, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("creating executor: %w", err)
	}

	ctx, cancel := timeoutContext(t)
	defer cancel()

	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return stdout.String(), fmt.Errorf("exec %v: %w: %s", command, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// groutInterfaceDump returns `grcli interface show` from the grout pod on the
// given node, for failure diagnostics. Best-effort: it builds its own REST
// config so it can run from cleanup hooks that carry only a clientset, and
// degrades to an explanatory string rather than failing the test.
func groutInterfaceDump(ctx context.Context, cs *kubernetes.Clientset, node string) string {
	pod := groutPodOnNode(ctx, cs, node)
	if pod == "" {
		return fmt.Sprintf("(no grout pod on node %s)", node)
	}
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return fmt.Sprintf("(could not build REST config: %v)", err)
	}
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(groutNamespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: groutContainer,
			Command:   []string{"grcli", "-s", "/run/grout/grout.sock", "interface", "show"},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(rc, "POST", req.URL())
	if err != nil {
		return fmt.Sprintf("(creating executor: %v)", err)
	}
	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return fmt.Sprintf("(grcli interface show failed: %v: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String()
}

func TestK8s_ClusterAvailable(t *testing.T) {
	cs, _ := k8sClient(t)
	ver, err := cs.Discovery().ServerVersion()
	require.NoError(t, err, "querying API server version")
	t.Logf("Kubernetes API server: %s", ver.String())
}

func TestK8s_NodesReady(t *testing.T) {
	cs, _ := k8sClient(t)
	ctx, cancel := timeoutContext(t)
	defer cancel()
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	require.NoError(t, err, "listing nodes")
	require.NotEmpty(t, nodes.Items, "no nodes found")

	for _, n := range nodes.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				ready = c.Status == corev1.ConditionTrue
				break
			}
		}
		assert.True(t, ready, "node %s is not Ready", n.Name)
	}
	t.Logf("All %d nodes Ready", len(nodes.Items))
}

func TestK8s_GroutDaemonSetOnEveryNode(t *testing.T) {
	cs, _ := k8sClient(t)
	nodes := nodeNames(t, cs)
	pods := groutPods(t, cs)
	require.Len(t, pods, len(nodes), "expected one grout pod per node (nodes=%v pods=%v)", nodes, pods)
	for _, node := range nodes {
		_, ok := pods[node]
		assert.True(t, ok, "no grout pod scheduled on node %s", node)
	}
	t.Logf("grout DaemonSet present on all %d nodes", len(nodes))
}

func TestK8s_CNIBinaryOnEveryNode(t *testing.T) {
	cs, rc := k8sClient(t)
	for node, pod := range groutPods(t, cs) {
		t.Run(node, func(t *testing.T) {
			t.Parallel()
			_, err := execInGrout(t, cs, rc, pod, "sh", "-c", "test -x "+hostCNIBinary)
			require.NoError(t, err, "CNI binary %s not executable on %s", hostCNIBinary, node)
			t.Logf("CNI binary present on %s", node)
		})
	}
}

func TestK8s_GroutVersion(t *testing.T) {
	cs, rc := k8sClient(t)
	nodes := nodeNames(t, cs)
	pods := groutPods(t, cs)
	node := nodes[0]
	pod := pods[node]
	require.NotEmpty(t, pod, "no grout pod on node %s", node)

	out, err := execInGrout(t, cs, rc, pod, "grout", "-V")
	require.NoError(t, err, "grout -V failed on %s", node)
	assert.Contains(t, out, "grout", "unexpected grout version output on %s: %s", node, out)
	t.Logf("grout version on %s: %s", node, strings.TrimSpace(out))
}
