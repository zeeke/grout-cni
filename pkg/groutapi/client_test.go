package groutapi

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockServer struct {
	listener net.Listener
	handler  func(net.Conn)
}

func newMockServer(t *testing.T) (*mockServer, string) {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "grout.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &mockServer{listener: l}
	return s, sockPath
}

func (s *mockServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handler(conn)
	}
}

func (s *mockServer) close() {
	s.listener.Close()
}

func makeMockIfaceEntry(id uint16, ifType InterfaceType, name string) []byte {
	entry := make([]byte, ifaceSize)
	byteOrder.PutUint16(entry[offIfaceID:], id)
	entry[offIfaceType] = uint8(ifType)
	copy(entry[offIfaceName:offIfaceName+ifNameSize], name)
	return entry
}

var mockInterfaces = [][]byte{
	makeMockIfaceEntry(1, InterfaceTypePort, "tap0"),
	makeMockIfaceEntry(2, InterfaceTypePort, "tap1"),
}

func makeMockFloodEntry(vni uint32, addr net.IP) []byte {
	entry := make([]byte, floodEntrySize)
	_ = encodeFloodEntry(entry, grVRFDefaultID, vni, addr)
	return entry
}

var mockFloodVTEPs = [][]byte{
	makeMockFloodEntry(100, net.ParseIP("10.0.0.1")),
	makeMockFloodEntry(100, net.ParseIP("10.0.0.2")),
}

func handleSuccessfulRequests(conn net.Conn) {
	defer conn.Close()
	for {
		var hdr requestHeader
		if err := binary.Read(conn, byteOrder, &hdr); err != nil {
			if err == io.EOF {
				return
			}
			return
		}

		if hdr.PayloadLen > 0 {
			payload := make([]byte, hdr.PayloadLen)
			if _, err := io.ReadFull(conn, payload); err != nil {
				return
			}
		}

		if hdr.Type == typeIfaceList {
			for _, entry := range mockInterfaces {
				entryResp := responseHeader{
					ForID:      hdr.ID,
					Status:     0,
					PayloadLen: uint32(len(entry)),
				}
				if err := binary.Write(conn, byteOrder, entryResp); err != nil {
					return
				}
				if _, err := conn.Write(entry); err != nil {
					return
				}
			}
			endResp := responseHeader{ForID: hdr.ID, Status: 0, PayloadLen: 0}
			if err := binary.Write(conn, byteOrder, endResp); err != nil {
				return
			}
			continue
		}

		if hdr.Type == typeFloodList {
			for _, entry := range mockFloodVTEPs {
				entryResp := responseHeader{
					ForID:      hdr.ID,
					Status:     0,
					PayloadLen: uint32(len(entry)),
				}
				if err := binary.Write(conn, byteOrder, entryResp); err != nil {
					return
				}
				if _, err := conn.Write(entry); err != nil {
					return
				}
			}
			endResp := responseHeader{ForID: hdr.ID, Status: 0, PayloadLen: 0}
			if err := binary.Write(conn, byteOrder, endResp); err != nil {
				return
			}
			continue
		}

		var respPayload []byte
		if hdr.Type == typeIfaceAdd {
			respPayload = make([]byte, 2)
			byteOrder.PutUint16(respPayload, 42)
		}

		resp := responseHeader{
			ForID:      hdr.ID,
			Status:     0,
			PayloadLen: uint32(len(respPayload)),
		}
		if err := binary.Write(conn, byteOrder, resp); err != nil {
			return
		}
		if len(respPayload) > 0 {
			if _, err := conn.Write(respPayload); err != nil {
				return
			}
		}
	}
}

func setupClient(t *testing.T) *Client {
	t.Helper()
	srv, sockPath := newMockServer(t)
	srv.handler = handleSuccessfulRequests
	go srv.serve()
	t.Cleanup(srv.close)

	client, err := Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestDial(t *testing.T) {
	srv, sockPath := newMockServer(t)
	srv.handler = handleSuccessfulRequests
	go srv.serve()
	defer srv.close()

	client, err := Dial(sockPath)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	client.Close()
}

func TestDialFailsOnBadPath(t *testing.T) {
	_, err := Dial(filepath.Join(t.TempDir(), "nonexistent.sock"))
	if err == nil {
		t.Fatal("expected error for nonexistent socket")
	}
}

func TestDialFailsOnBadHandshake(t *testing.T) {
	srv, sockPath := newMockServer(t)
	srv.handler = func(conn net.Conn) {
		defer conn.Close()
		var hdr requestHeader
		_ = binary.Read(conn, byteOrder, &hdr)
		if hdr.PayloadLen > 0 {
			payload := make([]byte, hdr.PayloadLen)
			_, _ = io.ReadFull(conn, payload)
		}
		resp := responseHeader{ForID: hdr.ID, Status: 1} // EPERM
		_ = binary.Write(conn, byteOrder, resp)
	}
	go srv.serve()
	defer srv.close()

	_, err := Dial(sockPath)
	if err == nil {
		t.Fatal("expected error for failed handshake")
	}
}

func TestInterfaceAdd(t *testing.T) {
	client := setupClient(t)

	resp, err := client.InterfaceAdd(InterfaceAddRequest{
		Name: "tap0",
		Type: InterfaceTypeTAP,
	})
	if err != nil {
		t.Fatalf("InterfaceAdd: %v", err)
	}
	if resp.IfaceID != 42 {
		t.Errorf("expected iface ID 42, got %d", resp.IfaceID)
	}
}

func TestInterfaceAddPayload(t *testing.T) {
	srv, sockPath := newMockServer(t)
	captured := make(chan []byte, 4)
	srv.handler = func(conn net.Conn) {
		defer conn.Close()
		for {
			var hdr requestHeader
			if err := binary.Read(conn, byteOrder, &hdr); err != nil {
				return
			}
			var payload []byte
			if hdr.PayloadLen > 0 {
				payload = make([]byte, hdr.PayloadLen)
				if _, err := io.ReadFull(conn, payload); err != nil {
					return
				}
			}
			if hdr.Type == typeIfaceAdd {
				captured <- payload
				_ = binary.Write(conn, byteOrder, responseHeader{ForID: hdr.ID, PayloadLen: 2})
				idbuf := make([]byte, 2)
				byteOrder.PutUint16(idbuf, 99)
				_, _ = conn.Write(idbuf)
			} else { // hello and anything else
				_ = binary.Write(conn, byteOrder, responseHeader{ForID: hdr.ID})
			}
		}
	}
	go srv.serve()
	t.Cleanup(srv.close)

	client, err := Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	// A bridge is the gr_iface base only (no port info section).
	if _, err := client.InterfaceAdd(InterfaceAddRequest{Name: "br0", Type: InterfaceTypeBridge}); err != nil {
		t.Fatalf("InterfaceAdd bridge: %v", err)
	}
	bridge := <-captured
	assert.Len(t, bridge, ifaceSize+bridgeInfoSize, "bridge add payload should include the bridge info section")
	assert.Equal(t, uint8(InterfaceTypeBridge), bridge[offIfaceType])
	// A valid locally-administered unicast MAC (bit0 clear, bit1 set).
	mac := bridge[offIfaceInfo+offBridgeInfoMAC : offIfaceInfo+offBridgeInfoMAC+6]
	assert.Equal(t, byte(0x02), mac[0]&0x03, "bridge MAC must be locally-administered unicast")
	assert.NotEqual(t, make([]byte, 6), mac, "bridge MAC must be set explicitly")

	// A member TAP is base + port info, with domain_id pointing at the bridge.
	if _, err := client.InterfaceAdd(InterfaceAddRequest{Name: "tapx", Type: InterfaceTypeTAP, DomainID: 7}); err != nil {
		t.Fatalf("InterfaceAdd member: %v", err)
	}
	member := <-captured
	assert.Len(t, member, ifaceSize+portInfoSize, "port add payload should include port info")
	assert.Equal(t, uint8(InterfaceTypePort), member[offIfaceType])
	assert.Equal(t, uint16(7), byteOrder.Uint16(member[offIfaceDomain:]), "domain_id must reference the bridge")
	assert.Contains(t, string(member[offPortDevargs:]), "iface=tapx", "devargs must carry the iface name")
	assert.Contains(t, string(member[offPortDevargs:]), "net_tap_", "a TAP port uses net_tap devargs")

	// A virtio port uses net_vhost devargs pointing at the vhost-user socket.
	if _, err := client.InterfaceAdd(InterfaceAddRequest{Name: "vh0", Type: InterfaceTypeTAP, VhostUserPath: "/run/grout/vhost/vh0.sock"}); err != nil {
		t.Fatalf("InterfaceAdd vhost: %v", err)
	}
	vhost := <-captured
	devargs := string(vhost[offPortDevargs:])
	assert.Contains(t, devargs, "net_vhost_", "a virtio port uses net_vhost devargs")
	assert.Contains(t, devargs, "iface=/run/grout/vhost/vh0.sock", "vhost devargs must carry the socket path")
}

func TestInterfaceAddVXLANPayload(t *testing.T) {
	srv, sockPath := newMockServer(t)
	captured := make(chan []byte, 1)
	srv.handler = func(conn net.Conn) {
		defer conn.Close()
		for {
			var hdr requestHeader
			if err := binary.Read(conn, byteOrder, &hdr); err != nil {
				return
			}
			var payload []byte
			if hdr.PayloadLen > 0 {
				payload = make([]byte, hdr.PayloadLen)
				if _, err := io.ReadFull(conn, payload); err != nil {
					return
				}
			}
			if hdr.Type == typeIfaceAdd {
				captured <- payload
				_ = binary.Write(conn, byteOrder, responseHeader{ForID: hdr.ID, PayloadLen: 2})
				idbuf := make([]byte, 2)
				byteOrder.PutUint16(idbuf, 7)
				_, _ = conn.Write(idbuf)
			} else { // hello and anything else
				_ = binary.Write(conn, byteOrder, responseHeader{ForID: hdr.ID})
			}
		}
	}
	go srv.serve()
	t.Cleanup(srv.close)

	client, err := Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	resp, err := client.InterfaceAdd(InterfaceAddRequest{
		Name: "vxlan100",
		Type: InterfaceTypeVXLAN,
		VXLAN: &VXLANConfig{
			VNI:        100,
			EncapVRFID: 1,
			DstPort:    4789,
			LocalAddr:  net.ParseIP("192.0.2.1"),
		},
	})
	if err != nil {
		t.Fatalf("InterfaceAdd VXLAN: %v", err)
	}
	if resp.IfaceID != 7 {
		t.Errorf("expected iface ID 7, got %d", resp.IfaceID)
	}

	payload := <-captured
	assert.Len(t, payload, ifaceSize+vxlanInfoSize, "VXLAN add payload should include the vxlan info section")
	assert.Equal(t, uint8(InterfaceTypeVXLAN), payload[offIfaceType])

	info := payload[offIfaceInfo:]
	assert.Equal(t, uint32(100), byteOrder.Uint32(info[offVxlanVNI:]))
	assert.Equal(t, uint16(1), byteOrder.Uint16(info[offVxlanEncapVRF:]))
	assert.Equal(t, uint16(4789), byteOrder.Uint16(info[offVxlanDstPort:]))
	local := info[offVxlanLocal : offVxlanLocal+l3AddrSize]
	assert.Equal(t, uint8(afIP4), local[offL3AddrAF])
	assert.Equal(t, net.ParseIP("192.0.2.1").To4(), net.IP(local[offL3AddrIPv4:offL3AddrIPv4+4]))
}

func TestInterfaceAddVXLANRequiresConfig(t *testing.T) {
	client := setupClient(t)

	_, err := client.InterfaceAdd(InterfaceAddRequest{Name: "vxlan100", Type: InterfaceTypeVXLAN})
	if err == nil {
		t.Fatal("expected error when VXLAN config is missing")
	}
}

func TestInterfaceList(t *testing.T) {
	client := setupClient(t)

	ifaces, err := client.InterfaceList(InterfaceListRequest{})
	if err != nil {
		t.Fatalf("InterfaceList: %v", err)
	}
	if len(ifaces) != 2 {
		t.Fatalf("expected 2 interfaces, got %d", len(ifaces))
	}
	if ifaces[0].ID != 1 || ifaces[0].Name != "tap0" {
		t.Errorf("iface[0]: got ID=%d Name=%q, want ID=1 Name=tap0", ifaces[0].ID, ifaces[0].Name)
	}
	if ifaces[1].ID != 2 || ifaces[1].Name != "tap1" {
		t.Errorf("iface[1]: got ID=%d Name=%q, want ID=2 Name=tap1", ifaces[1].ID, ifaces[1].Name)
	}
}

func TestInterfaceDel(t *testing.T) {
	client := setupClient(t)

	err := client.InterfaceDel(InterfaceDelRequest{IfaceID: 42})
	if err != nil {
		t.Fatalf("InterfaceDel: %v", err)
	}
}

func TestAddressAdd(t *testing.T) {
	client := setupClient(t)

	err := client.AddressAdd(AddressAddRequest{
		IfaceID: 1,
		Address: net.IPNet{
			IP:   net.ParseIP("10.0.0.2"),
			Mask: net.CIDRMask(24, 32),
		},
		ExistOK: true,
	})
	if err != nil {
		t.Fatalf("AddressAdd: %v", err)
	}
}

func TestAddressAddIPv6(t *testing.T) {
	client := setupClient(t)

	err := client.AddressAdd(AddressAddRequest{
		IfaceID: 1,
		Address: net.IPNet{
			IP:   net.ParseIP("fd00::2"),
			Mask: net.CIDRMask(64, 128),
		},
		ExistOK: true,
	})
	if err != nil {
		t.Fatalf("AddressAdd IPv6: %v", err)
	}
}

func TestAddressDel(t *testing.T) {
	client := setupClient(t)

	err := client.AddressDel(AddressDelRequest{
		IfaceID: 1,
		Address: net.IPNet{
			IP:   net.ParseIP("10.0.0.2"),
			Mask: net.CIDRMask(24, 32),
		},
		MissingOK: true,
	})
	if err != nil {
		t.Fatalf("AddressDel: %v", err)
	}
}

func TestRouteAdd(t *testing.T) {
	client := setupClient(t)

	err := client.RouteAdd(RouteAddRequest{
		Destination: net.IPNet{
			IP:   net.ParseIP("10.0.0.0"),
			Mask: net.CIDRMask(24, 32),
		},
		NextHop: net.ParseIP("10.0.0.1"),
		IfaceID: 1,
		ExistOK: true,
	})
	if err != nil {
		t.Fatalf("RouteAdd: %v", err)
	}
}

func TestRouteAddIPv6(t *testing.T) {
	client := setupClient(t)

	err := client.RouteAdd(RouteAddRequest{
		Destination: net.IPNet{
			IP:   net.ParseIP("fd00::"),
			Mask: net.CIDRMask(64, 128),
		},
		NextHop: net.ParseIP("fd00::1"),
		ExistOK: true,
	})
	if err != nil {
		t.Fatalf("RouteAdd IPv6: %v", err)
	}
}

func TestRouteDel(t *testing.T) {
	client := setupClient(t)

	err := client.RouteDel(RouteDelRequest{
		Destination: net.IPNet{
			IP:   net.ParseIP("10.0.0.0"),
			Mask: net.CIDRMask(24, 32),
		},
		MissingOK: true,
	})
	if err != nil {
		t.Fatalf("RouteDel: %v", err)
	}
}

func TestFloodAdd(t *testing.T) {
	client := setupClient(t)

	err := client.FloodAdd(FloodAddRequest{
		VNI:     100,
		Addr:    net.ParseIP("10.0.0.1"),
		ExistOK: true,
	})
	if err != nil {
		t.Fatalf("FloodAdd: %v", err)
	}
}

func TestAddressAddIPv6Payload(t *testing.T) {
	srv, sockPath := newMockServer(t)
	captured := make(chan []byte, 1)
	srv.handler = func(conn net.Conn) {
		defer conn.Close()
		for {
			var hdr requestHeader
			if err := binary.Read(conn, byteOrder, &hdr); err != nil {
				return
			}
			var payload []byte
			if hdr.PayloadLen > 0 {
				payload = make([]byte, hdr.PayloadLen)
				if _, err := io.ReadFull(conn, payload); err != nil {
					return
				}
			}
			if hdr.Type == typeIP6AddrAdd {
				captured <- payload
			}
			_ = binary.Write(conn, byteOrder, responseHeader{ForID: hdr.ID})
		}
	}
	go srv.serve()
	t.Cleanup(srv.close)

	client, err := Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	if err := client.AddressAdd(AddressAddRequest{
		IfaceID: 5,
		Address: net.IPNet{
			IP:   net.ParseIP("fd00::2"),
			Mask: net.CIDRMask(64, 128),
		},
		ExistOK: true,
	}); err != nil {
		t.Fatalf("AddressAdd IPv6: %v", err)
	}

	payload := <-captured
	require.Len(t, payload, ip6AddrReqSize)
	assert.Equal(t, uint16(5), byteOrder.Uint16(payload[offIP6IfaceID:]))
	assert.Equal(t, net.ParseIP("fd00::2").To16(), net.IP(payload[offIP6Addr:offIP6Addr+16]))
	assert.Equal(t, uint8(64), payload[offIP6Prefix])
	assert.Equal(t, byte(1), payload[offIP6Flag], "exist_ok must be set")
}

func TestRouteAddIPv6Payload(t *testing.T) {
	srv, sockPath := newMockServer(t)
	captured := make(chan []byte, 1)
	srv.handler = func(conn net.Conn) {
		defer conn.Close()
		for {
			var hdr requestHeader
			if err := binary.Read(conn, byteOrder, &hdr); err != nil {
				return
			}
			var payload []byte
			if hdr.PayloadLen > 0 {
				payload = make([]byte, hdr.PayloadLen)
				if _, err := io.ReadFull(conn, payload); err != nil {
					return
				}
			}
			if hdr.Type == typeIP6RouteAdd {
				captured <- payload
			}
			_ = binary.Write(conn, byteOrder, responseHeader{ForID: hdr.ID})
		}
	}
	go srv.serve()
	t.Cleanup(srv.close)

	client, err := Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	if err := client.RouteAdd(RouteAddRequest{
		VRFID: 2,
		Destination: net.IPNet{
			IP:   net.ParseIP("fd00::"),
			Mask: net.CIDRMask(64, 128),
		},
		NextHop: net.ParseIP("fd00::1"),
		ExistOK: true,
	}); err != nil {
		t.Fatalf("RouteAdd IPv6: %v", err)
	}

	payload := <-captured
	require.Len(t, payload, ip6RouteAddReqSize)
	assert.Equal(t, uint16(2), byteOrder.Uint16(payload[offIP6RteVRF:]))
	assert.Equal(t, net.ParseIP("fd00::").To16(), net.IP(payload[offIP6RteDest:offIP6RteDest+16]))
	assert.Equal(t, uint8(64), payload[offIP6RteDestPfx])
	assert.Equal(t, net.ParseIP("fd00::1").To16(), net.IP(payload[offIP6RteNH:offIP6RteNH+16]))
	assert.Equal(t, byte(4), payload[offIP6RteOrigin], "origin must be GR_NH_ORIGIN_STATIC")
	assert.Equal(t, byte(1), payload[offIP6RteExistOK], "exist_ok must be set")
}

func TestRouteDelIPv6(t *testing.T) {
	client := setupClient(t)

	err := client.RouteDel(RouteDelRequest{
		Destination: net.IPNet{
			IP:   net.ParseIP("fd00::"),
			Mask: net.CIDRMask(64, 128),
		},
		MissingOK: true,
	})
	if err != nil {
		t.Fatalf("RouteDel IPv6: %v", err)
	}
}

func TestAddressDelIPv6(t *testing.T) {
	client := setupClient(t)

	err := client.AddressDel(AddressDelRequest{
		IfaceID: 1,
		Address: net.IPNet{
			IP:   net.ParseIP("fd00::2"),
			Mask: net.CIDRMask(64, 128),
		},
		MissingOK: true,
	})
	if err != nil {
		t.Fatalf("AddressDel IPv6: %v", err)
	}
}

func TestFloodAddRejectsIPv6(t *testing.T) {
	client := setupClient(t)

	err := client.FloodAdd(FloodAddRequest{
		VNI:  100,
		Addr: net.ParseIP("::1"),
	})
	if err == nil {
		t.Fatal("expected error for IPv6 VTEP address")
	}
}

func TestFloodDel(t *testing.T) {
	client := setupClient(t)

	err := client.FloodDel(FloodDelRequest{
		VNI:       100,
		Addr:      net.ParseIP("10.0.0.1"),
		MissingOK: true,
	})
	if err != nil {
		t.Fatalf("FloodDel: %v", err)
	}
}

func TestFloodAddPayload(t *testing.T) {
	srv, sockPath := newMockServer(t)
	captured := make(chan []byte, 1)
	srv.handler = func(conn net.Conn) {
		defer conn.Close()
		for {
			var hdr requestHeader
			if err := binary.Read(conn, byteOrder, &hdr); err != nil {
				return
			}
			var payload []byte
			if hdr.PayloadLen > 0 {
				payload = make([]byte, hdr.PayloadLen)
				if _, err := io.ReadFull(conn, payload); err != nil {
					return
				}
			}
			if hdr.Type == typeFloodAdd {
				captured <- payload
			}
			_ = binary.Write(conn, byteOrder, responseHeader{ForID: hdr.ID})
		}
	}
	go srv.serve()
	t.Cleanup(srv.close)

	client, err := Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	if err := client.FloodAdd(FloodAddRequest{
		VRFID:   3,
		VNI:     200,
		Addr:    net.ParseIP("198.51.100.5"),
		ExistOK: true,
	}); err != nil {
		t.Fatalf("FloodAdd: %v", err)
	}

	payload := <-captured
	assert.Len(t, payload, floodReqSize)
	assert.Equal(t, uint8(floodTypeVTEP), payload[offFloodType])
	assert.Equal(t, uint16(3), byteOrder.Uint16(payload[offFloodVRFID:]))
	assert.Equal(t, uint32(200), byteOrder.Uint32(payload[offFloodVTEPVNI:]))
	addr := payload[offFloodVTEPAddr : offFloodVTEPAddr+l3AddrSize]
	assert.Equal(t, uint8(afIP4), addr[offL3AddrAF])
	assert.Equal(t, net.ParseIP("198.51.100.5").To4(), net.IP(addr[offL3AddrIPv4:offL3AddrIPv4+4]))
	assert.Equal(t, byte(1), payload[floodEntrySize], "exist_ok flag must be set")
}

func TestAddressFlush(t *testing.T) {
	client := setupClient(t)
	if err := client.AddressFlush(42); err != nil {
		t.Fatalf("AddressFlush: %v", err)
	}
}

func TestAddressFlushPayload(t *testing.T) {
	srv, sockPath := newMockServer(t)
	captured := make(chan []byte, 2)
	srv.handler = func(conn net.Conn) {
		defer conn.Close()
		for {
			var hdr requestHeader
			if err := binary.Read(conn, byteOrder, &hdr); err != nil {
				return
			}
			var payload []byte
			if hdr.PayloadLen > 0 {
				payload = make([]byte, hdr.PayloadLen)
				if _, err := io.ReadFull(conn, payload); err != nil {
					return
				}
			}
			if hdr.Type == typeIP4AddrFlush || hdr.Type == typeIP6AddrFlush {
				captured <- payload
			}
			_ = binary.Write(conn, byteOrder, responseHeader{ForID: hdr.ID})
		}
	}
	go srv.serve()
	t.Cleanup(srv.close)

	client, err := Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	if err := client.AddressFlush(7); err != nil {
		t.Fatalf("AddressFlush: %v", err)
	}

	ip4Payload := <-captured
	require.Len(t, ip4Payload, 2, "IPv4 flush payload is a single uint16 iface_id")
	assert.Equal(t, uint16(7), byteOrder.Uint16(ip4Payload[0:2]))

	ip6Payload := <-captured
	require.Len(t, ip6Payload, 2, "IPv6 flush payload is a single uint16 iface_id")
	assert.Equal(t, uint16(7), byteOrder.Uint16(ip6Payload[0:2]))
}

func TestFloodList(t *testing.T) {
	client := setupClient(t)

	vteps, err := client.FloodList(FloodListRequest{})
	if err != nil {
		t.Fatalf("FloodList: %v", err)
	}
	if len(vteps) != 2 {
		t.Fatalf("expected 2 VTEPs, got %d", len(vteps))
	}
	assert.Equal(t, uint32(100), vteps[0].VNI)
	assert.Equal(t, net.ParseIP("10.0.0.1").To4(), vteps[0].Addr)
	assert.Equal(t, uint32(100), vteps[1].VNI)
	assert.Equal(t, net.ParseIP("10.0.0.2").To4(), vteps[1].Addr)
}

func TestServerError(t *testing.T) {
	srv, sockPath := newMockServer(t)
	srv.handler = func(conn net.Conn) {
		defer conn.Close()
		for {
			var hdr requestHeader
			if err := binary.Read(conn, byteOrder, &hdr); err != nil {
				return
			}
			if hdr.PayloadLen > 0 {
				payload := make([]byte, hdr.PayloadLen)
				_, _ = io.ReadFull(conn, payload)
			}
			// Hello succeeds, everything else fails
			var status uint32
			if hdr.Type != TypeHello {
				status = 1 // EPERM
			}
			resp := responseHeader{ForID: hdr.ID, Status: status}
			_ = binary.Write(conn, byteOrder, resp)
		}
	}
	go srv.serve()
	defer srv.close()

	client, err := Dial(sockPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	err = client.InterfaceDel(InterfaceDelRequest{IfaceID: 1})
	if err == nil {
		t.Fatal("expected error from server")
	}
}

func TestClose(t *testing.T) {
	client := setupClient(t)

	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second close or operation should fail
	_, err := client.InterfaceAdd(InterfaceAddRequest{Name: "tap0"})
	if err == nil {
		t.Fatal("expected error after close")
	}
}

func TestTapDevargs(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"grout-abc123def", "net_tap_groutabc123def,iface=grout-abc123def"},
		{"tap-e2e0", "net_tap_tape2e0,iface=tap-e2e0"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tapDevargs(tt.name))
	}

	// Distinct interface names must yield distinct DPDK vdev ids so concurrent
	// TAPs on the same grout instance do not collide. Compare just the vdev id
	// (the segment before the first comma); the iface= part always differs.
	vdevID := func(name string) string {
		return strings.SplitN(tapDevargs(name), ",", 2)[0]
	}
	assert.NotEqual(t, vdevID("grout-aaa111bbb"), vdevID("grout-ccc222ddd"))
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func TestCString(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"trailing NULs", []byte("tap0\x00\x00\x00"), "tap0"},
		{"no NUL", []byte("tap0"), "tap0"},
		{"empty", []byte("\x00\x00"), ""},
		// grout reuses the 16-byte name buffer without zeroing it, so a shorter
		// name can leave a stale tail from a previous longer name after its NUL.
		{"stale tail after NUL", []byte("br-shared\x00irtio\x00\x00"), "br-shared"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cString(tc.in))
		})
	}
}

// TestInterfaceListNameStopsAtFirstNUL is a regression test for a bridge whose
// name buffer carries a stale tail from a longer name that previously occupied
// the same interface id (e.g. "br-shared" reusing "br-grout-virtio"'s slot).
// InterfaceList must decode the name as "br-shared", not "br-shared\x00irtio".
func TestInterfaceListNameStopsAtFirstNUL(t *testing.T) {
	entry := make([]byte, ifaceSize)
	byteOrder.PutUint16(entry[offIfaceID:], 8)
	entry[offIfaceType] = uint8(InterfaceTypeBridge)
	nameBuf := entry[offIfaceName : offIfaceName+ifNameSize]
	copy(nameBuf, "br-shared")
	copy(nameBuf[len("br-shared")+1:], "irtio") // stale leftover past the NUL

	srv, sockPath := newMockServer(t)
	srv.handler = func(conn net.Conn) {
		defer conn.Close()
		for {
			var hdr requestHeader
			if err := binary.Read(conn, byteOrder, &hdr); err != nil {
				return
			}
			if hdr.PayloadLen > 0 {
				if _, err := io.ReadFull(conn, make([]byte, hdr.PayloadLen)); err != nil {
					return
				}
			}
			if hdr.Type == typeIfaceList {
				_ = binary.Write(conn, byteOrder, responseHeader{ForID: hdr.ID, PayloadLen: uint32(len(entry))})
				_, _ = conn.Write(entry)
				_ = binary.Write(conn, byteOrder, responseHeader{ForID: hdr.ID, PayloadLen: 0})
				continue
			}
			// hello and everything else: empty success response.
			_ = binary.Write(conn, byteOrder, responseHeader{ForID: hdr.ID, PayloadLen: 0})
		}
	}
	go srv.serve()
	t.Cleanup(srv.close)

	client, err := Dial(sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	ifaces, err := client.InterfaceList(InterfaceListRequest{})
	if err != nil {
		t.Fatalf("InterfaceList: %v", err)
	}
	if len(ifaces) != 1 {
		t.Fatalf("expected 1 interface, got %d", len(ifaces))
	}
	assert.Equal(t, "br-shared", ifaces[0].Name, "name must stop at the first NUL")
	assert.Equal(t, uint16(8), ifaces[0].ID)
	assert.Equal(t, InterfaceTypeBridge, ifaces[0].Type)
}
