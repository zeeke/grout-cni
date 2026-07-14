//go:build k8se2e

package k8se2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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
	"k8s.io/client-go/tools/remotecommand"
)

// These tests exercise the Multus secondary-CNI scenario: a pod annotated with
// the grout-net NetworkAttachmentDefinition must receive a working secondary
// interface (net1) backed by a grout TAP. The grout DaemonSet, Multus, and the
// NAD are deployed by the `make kind-e2e` target before the suite runs.

const (
	nadName         = "grout-net"
	nadNamespace    = "default"
	multusNamespace = "kube-system"
	podImage        = "busybox:1.36"
	secondaryIf     = "net1"
	secondaryCIDR   = "10.243.0."
	podReadyWait    = 90 * time.Second
)

// multusNetworkStatus mirrors the entries in the
// k8s.v1.cni.cncf.io/network-status annotation Multus writes onto attached pods.
type multusNetworkStatus struct {
	Name      string   `json:"name"`
	Interface string   `json:"interface"`
	IPs       []string `json:"ips"`
}

// execInPod runs a command in a pod's first container and returns stdout.
func execInPod(t *testing.T, cs *kubernetes.Clientset, rc *rest.Config, namespace, pod string, command ...string) (string, error) {
	t.Helper()
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: command,
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(rc, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("creating executor: %w", err)
	}
	ctx, cancel := timeoutContext(t)
	defer cancel()

	var stdout, stderr strings.Builder
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return stdout.String(), fmt.Errorf("exec %v: %w: %s", command, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// startNADPod creates a sleeping busybox pod attached to the grout NAD and pinned
// to the given node, registering cleanup. It returns the created pod name.
func startNADPod(t *testing.T, cs *kubernetes.Clientset, name, node string) string {
	return startNADPodNet(t, cs, name, node, nadName)
}

// startNADPodNet is startNADPod for an arbitrary NAD network name.
func startNADPodNet(t *testing.T, cs *kubernetes.Clientset, name, node, network string) string {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name + "-",
			Namespace:    nadNamespace,
			Annotations: map[string]string{
				"k8s.v1.cni.cncf.io/networks": network,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      node,
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:  "main",
				Image: podImage,
				// Image is preloaded into the cluster by `make kind-e2e`; never
				// reach out to a registry from the kind node.
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sleep", "3600"},
			}},
		},
	}

	ctx, cancel := timeoutContext(t)
	defer cancel()
	created, err := cs.CoreV1().Pods(nadNamespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err, "creating pod %s", name)
	podName := created.Name

	t.Cleanup(func() {
		// On failure, dump cluster state before deleting the pod so CI logs
		// explain why the attachment did not come up.
		if t.Failed() {
			dumpPodDiagnostics(t, cs, podName)
		}
		delCtx, delCancel := context.WithTimeout(context.Background(), apiTimeout)
		defer delCancel()
		_ = cs.CoreV1().Pods(nadNamespace).Delete(delCtx, podName, metav1.DeleteOptions{})
	})
	return podName
}

// dumpPodDiagnostics logs the pod's phase, conditions, container states, the
// Multus network-status annotation, the Kubernetes events for the pod, and the
// grout container log from the node the pod is scheduled on. It is best-effort:
// every step degrades to a log line rather than failing the test, so it can run
// from a cleanup hook without masking the original failure.
func dumpPodDiagnostics(t *testing.T, cs *kubernetes.Clientset, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()

	pod, err := cs.CoreV1().Pods(nadNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Logf("DIAG: cannot get pod %s: %v", name, err)
		return
	}

	t.Logf("DIAG pod %s: phase=%s node=%s", name, pod.Status.Phase, pod.Spec.NodeName)
	for _, c := range pod.Status.Conditions {
		t.Logf("DIAG   condition %s=%s reason=%q msg=%q", c.Type, c.Status, c.Reason, c.Message)
	}
	for _, cst := range pod.Status.ContainerStatuses {
		t.Logf("DIAG   container %s ready=%v state=%+v", cst.Name, cst.Ready, cst.State)
	}
	if ns := pod.Annotations["k8s.v1.cni.cncf.io/network-status"]; ns != "" {
		t.Logf("DIAG   network-status: %s", ns)
	}

	// The NAD as actually stored — confirms the delegated spec.config and its name.
	nadRaw, err := cs.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("/apis/k8s.cni.cncf.io/v1/namespaces/%s/network-attachment-definitions/%s", nadNamespace, nadName)).
		DoRaw(ctx)
	if err != nil {
		t.Logf("DIAG: fetching NAD %s/%s: %v", nadNamespace, nadName, err)
	} else {
		t.Logf("DIAG NAD %s/%s:\n%s", nadNamespace, nadName, string(nadRaw))
	}

	events, err := cs.CoreV1().Events(nadNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + name,
	})
	if err != nil {
		t.Logf("DIAG: listing events for %s: %v", name, err)
	} else {
		for _, e := range events.Items {
			t.Logf("DIAG   event %s/%s (%s): %s", e.Type, e.Reason, e.Source.Component, strings.TrimSpace(e.Message))
		}
	}

	if pod.Spec.NodeName != "" {
		if gp := groutPodOnNode(ctx, cs, pod.Spec.NodeName); gp != "" {
			t.Logf("DIAG grout pod %s log (tail) on node %s:\n%s", gp, pod.Spec.NodeName, groutPodLogTail(ctx, cs, gp, 50))
		} else {
			t.Logf("DIAG: no grout pod found on node %s", pod.Spec.NodeName)
		}
		// grout's own view of its interfaces (bridges, ports, domains) — shows how
		// a bridge/port is represented when a CNI ADD fails to find or create it.
		t.Logf("DIAG grout interfaces on node %s:\n%s", pod.Spec.NodeName, groutInterfaceDump(ctx, cs, pod.Spec.NodeName))
		if mp, c := multusPodOnNode(ctx, cs, pod.Spec.NodeName); mp != "" {
			t.Logf("DIAG multus pod %s log (tail) on node %s:\n%s", mp, pod.Spec.NodeName, podLogTail(ctx, cs, multusNamespace, mp, c, 30))
		}
	}
}

// multusPodOnNode returns the Multus DaemonSet pod and its container name on the
// given node, or "" if none is found. Best-effort.
func multusPodOnNode(ctx context.Context, cs *kubernetes.Clientset, node string) (string, string) {
	pods, err := cs.CoreV1().Pods(multusNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=multus"})
	if err != nil {
		return "", ""
	}
	for _, p := range pods.Items {
		if p.Spec.NodeName == node && len(p.Spec.Containers) > 0 {
			return p.Name, p.Spec.Containers[0].Name
		}
	}
	return "", ""
}

// groutPodOnNode returns the grout DaemonSet pod scheduled on the given node, or
// "" if none is found. Best-effort: returns "" on any API error.
func groutPodOnNode(ctx context.Context, cs *kubernetes.Clientset, node string) string {
	pods, err := cs.CoreV1().Pods(groutNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=grout"})
	if err != nil {
		return ""
	}
	for _, p := range pods.Items {
		if p.Spec.NodeName == node {
			return p.Name
		}
	}
	return ""
}

// groutPodLogTail returns the last tailLines of the grout container log, or an
// error string if the log cannot be fetched.
func groutPodLogTail(ctx context.Context, cs *kubernetes.Clientset, pod string, tailLines int64) string {
	return podLogTail(ctx, cs, groutNamespace, pod, groutContainer, tailLines)
}

// podLogTail returns the last tailLines of a pod container's log, or an error
// string if the log cannot be fetched. Best-effort: never fails the test.
func podLogTail(ctx context.Context, cs *kubernetes.Clientset, namespace, pod, container string, tailLines int64) string {
	raw, err := cs.CoreV1().Pods(namespace).
		GetLogs(pod, &corev1.PodLogOptions{Container: container, TailLines: &tailLines}).
		DoRaw(ctx)
	if err != nil {
		return fmt.Sprintf("(could not fetch logs: %v)", err)
	}
	return string(raw)
}

// waitPodReady blocks until the pod is Running and Ready, or the deadline passes.
func waitPodReady(t *testing.T, cs *kubernetes.Clientset, name string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(podReadyWait)
	var last *corev1.Pod
	for time.Now().Before(deadline) {
		ctx, cancel := timeoutContext(t)
		p, err := cs.CoreV1().Pods(nadNamespace).Get(ctx, name, metav1.GetOptions{})
		cancel()
		require.NoError(t, err, "getting pod %s", name)
		last = p
		if p.Status.Phase == corev1.PodRunning {
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					return p
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	require.FailNow(t, "pod not ready", "pod %s did not become Ready in %s (phase=%s)", name, podReadyWait, last.Status.Phase)
	return nil
}

// secondaryIPFromStatus parses the Multus network-status annotation and returns
// the IP assigned to the grout secondary interface.
func secondaryIPFromStatus(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	raw := pod.Annotations["k8s.v1.cni.cncf.io/network-status"]
	require.NotEmpty(t, raw, "pod %s missing network-status annotation", pod.Name)

	var statuses []multusNetworkStatus
	require.NoError(t, json.Unmarshal([]byte(raw), &statuses), "parsing network-status")
	for _, s := range statuses {
		if s.Interface == secondaryIf && len(s.IPs) > 0 {
			return s.IPs[0]
		}
	}
	require.FailNow(t, "no secondary IP", "interface %s not found in network-status of %s: %s", secondaryIf, pod.Name, raw)
	return ""
}

// interfaceIPsFromStatus parses the Multus network-status annotation and
// returns all IPs assigned to the given pod interface — one per address
// family on a dual-stack attachment.
func interfaceIPsFromStatus(t *testing.T, pod *corev1.Pod, iface string) []string {
	t.Helper()
	raw := pod.Annotations["k8s.v1.cni.cncf.io/network-status"]
	require.NotEmpty(t, raw, "pod %s missing network-status annotation", pod.Name)

	var statuses []multusNetworkStatus
	require.NoError(t, json.Unmarshal([]byte(raw), &statuses), "parsing network-status")
	for _, s := range statuses {
		if s.Interface == iface && len(s.IPs) > 0 {
			return s.IPs
		}
	}
	require.FailNow(t, "no interface IPs", "interface %s not found in network-status of %s: %s", iface, pod.Name, raw)
	return nil
}

// networkIPFromStatus returns the first IP of the network whose status name
// contains the given substring (e.g. a virtio attachment, which carries no
// "interface" field because it has no kernel netdev).
func networkIPFromStatus(t *testing.T, pod *corev1.Pod, network string) string {
	t.Helper()
	raw := pod.Annotations["k8s.v1.cni.cncf.io/network-status"]
	require.NotEmpty(t, raw, "pod %s missing network-status annotation", pod.Name)

	var statuses []multusNetworkStatus
	require.NoError(t, json.Unmarshal([]byte(raw), &statuses), "parsing network-status")
	for _, s := range statuses {
		if strings.Contains(s.Name, network) && len(s.IPs) > 0 {
			return s.IPs[0]
		}
	}
	require.FailNow(t, "no network IP", "network %q not found in network-status of %s: %s", network, pod.Name, raw)
	return ""
}

func TestK8s_NADPodGetsGroutInterface(t *testing.T) {
	cs, rc := k8sClient(t)
	node := nodeNames(t, cs)[0]

	name := startNADPod(t, cs, "grout-nad-pod", node)
	pod := waitPodReady(t, cs, name)

	ip := secondaryIPFromStatus(t, pod)
	assert.True(t, strings.HasPrefix(ip, secondaryCIDR), "secondary IP %q not in expected range %s0/24", ip, secondaryCIDR)
	t.Logf("pod %s secondary IP: %s", name, ip)

	// The interface must exist, be up, and carry the assigned address.
	out, err := execInPod(t, cs, rc, nadNamespace, name, "ip", "addr", "show", "dev", secondaryIf)
	require.NoError(t, err, "inspecting %s in pod", secondaryIf)
	assert.Contains(t, out, "UP", "interface %s is not UP: %s", secondaryIf, out)
	assert.Contains(t, out, ip, "interface %s missing address %s: %s", secondaryIf, ip, out)
}

// TestK8s_NADPodsConnectivity verifies pod-to-pod connectivity: two pods on the
// same node and NAD reach each other through the grout L2 bridge.
func TestK8s_NADPodsConnectivity(t *testing.T) {
	cs, rc := k8sClient(t)
	node := nodeNames(t, cs)[0]

	a := startNADPod(t, cs, "grout-nad-a", node)
	b := startNADPod(t, cs, "grout-nad-b", node)
	waitPodReady(t, cs, a)
	podB := waitPodReady(t, cs, b)

	ipB := secondaryIPFromStatus(t, podB)
	t.Logf("pinging %s (%s) from %s", b, ipB, a)

	out, err := execInPod(t, cs, rc, nadNamespace, a, "ping", "-c", "3", "-W", "2", ipB)
	require.NoError(t, err, "ping %s from %s failed: %s", ipB, a, out)
	assert.Contains(t, out, "0% packet loss", "unexpected packet loss pinging %s: %s", ipB, out)
}

const (
	// grout-dual (deploy/nad-dual.yaml) hands out one
	// address per family: IPv4 from 10.246.0.0/24, IPv6 from fd10:246::/64.
	dualNet     = "grout-dual"
	dualCIDR4   = "10.246.0."
	dualPrefix6 = "fd10:246:"
)

// TestK8s_NADDualStack verifies the dual-stack attachment end to end: a pod on
// the grout-dual NAD gets one secondary-interface address per family, both are
// configured on net1, and two pods on the NAD reach each other over IPv4 and
// IPv6 through the same grout bridge (ARP and NDP both flooded through grout's
// L2 forwarding).
func TestK8s_NADDualStack(t *testing.T) {
	cs, rc := k8sClient(t)
	node := nodeNames(t, cs)[0]

	a := startNADPodNet(t, cs, "grout-dual-a", node, dualNet)
	b := startNADPodNet(t, cs, "grout-dual-b", node, dualNet)
	waitPodReady(t, cs, a)
	podB := waitPodReady(t, cs, b)

	ipsB := interfaceIPsFromStatus(t, podB, secondaryIf)
	var ip4, ip6 string
	for _, s := range ipsB {
		parsed := net.ParseIP(s)
		require.NotNil(t, parsed, "network-status IP %q of pod %s does not parse", s, b)
		if parsed.To4() != nil {
			ip4 = s
		} else {
			ip6 = s
		}
	}
	require.NotEmpty(t, ip4, "pod %s got no IPv4 on %s: %v", b, secondaryIf, ipsB)
	require.NotEmpty(t, ip6, "pod %s got no IPv6 on %s: %v", b, secondaryIf, ipsB)
	assert.True(t, strings.HasPrefix(ip4, dualCIDR4), "IPv4 %q not in expected range %s0/24", ip4, dualCIDR4)
	assert.True(t, strings.HasPrefix(strings.ToLower(ip6), dualPrefix6), "IPv6 %q not in expected prefix %s:/64", ip6, dualPrefix6)
	t.Logf("pod %s dual-stack IPs: %s / %s", b, ip4, ip6)

	// Both addresses must be configured on the pod interface.
	out, err := execInPod(t, cs, rc, nadNamespace, b, "ip", "addr", "show", "dev", secondaryIf)
	require.NoError(t, err, "inspecting %s in pod", secondaryIf)
	assert.Contains(t, out, "UP", "interface %s is not UP: %s", secondaryIf, out)
	assert.Contains(t, out, ip4, "interface %s missing IPv4 %s: %s", secondaryIf, ip4, out)
	assert.Contains(t, out, ip6, "interface %s missing IPv6 %s: %s", secondaryIf, ip6, out)

	out, err = execInPod(t, cs, rc, nadNamespace, a, "ping", "-4", "-c", "3", "-W", "2", ip4)
	require.NoError(t, err, "IPv4 ping %s from %s failed: %s", ip4, a, out)
	assert.Contains(t, out, "0% packet loss", "unexpected IPv4 packet loss pinging %s: %s", ip4, out)

	// IPv6 needs DAD to settle and the first NDP resolution to complete, so
	// retry briefly instead of failing on the first attempt.
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err = execInPod(t, cs, rc, nadNamespace, a, "ping", "-6", "-c", "3", "-W", "2", ip6)
		if err == nil && strings.Contains(out, "0% packet loss") {
			break
		}
		if time.Now().After(deadline) {
			require.NoError(t, err, "IPv6 ping %s from %s failed: %s", ip6, a, out)
			require.Contains(t, out, "0% packet loss", "unexpected IPv6 packet loss pinging %s: %s", ip6, out)
			break
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("pod %s reached %s over IPv4 (%s) and IPv6 (%s)", a, b, ip4, ip6)
}

const testpmdImage = "grout-k-testpmd:e2e"

// testpmdScript waits for the CNI-created vhost-user socket, then runs
// dpdk-testpmd as a vhost-user client in icmpecho mode (replies to ARP + ICMP).
// --no-huge uses memfd-backed memory (shareable over vhost-user since DPDK
// 19.02), so no hugepages are needed.
const testpmdScript = `set -e
for i in $(seq 1 30); do
  sock=$(ls -t /run/grout/vhost/*.sock 2>/dev/null | head -1)
  [ -n "$sock" ] && break
  sleep 1
done
[ -n "$sock" ] || { echo "no vhost socket found under /run/grout/vhost"; exit 1; }
echo "using vhost socket: $sock"
# Non-interactive testpmd forwards packets while blocked on its "Press enter to
# exit" read. The container has no stdin, so feed it a never-closing pipe;
# otherwise testpmd sees EOF and exits immediately.
tail -f /dev/null | dpdk-testpmd -l 0-1 --no-huge -m 256 --no-pci --file-prefix virtio \
  --vdev net_virtio_user0,path=$sock -- \
  --forward-mode=icmpecho --auto-start --total-num-mbufs 8192`

// startTestpmdPod creates a privileged DPDK testpmd pod on the grout-virtio NAD,
// pinned to the node, mounting the host vhost socket directory. Returns its name.
func startTestpmdPod(t *testing.T, cs *kubernetes.Clientset, name, node string) string {
	return startTestpmdPodNet(t, cs, name, node, "grout-virtio")
}

// startTestpmdPodNet is startTestpmdPod for an arbitrary virtio NAD network name,
// so the same DPDK responder can be attached to different grout bridges.
func startTestpmdPodNet(t *testing.T, cs *kubernetes.Clientset, name, node, network string) string {
	t.Helper()
	priv := true
	hostPathDir := corev1.HostPathDirectoryOrCreate
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name + "-",
			Namespace:    nadNamespace,
			Annotations:  map[string]string{"k8s.v1.cni.cncf.io/networks": network},
		},
		Spec: corev1.PodSpec{
			NodeName:      node,
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name:            "testpmd",
				Image:           testpmdImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sh", "-c", testpmdScript},
				SecurityContext: &corev1.SecurityContext{Privileged: &priv},
				VolumeMounts:    []corev1.VolumeMount{{Name: "vhost", MountPath: "/run/grout/vhost"}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "vhost",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/grout/vhost", Type: &hostPathDir}},
			}},
		},
	}

	ctx, cancel := timeoutContext(t)
	defer cancel()
	created, err := cs.CoreV1().Pods(nadNamespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err, "creating testpmd pod")
	podName := created.Name

	t.Cleanup(func() {
		if t.Failed() {
			dumpPodDiagnostics(t, cs, podName)
			lc, lcancel := context.WithTimeout(context.Background(), apiTimeout)
			t.Logf("DIAG testpmd %s log:\n%s", podName, podLogTail(lc, cs, nadNamespace, podName, "testpmd", 60))
			lcancel()
		}
		delCtx, delCancel := context.WithTimeout(context.Background(), apiTimeout)
		defer delCancel()
		_ = cs.CoreV1().Pods(nadNamespace).Delete(delCtx, podName, metav1.DeleteOptions{})
	})
	return podName
}

// TestK8s_VirtioDataplane is the real DPDK virtio dataplane test: a testpmd
// (icmpecho) pod connects to grout's vhost-user socket, and grout pings the
// pod's IP over the bridge. A successful ping proves packets traverse
// grout <-> vhost-user <-> the DPDK app — no kernel stack in the pod's path.
func TestK8s_VirtioDataplane(t *testing.T) {
	cs, rc := k8sClient(t)
	node := nodeNames(t, cs)[0]

	name := startTestpmdPod(t, cs, "grout-virtio", node)
	pod := waitPodReady(t, cs, name)

	// A virtio attachment has no kernel netdev, so its network-status entry has
	// no "interface" field — match it by network name instead.
	ip := networkIPFromStatus(t, pod, "grout-virtio")
	t.Logf("testpmd pod %s virtio IP: %s", name, ip)

	grout := groutPods(t, cs)[node]
	require.NotEmpty(t, grout, "no grout pod on node %s", node)

	// grout originates ICMP from the bridge to the pod's IP; testpmd icmpecho
	// answers ARP + ICMP. Retry: testpmd needs a moment to attach to the socket.
	var out string
	var err error
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, err = execInGrout(t, cs, rc, grout, "grcli", "-s", "/run/grout/grout.sock",
			"ping", ip, "count", "3", "delay", "200")
		if err == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}
	require.NoError(t, err, "grout ping %s failed (no replies): %s", ip, out)
	t.Logf("grout ping %s succeeded:\n%s", ip, strings.TrimSpace(out))
}

const (
	// grout-shared-tap and grout-shared-virtio (deploy/nad-mixed.yaml)
	// share one grout bridge (br-shared) and subnet (10.245.0.0/24), differing
	// only in interface type, with disjoint IPAM ranges.
	sharedTapNet    = "grout-shared-tap"
	sharedVirtioNet = "grout-shared-virtio"
	sharedCIDR      = "10.245.0."
)

// TestK8s_VirtioTapPodToPod verifies mixed-workload pod-to-pod across interface
// types on a single grout bridge: a DPDK testpmd (virtio/vhost-user) pod and a
// kernel-TAP busybox pod attach to the same bridge on the same subnet, and the
// TAP pod pings the DPDK pod. A reply proves grout bridges a kernel-TAP port and
// a vhost-user port into one L2 domain — packets go pod(kernel) -> grout TAP
// port -> br-shared -> grout vhost-user port -> the DPDK app and back, with no
// kernel stack on the DPDK pod's side.
func TestK8s_VirtioTapPodToPod(t *testing.T) {
	cs, rc := k8sClient(t)
	node := nodeNames(t, cs)[0]

	// DPDK responder (icmpecho) on the virtio side of the shared bridge.
	virtio := startTestpmdPodNet(t, cs, "shared-virtio", node, sharedVirtioNet)
	vpod := waitPodReady(t, cs, virtio)
	// A virtio attachment carries no "interface" field, so match by network name.
	virtioIP := networkIPFromStatus(t, vpod, sharedVirtioNet)
	require.True(t, strings.HasPrefix(virtioIP, sharedCIDR), "virtio IP %q not in %s0/24", virtioIP, sharedCIDR)
	t.Logf("testpmd (virtio) pod %s IP: %s", virtio, virtioIP)

	// Kernel-TAP pod on the tap side of the same bridge.
	tap := startNADPodNet(t, cs, "shared-tap", node, sharedTapNet)
	waitPodReady(t, cs, tap)

	// The TAP pod pings the DPDK pod over br-shared. The virtio IP is on-link on
	// the TAP pod's net1 (same /24), so it resolves via ARP directly. Retry:
	// testpmd needs a moment to attach to its vhost-user socket after Ready.
	var out string
	var err error
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, err = execInPod(t, cs, rc, nadNamespace, tap, "ping", "-c", "3", "-W", "2", virtioIP)
		if err == nil && strings.Contains(out, "0% packet loss") {
			break
		}
		time.Sleep(3 * time.Second)
	}
	require.NoError(t, err, "TAP pod %s ping of virtio pod %s (%s) failed: %s", tap, virtio, virtioIP, out)
	assert.Contains(t, out, "0% packet loss", "unexpected packet loss from TAP pod to virtio pod %s: %s", virtioIP, out)
	t.Logf("TAP pod %s pinged virtio pod %s (%s) successfully:\n%s", tap, virtio, virtioIP, strings.TrimSpace(out))
}
