package cni

import (
	"fmt"
	"net"
	"os"

	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

// moveLinkToNamespace moves a network link (by name) into the network
// namespace identified by the given path (e.g. /proc/<pid>/ns/net).
func moveLinkToNamespace(linkName, netnsPath string) error {
	link, err := netlink.LinkByName(linkName)
	if err != nil {
		return fmt.Errorf("finding link %q: %w", linkName, err)
	}

	nsHandle, err := ns.GetNS(netnsPath)
	if err != nil {
		return fmt.Errorf("opening netns %s: %w", netnsPath, err)
	}
	defer func() { _ = nsHandle.Close() }()

	if err := netlink.LinkSetNsFd(link, int(nsHandle.Fd())); err != nil {
		return fmt.Errorf("moving link %q to netns %s: %w", linkName, netnsPath, err)
	}
	return nil
}

// configurePodInterface configures a TAP link that has already been moved into
// the pod network namespace: it renames the link from hostIfName to the CNI
// requested podIfName, then assigns the addresses and routes carried by result
// and brings the link up. result.Interfaces[0] is expected to describe the pod
// interface; its MAC is filled in from the configured link. All netlink
// operations run inside the pod netns.
func configurePodInterface(netnsPath, hostIfName, podIfName string, mtu int, result *types100.Result) error {
	nsHandle, err := ns.GetNS(netnsPath)
	if err != nil {
		return fmt.Errorf("opening netns %s: %w", netnsPath, err)
	}
	defer func() { _ = nsHandle.Close() }()

	return nsHandle.Do(func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(hostIfName)
		if err != nil {
			return fmt.Errorf("finding moved link %q in pod netns: %w", hostIfName, err)
		}

		if hostIfName != podIfName {
			if err := netlink.LinkSetDown(link); err != nil {
				return fmt.Errorf("setting %q down for rename: %w", hostIfName, err)
			}
			if err := netlink.LinkSetName(link, podIfName); err != nil {
				return fmt.Errorf("renaming %q to %q: %w", hostIfName, podIfName, err)
			}
			if link, err = netlink.LinkByName(podIfName); err != nil {
				return fmt.Errorf("re-finding renamed link %q: %w", podIfName, err)
			}
		}

		if mtu > 0 {
			if err := netlink.LinkSetMTU(link, mtu); err != nil {
				return fmt.Errorf("setting MTU %d on %q: %w", mtu, podIfName, err)
			}
		}

		// The pod-side TAP must have a different MAC than grout's port; otherwise
		// the kernel treats grout-originated frames as locally looped and drops
		// them, breaking L2 forwarding. Assign a deterministic local MAC derived
		// from the pod IP (the link is down here, before ConfigureIface brings it
		// up). See grout smoke/bridge_test.sh.
		if mac := podMACFromResult(result); mac != nil {
			if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
				return fmt.Errorf("setting MAC on %q: %w", podIfName, err)
			}
		}

		// ipam.ConfigureIface brings the link up and installs the addresses and
		// routes from result, matching them to the pod interface by name/index.
		if err := ipam.ConfigureIface(podIfName, result); err != nil {
			return fmt.Errorf("configuring pod interface %q: %w", podIfName, err)
		}

		if l, err := netlink.LinkByName(podIfName); err == nil && len(result.Interfaces) > 0 {
			result.Interfaces[0].Mac = l.Attrs().HardwareAddr.String()
		}
		return nil
	})
}

// podMACFromResult derives a deterministic, locally-administered unicast MAC
// from the first IP address in result. IPv4 uses 02:00:<a>.<b>.<c>.<d>; IPv6
// uses 02:00 plus the last 4 bytes of the address. Returns nil if there is no
// usable address.
func podMACFromResult(result *types100.Result) net.HardwareAddr {
	if result == nil {
		return nil
	}
	for _, ipc := range result.IPs {
		if ipc == nil {
			continue
		}
		if ip4 := ipc.Address.IP.To4(); ip4 != nil {
			return net.HardwareAddr{0x02, 0x00, ip4[0], ip4[1], ip4[2], ip4[3]}
		}
	}
	for _, ipc := range result.IPs {
		if ipc == nil {
			continue
		}
		if ip6 := ipc.Address.IP.To16(); ip6 != nil {
			return net.HardwareAddr{0x02, 0x00, ip6[12], ip6[13], ip6[14], ip6[15]}
		}
	}
	return nil
}

// verifyPodInterface enters the pod network namespace and confirms the pod
// interface named podIfName exists, is administratively up, and carries every
// address in expectedIPs. It is the production VerifyPodIfaceFunc used by CHECK.
// A descriptive error is returned on the first check that fails. When
// expectedIPs is empty only presence and up state are verified. All netlink
// operations run inside the pod netns, mirroring configurePodInterface.
func verifyPodInterface(netnsPath, podIfName string, expectedIPs []*net.IPNet) error {
	nsHandle, err := ns.GetNS(netnsPath)
	if err != nil {
		return fmt.Errorf("opening netns %s: %w", netnsPath, err)
	}
	defer func() { _ = nsHandle.Close() }()

	return nsHandle.Do(func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(podIfName)
		if err != nil {
			return fmt.Errorf("pod interface %q not found in netns %s: %w", podIfName, netnsPath, err)
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			return fmt.Errorf("pod interface %q is not administratively up", podIfName)
		}
		if len(expectedIPs) == 0 {
			return nil
		}
		addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return fmt.Errorf("listing addresses on %q: %w", podIfName, err)
		}
		for _, want := range expectedIPs {
			if !addrPresent(addrs, want.IP) {
				return fmt.Errorf("pod interface %q missing expected address %s", podIfName, want.String())
			}
		}
		return nil
	})
}

// addrPresent reports whether ip is configured on the interface, matching by
// address (the mask may differ between the CNI result and the kernel).
func addrPresent(addrs []netlink.Addr, ip net.IP) bool {
	for _, a := range addrs {
		if a.IPNet != nil && a.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// linkExists checks whether a network link with the given name exists.
func linkExists(linkName string) bool {
	_, err := netlink.LinkByName(linkName)
	return err == nil
}

// lockPathForSocket returns the CNI serialization lock path, co-located with
// grout's control socket (<socket>.lock) so the lock is scoped to the grout
// instance the concurrent CNI calls actually contend over. See
// docs/design/refless-cni-state.md.
func lockPathForSocket(socketPath string) string {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	return socketPath + ".lock"
}

// fileExists checks if a file exists at the given path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
