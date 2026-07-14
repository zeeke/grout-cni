package cni

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/zeeke/grout-cni/pkg/groutapi"
)

// GroutClient defines the subset of grout API methods needed by the CNI handler.
// The concrete *groutapi.Client satisfies this interface.
// Using an interface allows unit testing the handler logic with a mock.
type GroutClient interface {
	InterfaceList(groutapi.InterfaceListRequest) ([]groutapi.InterfaceInfo, error)
	InterfaceAdd(groutapi.InterfaceAddRequest) (*groutapi.InterfaceAddResponse, error)
	InterfaceDel(groutapi.InterfaceDelRequest) error
	AddressAdd(groutapi.AddressAddRequest) error
	AddressFlush(ifaceID uint16) error
}

// IPAMAddFunc is the function signature for allocating IP through IPAM.
// Defined as a type to allow test injection.
type IPAMAddFunc func(config *PluginConf, args *skel.CmdArgs) (types.Result, error)

// IPAMDelFunc is the function signature for releasing IPAM allocations.
// Defined as a type to allow test injection.
type IPAMDelFunc func(config *PluginConf, args *skel.CmdArgs)

// MoveLinkFunc is the function signature for moving a link to a network namespace.
// Defined as a type to allow test injection.
type MoveLinkFunc func(linkName, netnsPath string) error

// ConfigurePodIfaceFunc is the function signature for configuring the TAP link
// inside the pod network namespace (rename, MTU, address, routes, up).
// Defined as a type to allow test injection.
type ConfigurePodIfaceFunc func(netnsPath, hostIfName, podIfName string, mtu int, result *types100.Result) error

// VerifyPodIfaceFunc is the function signature for verifying the pod-side TAP
// interface during CHECK: it confirms the interface exists, is administratively
// up, and carries the expected addresses. Defined as a type so the netns-bound
// verification can be swapped out in unit tests.
type VerifyPodIfaceFunc func(netnsPath, podIfName string, expectedIPs []*net.IPNet) error

type AddConfig struct {
	Client            GroutClient
	Config            *PluginConf
	Args              *skel.CmdArgs
	IPAMAdd           IPAMAddFunc
	IPAMDel           IPAMDelFunc
	MoveLink          MoveLinkFunc
	ConfigurePodIface ConfigurePodIfaceFunc
}

// DelConfig also serves CHECK.
type DelConfig struct {
	Client  GroutClient
	Config  *PluginConf
	Args    *skel.CmdArgs
	IPAMDel IPAMDelFunc
	// VerifyPodIface verifies the pod-side TAP interface during CHECK. Nil in
	// production (the netns-bound verifyPodInterface is used); tests inject a stub
	// to exercise the dispatch and grout-side logic without a real pod netns.
	VerifyPodIface VerifyPodIfaceFunc
}

func HandleAdd(cfg *AddConfig) (types.Result, error) {
	lock, err := NewFileLock(lockPathForSocket(cfg.Config.GroutSocketPath))
	if err != nil {
		return nil, fmt.Errorf("creating lock: %w", err)
	}
	if err := lock.Lock(); err != nil {
		lock.Close()
		return nil, fmt.Errorf("acquiring lock: %w", err)
	}
	defer func() {
		_ = lock.Unlock()
		lock.Close()
	}()

	// The port name is derived deterministically from (containerID, ifName), so
	// DEL/CHECK/GC can recompute it and find the port in grout's interface list
	// without a node-local ref store. See docs/design/refless-cni-state.md.
	tapName := groutPortName(cfg.Args.ContainerID, cfg.Args.IfName)
	slog.Debug("CNI ADD", "containerID", cfg.Args.ContainerID, "ifName", cfg.Args.IfName, "portName", tapName)

	ipamAddFn := cfg.IPAMAdd
	if ipamAddFn == nil {
		ipamAddFn = delegateIPAMAdd
	}
	result, err := ipamAddFn(cfg.Config, cfg.Args)
	if err != nil {
		return nil, fmt.Errorf("IPAM add: %w", err)
	}

	cniResult, err := convertResult(result)
	if err != nil {
		return nil, fmt.Errorf("converting IPAM result: %w", err)
	}

	// Ensure the per-network grout bridge exists and holds the gateway address.
	// Pod ports become L2 members of this bridge, so pods on the same network
	// reach each other through grout's L2 forwarding. The file lock (held above)
	// serializes concurrent CNI calls, so find-or-create is race-free.
	bridgeID, err := ensureBridge(cfg.Client, cfg.Config.Bridge)
	if err != nil {
		ipamDel(cfg.IPAMDel, cfg.Config, cfg.Args)
		return nil, fmt.Errorf("ensuring grout bridge %q: %w", cfg.Config.Bridge, err)
	}
	slog.Debug("grout bridge ready", "bridge", cfg.Config.Bridge, "ifaceID", bridgeID)

	for _, gw := range gatewaysFor(cniResult) {
		if err := cfg.Client.AddressAdd(groutapi.AddressAddRequest{
			IfaceID: bridgeID,
			Address: *gw,
			ExistOK: true,
		}); err != nil {
			// The bridge is shared; do not tear it down, just release IPAM.
			ipamDel(cfg.IPAMDel, cfg.Config, cfg.Args)
			return nil, fmt.Errorf("grout bridge address add: %w", err)
		}
		slog.Debug("gateway assigned to bridge", "bridge", cfg.Config.Bridge, "gateway", gw.String())
	}

	virtio := cfg.Config.InterfaceType == InterfaceTypeVirtio

	// Add the pod's port as an L2 member of the bridge (no L3 address on the
	// port). For TAP, grout creates a kernel netdev moved into the pod netns;
	// for virtio, grout exposes a vhost-user socket the pod's DPDK app uses.
	addReq := groutapi.InterfaceAddRequest{
		Name:     tapName,
		Type:     groutapi.InterfaceTypeTAP, // a grout PORT in both cases
		DomainID: bridgeID,
		MTU:      uint16(cfg.Config.MTU),
	}
	var sockPath string
	if virtio {
		sockPath = vhostSocketPath(cfg.Config.GroutSocketPath, tapName)
		if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
			ipamDel(cfg.IPAMDel, cfg.Config, cfg.Args)
			return nil, fmt.Errorf("creating vhost socket dir: %w", err)
		}
		addReq.VhostUserPath = sockPath
	}

	addResp, err := cfg.Client.InterfaceAdd(addReq)
	if err != nil {
		ipamDel(cfg.IPAMDel, cfg.Config, cfg.Args)
		return nil, fmt.Errorf("grout interface add: %w", err)
	}
	slog.Debug("port added as bridge member", "ifaceID", addResp.IfaceID, "tapName", tapName, "bridge", cfg.Config.Bridge, "virtio", virtio)

	var podRes *types100.Result
	if virtio {
		// No kernel netdev / netns config: the DPDK app owns L2/L3 and connects
		// to the vhost-user socket. Report the socket path in the result.
		podRes = vhostResult(cfg.Args, cniResult, sockPath)
		slog.Debug("vhost-user interface ready", "ifName", cfg.Args.IfName, "socket", sockPath)
	} else {
		podRes = podResult(cfg.Args, cniResult)
		if cfg.Args.Netns != "" {
			moveFn := cfg.MoveLink
			if moveFn == nil {
				moveFn = moveLinkToNamespace
			}
			if err := moveFn(tapName, cfg.Args.Netns); err != nil {
				cleanupAfterAddFailure(cfg, addResp.IfaceID)
				return nil, fmt.Errorf("moving TAP to pod namespace: %w", err)
			}
			slog.Debug("TAP moved to pod namespace", "tapName", tapName, "netns", cfg.Args.Netns)

			configureFn := cfg.ConfigurePodIface
			if configureFn == nil {
				configureFn = configurePodInterface
			}
			if err := configureFn(cfg.Args.Netns, tapName, cfg.Args.IfName, cfg.Config.MTU, podRes); err != nil {
				cleanupAfterAddFailure(cfg, addResp.IfaceID)
				return nil, fmt.Errorf("configuring pod interface: %w", err)
			}
			slog.Debug("pod interface configured", "ifName", cfg.Args.IfName, "netns", cfg.Args.Netns)
		}
	}

	// No node-local ref is written: grout now holds the mapping (the port name
	// encodes the attachment, its Domain is the bridge, and the vhost socket path
	// is deterministic), so DEL/CHECK/GC recover everything from grout.
	return podRes, nil
}

// vhostSocketPath returns the vhost-user socket path for a virtio interface,
// placed under a "vhost" directory next to grout's control socket so the
// privileged grout DaemonSet (and a pod mounting that host dir) can reach it.
func vhostSocketPath(groutSocketPath, tapName string) string {
	return filepath.Join(filepath.Dir(groutSocketPath), "vhost", tapName+".sock")
}

// vhostResult builds the CNI result for a virtio interface: it reports the
// vhost-user socket path and the allocated IP, with no sandbox (there is no
// kernel netdev). The DPDK app in the pod uses the socket and IP directly.
func vhostResult(args *skel.CmdArgs, ipamResult *types100.Result, socketPath string) *types100.Result {
	result := &types100.Result{
		CNIVersion: ipamResult.CNIVersion,
		Interfaces: []*types100.Interface{{
			Name:       args.IfName,
			SocketPath: socketPath,
		}},
		IPs:    ipamResult.IPs,
		Routes: ipamResult.Routes,
		DNS:    ipamResult.DNS,
	}
	ifaceIdx := 0
	for _, ipc := range result.IPs {
		ipc.Interface = &ifaceIdx
	}
	return result
}

// podResult builds the CNI result returned to the runtime. It describes the pod
// interface (name + sandbox) and points every allocated IP at it by index.
func podResult(args *skel.CmdArgs, ipamResult *types100.Result) *types100.Result {
	result := &types100.Result{
		CNIVersion: ipamResult.CNIVersion,
		Interfaces: []*types100.Interface{{
			Name:    args.IfName,
			Sandbox: args.Netns,
		}},
		IPs:    ipamResult.IPs,
		Routes: ipamResult.Routes,
		DNS:    ipamResult.DNS,
	}
	ifaceIdx := 0
	for _, ipc := range result.IPs {
		ipc.Interface = &ifaceIdx
	}
	return result
}

// ensureBridge returns the iface id of the grout bridge with the given name,
// creating it if it does not exist. Callers must hold the CNI file lock so
// concurrent creates do not race.
func ensureBridge(client GroutClient, name string) (uint16, error) {
	ifaces, err := client.InterfaceList(groutapi.InterfaceListRequest{})
	if err != nil {
		return 0, fmt.Errorf("listing interfaces: %w", err)
	}
	if id, ok := findBridgeID(ifaces, name); ok {
		return id, nil
	}
	resp, err := client.InterfaceAdd(groutapi.InterfaceAddRequest{
		Name: name,
		Type: groutapi.InterfaceTypeBridge,
	})
	if err != nil {
		// The initial list did not surface the bridge, yet the create failed —
		// typically grout returning EEXIST because the bridge already exists (e.g.
		// created by another CNI call for the same network, such as a vhost-user
		// pod). Re-list and reuse the existing interface by name before giving up;
		// grout enforces globally-unique interface names, so a name match is
		// unambiguous even if the list reports its type differently than create.
		// If it is still not found, report what the list did return so a genuine
		// miss (e.g. a truncated list) is diagnosable from the CNI error alone.
		if ifaces2, listErr := client.InterfaceList(groutapi.InterfaceListRequest{}); listErr == nil {
			if id, ok := findByName(ifaces2, name); ok {
				slog.Debug("bridge found by name after create conflict", "bridge", name, "ifaceID", id)
				return id, nil
			}
			ifaces = ifaces2
		}
		return 0, fmt.Errorf("creating bridge (interfaces seen: %s): %w", summarizeIfaces(ifaces), err)
	}
	return resp.IfaceID, nil
}

// findBridgeID returns the id of a bridge-typed interface with the given name.
func findBridgeID(ifaces []groutapi.InterfaceInfo, name string) (uint16, bool) {
	for _, iface := range ifaces {
		if iface.Name == name && iface.Type == groutapi.InterfaceTypeBridge {
			return iface.ID, true
		}
	}
	return 0, false
}

// findByName returns the id of any interface with the given name. grout enforces
// globally-unique interface names, so a name match unambiguously identifies the
// interface regardless of the type reported by the list API.
func findByName(ifaces []groutapi.InterfaceInfo, name string) (uint16, bool) {
	for _, iface := range ifaces {
		if iface.Name == name {
			return iface.ID, true
		}
	}
	return 0, false
}

// summarizeIfaces renders an interface list as "name(id=,type=,domain=)" tuples
// for diagnostics embedded in errors.
func summarizeIfaces(ifaces []groutapi.InterfaceInfo) string {
	parts := make([]string, 0, len(ifaces))
	for _, iface := range ifaces {
		parts = append(parts, fmt.Sprintf("%s(id=%d,type=%d,domain=%d)", iface.Name, iface.ID, iface.Type, iface.Domain))
	}
	return strings.Join(parts, ", ")
}

// gatewaysFor returns the gateway addresses (with the pod subnet mask) to
// assign to the bridge, one per address family present in the IPAM result. If
// IPAM did not supply a gateway for a family, the first host address of that
// subnet is used.
func gatewaysFor(result *types100.Result) []*net.IPNet {
	seen4, seen6 := false, false
	var gws []*net.IPNet
	for _, ip := range result.IPs {
		isV4 := ip.Address.IP.To4() != nil
		if isV4 && seen4 || !isV4 && seen6 {
			continue
		}
		gw := ip.Gateway
		if gw == nil {
			gw = ip.Address.IP.Mask(ip.Address.Mask)
			gw[len(gw)-1]++
		}
		gws = append(gws, &net.IPNet{IP: gw, Mask: ip.Address.Mask})
		if isV4 {
			seen4 = true
		} else {
			seen6 = true
		}
	}
	return gws
}

func HandleDel(cfg *DelConfig) error {
	lock, err := NewFileLock(lockPathForSocket(cfg.Config.GroutSocketPath))
	if err != nil {
		return fmt.Errorf("creating lock: %w", err)
	}
	if err := lock.Lock(); err != nil {
		lock.Close()
		return fmt.Errorf("acquiring lock: %w", err)
	}
	defer func() {
		_ = lock.Unlock()
		lock.Close()
	}()

	slog.Debug("CNI DEL", "containerID", cfg.Args.ContainerID, "ifName", cfg.Args.IfName)

	// Refless path: recompute the deterministic port name and find it in grout.
	name := groutPortName(cfg.Args.ContainerID, cfg.Args.IfName)
	ifaces, err := cfg.Client.InterfaceList(groutapi.InterfaceListRequest{})
	if err != nil {
		return fmt.Errorf("listing interfaces: %w", err)
	}
	if port, found := findPortByName(ifaces, name); found {
		if err := cfg.Client.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: port.ID}); err != nil {
			slog.Warn("grout interface del", "ifaceID", port.ID, "error", err)
		}
		// The vhost socket path is deterministic; removal is a no-op for TAP
		// ports (whose socket never existed).
		removeVhostSocket(cfg.Config.GroutSocketPath, name)
		// The bridge is the port's domain; GC it once its last member is gone.
		if port.Domain != 0 {
			gcBridgeIfEmpty(cfg.Client, port.Domain)
		}
	} else {
		slog.Debug("no grout port for attachment, cleanup only", "containerID", cfg.Args.ContainerID, "ifName", cfg.Args.IfName)
	}

	ipamDel(cfg.IPAMDel, cfg.Config, cfg.Args)
	return nil
}

// removeVhostSocket best-effort removes the deterministic vhost-user socket for
// a port. It is a no-op for TAP ports, whose socket path never existed.
func removeVhostSocket(groutSocketPath, portName string) {
	path := vhostSocketPath(groutSocketPath, portName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("removing vhost socket", "path", path, "error", err)
	}
}

// gcBridgeIfEmpty deletes the bridge if no interface still references it as its
// domain. Best-effort: failures are logged, not fatal to DEL.
func gcBridgeIfEmpty(client GroutClient, bridgeID uint16) {
	ifaces, err := client.InterfaceList(groutapi.InterfaceListRequest{})
	if err != nil {
		slog.Warn("bridge GC: listing interfaces", "bridgeID", bridgeID, "error", err)
		return
	}
	bridgeFound := false
	for _, iface := range ifaces {
		if iface.ID == bridgeID {
			// Guard against stale refs after grout restarts and reuses ids: only
			// GC if the id still refers to a bridge.
			if iface.Type != groutapi.InterfaceTypeBridge {
				slog.Debug("bridge GC: id is no longer a bridge, skipping", "bridgeID", bridgeID, "type", iface.Type)
				return
			}
			bridgeFound = true
		}
		if iface.Domain == bridgeID {
			return // still has members
		}
	}
	if !bridgeFound {
		return // bridge already gone
	}
	// grout refuses to delete an interface that still holds L3 addresses, so
	// clear the gateway address we assigned to the bridge before deleting it.
	// Best-effort: a flush failure should not stop the delete attempt.
	if err := client.AddressFlush(bridgeID); err != nil {
		slog.Warn("bridge GC: flushing bridge addresses", "bridgeID", bridgeID, "error", err)
	}
	if err := client.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: bridgeID}); err != nil {
		slog.Warn("bridge GC: deleting empty bridge", "bridgeID", bridgeID, "error", err)
		return
	}
	slog.Debug("empty bridge garbage-collected", "bridgeID", bridgeID)
}

// HandleCheck verifies that the attachment the runtime is asking about is still
// intact end to end. Unlike a bare existence probe, it detects the drift the
// CHECK verb exists to catch — e.g. grout restarted and lost its in-memory
// port/address state, or the pod-side interface went down.
//
// The grout port is the source of truth (its name encodes the attachment), so an
// absent port is drift and always an error. For a TAP attachment CHECK then
// enters the pod netns and confirms the pod interface exists, is up, and carries
// the addresses from the runtime-provided prevResult. For a virtio attachment
// there is no kernel netdev in the pod, so CHECK confirms the deterministic
// vhost-user socket is present instead and never touches the pod netns.
func HandleCheck(cfg *DelConfig) error {
	lock, err := NewFileLock(lockPathForSocket(cfg.Config.GroutSocketPath))
	if err != nil {
		return fmt.Errorf("creating lock: %w", err)
	}
	if err := lock.Lock(); err != nil {
		lock.Close()
		return fmt.Errorf("acquiring lock: %w", err)
	}
	defer func() {
		_ = lock.Unlock()
		lock.Close()
	}()

	name := groutPortName(cfg.Args.ContainerID, cfg.Args.IfName)
	slog.Debug("CNI CHECK", "containerID", cfg.Args.ContainerID, "ifName", cfg.Args.IfName, "portName", name)

	ifaces, err := cfg.Client.InterfaceList(groutapi.InterfaceListRequest{})
	if err != nil {
		return fmt.Errorf("listing interfaces: %w", err)
	}
	// A missing grout port for a container the runtime is asking about is drift,
	// not a tolerable absence: grout is the source of truth and it has lost the
	// attachment. Report it so the runtime can re-ADD.
	if _, found := findPortByName(ifaces, name); !found {
		return fmt.Errorf("grout port %q for container %s ifname %s is missing (grout lost state)", name, cfg.Args.ContainerID, cfg.Args.IfName)
	}

	if cfg.Config.InterfaceType == InterfaceTypeVirtio {
		// No kernel netdev in the pod: the DPDK app owns L2/L3 via the vhost-user
		// socket. Verify that socket is still present; do not touch the pod netns.
		sockPath := vhostSocketPath(cfg.Config.GroutSocketPath, name)
		if !fileExists(sockPath) {
			return fmt.Errorf("vhost-user socket %q for grout port %q is missing", sockPath, name)
		}
		slog.Debug("CHECK ok (virtio)", "portName", name, "socket", sockPath)
		return nil
	}

	// TAP path: verify the pod-side interface. Without a netns there is nothing
	// pod-side to inspect (the grout port check above already passed).
	if cfg.Args.Netns == "" {
		slog.Debug("CHECK ok (grout port present, no netns to verify)", "portName", name)
		return nil
	}

	expectedIPs, err := expectedPodIPs(cfg.Config)
	if err != nil {
		return fmt.Errorf("determining expected pod addresses: %w", err)
	}

	verifyFn := cfg.VerifyPodIface
	if verifyFn == nil {
		verifyFn = verifyPodInterface
	}
	if err := verifyFn(cfg.Args.Netns, cfg.Args.IfName, expectedIPs); err != nil {
		return fmt.Errorf("verifying pod interface %q: %w", cfg.Args.IfName, err)
	}
	slog.Debug("CHECK ok (tap)", "portName", name, "ifName", cfg.Args.IfName, "netns", cfg.Args.Netns)
	return nil
}

// expectedPodIPs extracts the pod addresses the runtime recorded in the previous
// ADD result (passed back to CHECK as prevResult per the CNI spec). CHECK has no
// IPAM call of its own, so prevResult is the only source of the expected IPs. A
// config without a prevResult yields no expected addresses (nil), in which case
// CHECK verifies only the interface's presence and up state.
func expectedPodIPs(conf *PluginConf) ([]*net.IPNet, error) {
	if conf.RawPrevResult == nil {
		return nil, nil
	}
	if err := version.ParsePrevResult(&conf.NetConf); err != nil {
		return nil, fmt.Errorf("parsing prevResult: %w", err)
	}
	if conf.PrevResult == nil {
		return nil, nil
	}
	result, err := types100.NewResultFromResult(conf.PrevResult)
	if err != nil {
		return nil, fmt.Errorf("converting prevResult to 1.0.0 format: %w", err)
	}
	var ips []*net.IPNet
	for _, ipc := range result.IPs {
		if ipc == nil {
			continue
		}
		addr := ipc.Address // copy: do not alias the parsed result
		ips = append(ips, &addr)
	}
	return ips, nil
}

// IPAMGCFunc releases IPAM allocations the runtime no longer considers valid.
// Defined as a type to allow test injection.
type IPAMGCFunc func(config *PluginConf, args *skel.CmdArgs) error

// GCConfig carries the inputs for a CNI GC (garbage collect) call.
type GCConfig struct {
	Client GroutClient
	Config *PluginConf
	Args   *skel.CmdArgs
	IPAMGC IPAMGCFunc
}

// HandleGC implements CNI GC refless: it reaps grout ports (and their vhost
// sockets) on this network's bridge that the runtime no longer lists as valid,
// GCs the bridge if it empties, then delegates GC to the IPAM plugin so it can
// free leaked reservations. GC runs per network; scoping the reap to the
// network's bridge (found by name) keeps it from touching other networks'
// ports, and the grout-k port-name prefix guards non-CNI grout interfaces
// (physical ports, VXLAN, other apps). Best-effort: per-item failures are
// logged, not fatal.
func HandleGC(cfg *GCConfig) error {
	lock, err := NewFileLock(lockPathForSocket(cfg.Config.GroutSocketPath))
	if err != nil {
		return fmt.Errorf("creating lock: %w", err)
	}
	if err := lock.Lock(); err != nil {
		lock.Close()
		return fmt.Errorf("acquiring lock: %w", err)
	}
	defer func() {
		_ = lock.Unlock()
		lock.Close()
	}()

	ifaces, err := cfg.Client.InterfaceList(groutapi.InterfaceListRequest{})
	if err != nil {
		return fmt.Errorf("GC: listing interfaces: %w", err)
	}

	bridgeID, ok := findBridgeID(ifaces, cfg.Config.Bridge)
	if !ok {
		// No bridge for this network on the node: nothing of ours to reap.
		gcIPAM(cfg.IPAMGC, cfg.Config, cfg.Args)
		return nil
	}

	valid := make(map[string]bool, len(cfg.Config.ValidAttachments))
	for _, a := range cfg.Config.ValidAttachments {
		valid[groutPortName(a.ContainerID, a.IfName)] = true
	}

	reaped := false
	for _, iface := range ifaces {
		if iface.Domain != bridgeID || !strings.HasPrefix(iface.Name, portPrefix) {
			continue
		}
		if valid[iface.Name] {
			continue
		}
		slog.Debug("GC: reaping stale port", "network", cfg.Config.Name, "port", iface.Name, "ifaceID", iface.ID)
		if err := cfg.Client.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: iface.ID}); err != nil {
			slog.Warn("GC: grout interface del", "ifaceID", iface.ID, "error", err)
			continue
		}
		removeVhostSocket(cfg.Config.GroutSocketPath, iface.Name)
		reaped = true
	}
	if reaped {
		gcBridgeIfEmpty(cfg.Client, bridgeID)
	}

	// Let the IPAM plugin reap its own leaked reservations.
	gcIPAM(cfg.IPAMGC, cfg.Config, cfg.Args)
	return nil
}

func ipamDel(fn IPAMDelFunc, config *PluginConf, args *skel.CmdArgs) {
	if fn == nil {
		fn = delegateIPAMDel
	}
	fn(config, args)
}

func gcIPAM(fn IPAMGCFunc, config *PluginConf, args *skel.CmdArgs) {
	if fn == nil {
		fn = delegateIPAMGC
	}
	if err := fn(config, args); err != nil {
		slog.Warn("GC: IPAM GC", "type", config.IPAM.Type, "error", err)
	}
}

func delegateIPAMGC(config *PluginConf, args *skel.CmdArgs) error {
	if err := invoke.DelegateGC(context.TODO(), config.IPAM.Type, args.StdinData, nil); err != nil {
		return fmt.Errorf("invoking IPAM plugin %q GC: %w", config.IPAM.Type, err)
	}
	return nil
}

func delegateIPAMAdd(config *PluginConf, args *skel.CmdArgs) (types.Result, error) {
	// IPAM plugins receive the full network config (which carries name and
	// cniVersion alongside the ipam section); passing only config.IPAM makes
	// their skel reject the call with "missing network name".
	result, err := invoke.DelegateAdd(context.TODO(), config.IPAM.Type, args.StdinData, nil)
	if err != nil {
		return nil, fmt.Errorf("invoking IPAM plugin %q: %w", config.IPAM.Type, err)
	}
	return result, nil
}

func delegateIPAMDel(config *PluginConf, args *skel.CmdArgs) {
	if err := invoke.DelegateDel(context.TODO(), config.IPAM.Type, args.StdinData, nil); err != nil {
		slog.Warn("IPAM DEL", "type", config.IPAM.Type, "error", err)
	}
}

func cleanupAfterAddFailure(cfg *AddConfig, ifaceID uint16) {
	if err := cfg.Client.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: ifaceID}); err != nil {
		slog.Warn("rollback: grout interface del", "ifaceID", ifaceID, "error", err)
	}
	ipamDel(cfg.IPAMDel, cfg.Config, cfg.Args)
}

func convertResult(result types.Result) (*types100.Result, error) {
	cniResult, err := types100.NewResultFromResult(result)
	if err != nil {
		return nil, fmt.Errorf("converting result to 1.0.0 format: %w", err)
	}
	return cniResult, nil
}
