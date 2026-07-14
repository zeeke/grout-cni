//go:build e2e

package groutapi_test

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/zeeke/grout-cni/pkg/groutapi"
)

const groutImage = "quay.io/grout/grout:0.16.0"

var sharedClient *groutapi.Client

func TestMain(m *testing.M) {
	ctx := context.Background()

	sockDir, err := os.MkdirTemp("", "grout-e2e-*")
	if err != nil {
		log.Fatalf("create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(sockDir) }()

	req := testcontainers.ContainerRequest{
		Image: groutImage,
		Cmd:   []string{"/usr/bin/grout", "-t", "-s", "/run/grout/grout.sock", "-m", "0666"},
		HostConfigModifier: func(hc *container.HostConfig) {
			hc.Privileged = true
			hc.Binds = append(hc.Binds, sockDir+":/run/grout")
		},
	}

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		log.Fatalf("start grout container: %v", err)
	}
	defer func() {
		if err := ctr.Terminate(ctx); err != nil {
			log.Printf("terminate grout container: %v", err)
		}
	}()

	sockPath := filepath.Join(sockDir, "grout.sock")
	if err := waitForSocket(sockPath, 60*time.Second); err != nil {
		logs, _ := ctr.Logs(ctx)
		if logs != nil {
			buf := make([]byte, 4096)
			n, _ := logs.Read(buf)
			log.Printf("grout container logs:\n%s", buf[:n])
		}
		log.Fatalf("grout socket not ready: %v", err)
	}

	sharedClient, err = groutapi.Dial(sockPath)
	if err != nil {
		log.Fatalf("dial grout: %v", err)
	}
	defer func() { _ = sharedClient.Close() }()

	os.Exit(m.Run())
}

func waitForSocket(sockPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", sockPath)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("socket %s not ready after %s", sockPath, timeout)
}

func findInterface(ifaces []groutapi.InterfaceInfo, id uint16) *groutapi.InterfaceInfo {
	for i := range ifaces {
		if ifaces[i].ID == id {
			return &ifaces[i]
		}
	}
	return nil
}

func TestE2E_InterfaceLifecycle(t *testing.T) {
	resp, err := sharedClient.InterfaceAdd(groutapi.InterfaceAddRequest{
		Name: "tap-e2e0",
		Type: groutapi.InterfaceTypeTAP,
	})
	require.NoError(t, err)
	assert.NotZero(t, resp.IfaceID)

	ifaces, err := sharedClient.InterfaceList(groutapi.InterfaceListRequest{})
	require.NoError(t, err)
	found := findInterface(ifaces, resp.IfaceID)
	require.NotNil(t, found, "interface %d should be present after add", resp.IfaceID)
	assert.Equal(t, "tap-e2e0", found.Name)

	err = sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: resp.IfaceID})
	require.NoError(t, err)

	ifaces, err = sharedClient.InterfaceList(groutapi.InterfaceListRequest{})
	require.NoError(t, err)
	assert.Nil(t, findInterface(ifaces, resp.IfaceID), "interface %d should be absent after del", resp.IfaceID)
}

func TestE2E_AddressLifecycle(t *testing.T) {
	resp, err := sharedClient.InterfaceAdd(groutapi.InterfaceAddRequest{
		Name: "tap-e2e1",
		Type: groutapi.InterfaceTypeTAP,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: resp.IfaceID})
	})

	addr := net.IPNet{
		IP:   net.ParseIP("10.200.0.2"),
		Mask: net.CIDRMask(24, 32),
	}

	err = sharedClient.AddressAdd(groutapi.AddressAddRequest{
		IfaceID: resp.IfaceID,
		Address: addr,
	})
	require.NoError(t, err)

	err = sharedClient.AddressDel(groutapi.AddressDelRequest{
		IfaceID: resp.IfaceID,
		Address: addr,
	})
	require.NoError(t, err)
}

func TestE2E_RouteLifecycle(t *testing.T) {
	resp, err := sharedClient.InterfaceAdd(groutapi.InterfaceAddRequest{
		Name: "tap-e2e2",
		Type: groutapi.InterfaceTypeTAP,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: resp.IfaceID})
	})

	ifaceAddr := net.IPNet{
		IP:   net.ParseIP("10.201.0.1"),
		Mask: net.CIDRMask(24, 32),
	}
	require.NoError(t, sharedClient.AddressAdd(groutapi.AddressAddRequest{
		IfaceID: resp.IfaceID,
		Address: ifaceAddr,
	}))
	t.Cleanup(func() {
		_ = sharedClient.AddressDel(groutapi.AddressDelRequest{
			IfaceID:   resp.IfaceID,
			Address:   ifaceAddr,
			MissingOK: true,
		})
	})

	dest := net.IPNet{
		IP:   net.ParseIP("10.202.0.0"),
		Mask: net.CIDRMask(24, 32),
	}
	nextHop := net.ParseIP("10.201.0.254")

	require.NoError(t, sharedClient.RouteAdd(groutapi.RouteAddRequest{
		Destination: dest,
		NextHop:     nextHop,
		IfaceID:     resp.IfaceID,
	}))

	require.NoError(t, sharedClient.RouteDel(groutapi.RouteDelRequest{
		Destination: dest,
	}))
}

// TestE2E_AddressLifecycleIPv6 validates the gr_ip6_addr_add/del wire layout
// (hand-computed offsets in pkg/groutapi/client.go) against a real grout.
func TestE2E_AddressLifecycleIPv6(t *testing.T) {
	resp, err := sharedClient.InterfaceAdd(groutapi.InterfaceAddRequest{
		Name: "tap-e2e6a",
		Type: groutapi.InterfaceTypeTAP,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: resp.IfaceID})
	})

	addr := net.IPNet{
		IP:   net.ParseIP("fd00:10:200::2"),
		Mask: net.CIDRMask(64, 128),
	}

	err = sharedClient.AddressAdd(groutapi.AddressAddRequest{
		IfaceID: resp.IfaceID,
		Address: addr,
	})
	require.NoError(t, err)

	// ExistOK must make a duplicate add a no-op, like the CNI's bridge
	// gateway assignment on the second ADD for the same network.
	err = sharedClient.AddressAdd(groutapi.AddressAddRequest{
		IfaceID: resp.IfaceID,
		Address: addr,
		ExistOK: true,
	})
	require.NoError(t, err)

	err = sharedClient.AddressDel(groutapi.AddressDelRequest{
		IfaceID: resp.IfaceID,
		Address: addr,
	})
	require.NoError(t, err)
}

// TestE2E_RouteLifecycleIPv6 validates the gr_ip6_route_add/del wire layout
// against a real grout.
func TestE2E_RouteLifecycleIPv6(t *testing.T) {
	resp, err := sharedClient.InterfaceAdd(groutapi.InterfaceAddRequest{
		Name: "tap-e2e6r",
		Type: groutapi.InterfaceTypeTAP,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: resp.IfaceID})
	})

	ifaceAddr := net.IPNet{
		IP:   net.ParseIP("fd00:10:201::1"),
		Mask: net.CIDRMask(64, 128),
	}
	require.NoError(t, sharedClient.AddressAdd(groutapi.AddressAddRequest{
		IfaceID: resp.IfaceID,
		Address: ifaceAddr,
	}))
	t.Cleanup(func() {
		_ = sharedClient.AddressDel(groutapi.AddressDelRequest{
			IfaceID:   resp.IfaceID,
			Address:   ifaceAddr,
			MissingOK: true,
		})
	})

	dest := net.IPNet{
		IP:   net.ParseIP("fd00:10:202::"),
		Mask: net.CIDRMask(64, 128),
	}
	nextHop := net.ParseIP("fd00:10:201::fe")

	require.NoError(t, sharedClient.RouteAdd(groutapi.RouteAddRequest{
		Destination: dest,
		NextHop:     nextHop,
		IfaceID:     resp.IfaceID,
	}))

	require.NoError(t, sharedClient.RouteDel(groutapi.RouteDelRequest{
		Destination: dest,
	}))
}

// TestE2E_DualStackBridgeFlush mirrors the CNI's empty-bridge GC for a
// dual-stack network: the bridge holds one gateway address per family, and
// AddressFlush must clear both so the delete succeeds (grout refuses to
// delete an interface that still holds addresses of either family).
func TestE2E_DualStackBridgeFlush(t *testing.T) {
	br, err := sharedClient.InterfaceAdd(groutapi.InterfaceAddRequest{
		Name: "br-e2eds",
		Type: groutapi.InterfaceTypeBridge,
	})
	require.NoError(t, err)
	done := false
	t.Cleanup(func() {
		if !done {
			_ = sharedClient.AddressFlush(br.IfaceID)
			_ = sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: br.IfaceID})
		}
	})

	gw4 := net.IPNet{IP: net.ParseIP("10.251.0.1"), Mask: net.CIDRMask(24, 32)}
	gw6 := net.IPNet{IP: net.ParseIP("fd00:10:251::1"), Mask: net.CIDRMask(64, 128)}
	require.NoError(t, sharedClient.AddressAdd(groutapi.AddressAddRequest{
		IfaceID: br.IfaceID,
		Address: gw4,
		ExistOK: true,
	}))
	require.NoError(t, sharedClient.AddressAdd(groutapi.AddressAddRequest{
		IfaceID: br.IfaceID,
		Address: gw6,
		ExistOK: true,
	}))

	require.NoError(t, sharedClient.AddressFlush(br.IfaceID))
	require.NoError(t, sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: br.IfaceID}))
	done = true

	ifaces, err := sharedClient.InterfaceList(groutapi.InterfaceListRequest{})
	require.NoError(t, err)
	assert.Nil(t, findInterface(ifaces, br.IfaceID), "bridge should be gone after dual-stack flush + delete")
}

func TestE2E_ErrorCases(t *testing.T) {
	t.Run("delete non-existent interface", func(t *testing.T) {
		err := sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: 9999})
		assert.Error(t, err)
	})

	t.Run("delete non-existent address with MissingOK", func(t *testing.T) {
		resp, err := sharedClient.InterfaceAdd(groutapi.InterfaceAddRequest{
			Name: "tap-e2e3",
			Type: groutapi.InterfaceTypeTAP,
		})
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: resp.IfaceID})
		})

		err = sharedClient.AddressDel(groutapi.AddressDelRequest{
			IfaceID: resp.IfaceID,
			Address: net.IPNet{
				IP:   net.ParseIP("10.99.99.99"),
				Mask: net.CIDRMask(24, 32),
			},
			MissingOK: true,
		})
		assert.NoError(t, err)
	})

	t.Run("delete non-existent route with MissingOK", func(t *testing.T) {
		err := sharedClient.RouteDel(groutapi.RouteDelRequest{
			Destination: net.IPNet{
				IP:   net.ParseIP("10.88.88.0"),
				Mask: net.CIDRMask(24, 32),
			},
			MissingOK: true,
		})
		assert.NoError(t, err)
	})

	t.Run("delete non-existent IPv6 address with MissingOK", func(t *testing.T) {
		resp, err := sharedClient.InterfaceAdd(groutapi.InterfaceAddRequest{
			Name: "tap-e2e6e",
			Type: groutapi.InterfaceTypeTAP,
		})
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: resp.IfaceID})
		})

		err = sharedClient.AddressDel(groutapi.AddressDelRequest{
			IfaceID: resp.IfaceID,
			Address: net.IPNet{
				IP:   net.ParseIP("fd00:99:99::99"),
				Mask: net.CIDRMask(64, 128),
			},
			MissingOK: true,
		})
		assert.NoError(t, err)
	})

	t.Run("delete non-existent IPv6 route with MissingOK", func(t *testing.T) {
		err := sharedClient.RouteDel(groutapi.RouteDelRequest{
			Destination: net.IPNet{
				IP:   net.ParseIP("fd00:88:88::"),
				Mask: net.CIDRMask(64, 128),
			},
			MissingOK: true,
		})
		assert.NoError(t, err)
	})
}

// TestE2E_BridgeGCFlushesAddress reproduces the empty-bridge GC bug: grout will
// not reclaim a bridge that still holds the gateway address the CNI assigned to
// it. The fix (mirrored by gcBridgeIfEmpty) is to flush the interface's
// addresses before deleting it. The direct-delete probe documents grout's
// actual behavior in the CI logs.
func TestE2E_BridgeGCFlushesAddress(t *testing.T) {
	br, err := sharedClient.InterfaceAdd(groutapi.InterfaceAddRequest{
		Name: "br-e2egc",
		Type: groutapi.InterfaceTypeBridge,
	})
	require.NoError(t, err)
	done := false
	t.Cleanup(func() {
		if !done {
			_ = sharedClient.AddressFlush(br.IfaceID)
			_ = sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: br.IfaceID})
		}
	})

	gw := net.IPNet{IP: net.ParseIP("10.250.0.1"), Mask: net.CIDRMask(24, 32)}
	require.NoError(t, sharedClient.AddressAdd(groutapi.AddressAddRequest{
		IfaceID: br.IfaceID,
		Address: gw,
		ExistOK: true,
	}))

	// Probe: try to delete the bridge while it still holds the address.
	directErr := sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: br.IfaceID})
	ifaces, err := sharedClient.InterfaceList(groutapi.InterfaceListRequest{})
	require.NoError(t, err)
	stillPresent := findInterface(ifaces, br.IfaceID) != nil
	t.Logf("PROBE direct InterfaceDel(bridge holding an address): err=%v, bridge still present=%v", directErr, stillPresent)

	if !stillPresent {
		done = true
		t.Skip("grout reclaimed the bridge despite the address; flush-before-delete is belt-and-suspenders here")
	}

	// Flush the addresses, then delete — the bridge must now be reclaimed.
	require.NoError(t, sharedClient.AddressFlush(br.IfaceID))
	require.NoError(t, sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: br.IfaceID}))
	done = true

	ifaces, err = sharedClient.InterfaceList(groutapi.InterfaceListRequest{})
	require.NoError(t, err)
	assert.Nil(t, findInterface(ifaces, br.IfaceID), "bridge should be gone after flush + delete")
}

func TestE2E_FullCRUDSequence(t *testing.T) {
	iface, err := sharedClient.InterfaceAdd(groutapi.InterfaceAddRequest{
		Name: "tap-e2e4",
		Type: groutapi.InterfaceTypeTAP,
	})
	require.NoError(t, err)

	addr := net.IPNet{
		IP:   net.ParseIP("10.203.0.1"),
		Mask: net.CIDRMask(24, 32),
	}
	require.NoError(t, sharedClient.AddressAdd(groutapi.AddressAddRequest{
		IfaceID: iface.IfaceID,
		Address: addr,
	}))

	dest := net.IPNet{
		IP:   net.ParseIP("10.204.0.0"),
		Mask: net.CIDRMask(24, 32),
	}
	require.NoError(t, sharedClient.RouteAdd(groutapi.RouteAddRequest{
		Destination: dest,
		NextHop:     net.ParseIP("10.203.0.254"),
		IfaceID:     iface.IfaceID,
	}))

	ifaces, err := sharedClient.InterfaceList(groutapi.InterfaceListRequest{})
	require.NoError(t, err)
	assert.NotNil(t, findInterface(ifaces, iface.IfaceID))

	require.NoError(t, sharedClient.RouteDel(groutapi.RouteDelRequest{Destination: dest}))
	require.NoError(t, sharedClient.AddressDel(groutapi.AddressDelRequest{IfaceID: iface.IfaceID, Address: addr}))
	require.NoError(t, sharedClient.InterfaceDel(groutapi.InterfaceDelRequest{IfaceID: iface.IfaceID}))

	ifaces, err = sharedClient.InterfaceList(groutapi.InterfaceListRequest{})
	require.NoError(t, err)
	assert.Nil(t, findInterface(ifaces, iface.IfaceID))
}
