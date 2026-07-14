package groutapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"net"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	ifNameSize      = 16 // IFNAMSIZ
	ifDescSize      = 256
	portDevargsSize = 128
	portDriverSize  = 32

	// gr_iface base layout offsets
	ifaceBaseSize = 20                                      // sizeof(__gr_iface_base) with padding
	ifaceSize     = ifaceBaseSize + ifNameSize + ifDescSize // 292

	// gr_iface_info_port layout
	portInfoBaseSize = 14                                                  // sizeof(__gr_iface_info_port_base)
	portInfoSize     = portInfoBaseSize + portDevargsSize + portDriverSize // 174

	// gr_iface_info_bridge layout (modules/l2): base { uint16 ageing_time,
	// uint16 flags, rte_ether_addr mac[6], uint16 n_members } + uint16
	// members[GR_BRIDGE_MAX_MEMBERS].
	bridgeMaxMembers = 64
	bridgeInfoBase   = 12                                  // ageing(2)+flags(2)+mac(6)+n_members(2)
	bridgeInfoSize   = bridgeInfoBase + bridgeMaxMembers*2 // 140
	offBridgeInfoMAC = 4                                   // mac offset within the bridge info

	// struct l3_addr (api/gr_net_types.h): uint8_t af + 3 bytes padding +
	// a 4-byte-aligned union of {ip4_addr_t; rte_ipv6_addr[16]} = 20 bytes.
	l3AddrSize    = 20
	offL3AddrAF   = 0
	offL3AddrIPv4 = 4
	afIP4         = 2 // GR_AF_IP4 = AF_INET (Linux)

	// gr_ip6_ifaddr: uint16 iface_id + rte_ipv6_addr ip[16] + uint8 prefixlen
	// + 1 byte pad = 20 bytes. gr_ip6_addr_add/del_req appends uint8 flag + pad = 22.
	ip6AddrReqSize = 22
	offIP6IfaceID  = 0
	offIP6Addr     = 2  // rte_ipv6_addr, 16 bytes
	offIP6Prefix   = 18 // uint8_t prefixlen
	offIP6Flag     = 20 // exist_ok or missing_ok

	// gr_ip6_route_add_req layout (44 bytes):
	//   [0:2]   uint16_t vrf_id
	//   [2:18]  ip6_net dest.ip (16 bytes)
	//   [18]    uint8_t dest.prefixlen
	//   [19:35] rte_ipv6_addr nh (16 bytes)
	//   [35]    1 byte padding (align uint32_t)
	//   [36:40] uint32_t nh_id
	//   [40]    uint8_t origin
	//   [41]    uint8_t exist_ok
	//   [42:44] padding
	ip6RouteAddReqSize = 44
	offIP6RteVRF       = 0
	offIP6RteDest      = 2  // 16-byte IPv6 addr
	offIP6RteDestPfx   = 18 // uint8_t prefixlen
	offIP6RteNH        = 19 // 16-byte IPv6 addr
	offIP6RteOrigin = 40 // uint8_t
	offIP6RteExistOK   = 41 // uint8_t

	// gr_ip6_route_del_req layout (20 bytes):
	//   [0:2]  uint16_t vrf_id
	//   [2:18] ip6_net dest.ip (16 bytes)
	//   [18]   uint8_t dest.prefixlen
	//   [19]   uint8_t missing_ok
	ip6RouteDelReqSize  = 20
	offIP6RteDelMissing = 19

	// gr_iface_info_vxlan layout (modules/l2/api/gr_l2.h): uint32 vni +
	// uint16 encap_vrf_id + uint16 dst_port + l3_addr local (20) +
	// rte_ether_addr mac (6), padded to 36 for 4-byte struct alignment.
	// mac is left zero so grout auto-generates one, as it does for ports.
	vxlanInfoSize    = 36
	offVxlanVNI      = 0
	offVxlanEncapVRF = 4
	offVxlanDstPort  = 6
	offVxlanLocal    = 8 // l3_addr, 20 bytes

	// gr_flood_entry layout (modules/l2/api/gr_l2.h): uint8_t type +
	// padding + uint16_t vrf_id + the gr_flood_vtep union member
	// (uint32_t vni + l3_addr addr) = 28 bytes. gr_flood_add_req/
	// gr_flood_del_req append a trailing bool, padded to 32.
	floodEntrySize   = 28
	floodReqSize     = 32
	offFloodType     = 0
	offFloodVRFID    = 2
	offFloodVTEPVNI  = 4
	offFloodVTEPAddr = 8
	floodTypeVTEP    = 1 // GR_FLOOD_T_VTEP

	// C struct offsets within gr_iface
	offIfaceID     = 0
	offIfaceType   = 2
	offIfaceFlags  = 4
	offIfaceMTU    = 8  // __gr_iface_base.mtu
	offIfaceDomain = 12 // __gr_iface_base.domain_id
	offIfaceName   = 20
	offIfaceInfo   = 292                             // start of info[] flexible array
	offPortDevargs = offIfaceInfo + portInfoBaseSize // 306

	grIfaceIDUndef uint16 = 0
	grIfaceFlagUp  uint16 = 1
	grVRFDefaultID uint16 = 1
)

// Client communicates with a grout instance over a Unix socket.
type Client struct {
	conn  net.Conn
	mu    sync.Mutex
	reqID atomic.Uint32
}

// Dial connects to grout at the given Unix socket path.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("connecting to grout: %w", err)
	}
	c := &Client{conn: conn}
	if err := c.hello(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("grout handshake: %w", err)
	}
	return c, nil
}

// Close closes the connection to grout.
func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) hello() error {
	// gr_hello_req: uint32_t api_version + char version[128]
	payload := make([]byte, 4+128)
	byteOrder.PutUint32(payload[0:4], apiVersion)
	copy(payload[4:], "grout-k-cni")
	return c.request(TypeHello, payload, nil)
}

func (c *Client) nextID() uint32 {
	return c.reqID.Add(1)
}

func (c *Client) request(msgType uint32, payload []byte, resp any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID()
	if err := writeRequest(c.conn, id, msgType, payload); err != nil {
		return err
	}

	hdr, respPayload, err := readResponse(c.conn)
	if err != nil {
		return err
	}
	if hdr.ForID != id {
		return fmt.Errorf("response ID mismatch: got %d, want %d", hdr.ForID, id)
	}
	if hdr.Status != 0 {
		return fmt.Errorf("grout error: status %d", hdr.Status)
	}

	if resp != nil && len(respPayload) > 0 {
		if err := binary.Read(
			bytes.NewReader(respPayload),
			byteOrder,
			resp,
		); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

func (c *Client) requestStream(msgType uint32, payload []byte) ([][]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID()
	if err := writeRequest(c.conn, id, msgType, payload); err != nil {
		return nil, err
	}

	var items [][]byte
	for {
		hdr, respPayload, err := readResponse(c.conn)
		if err != nil {
			return nil, err
		}
		if hdr.ForID != id {
			return nil, fmt.Errorf("response ID mismatch: got %d, want %d", hdr.ForID, id)
		}
		if hdr.Status != 0 {
			return nil, fmt.Errorf("grout error: status %d", hdr.Status)
		}
		if hdr.PayloadLen == 0 {
			break
		}
		items = append(items, respPayload)
	}
	return items, nil
}

// cString decodes a fixed-width, NUL-terminated C string field: the value is
// everything up to the first NUL. grout reuses interface-name buffers across
// interface ids without zeroing them, so a shorter name (e.g. "br-shared") can
// leave a stale tail from a previous longer name (e.g. "br-grout-virtio") after
// its terminator. Trimming only trailing NULs would keep that garbage; cutting
// at the first NUL matches how grout itself (and grcli) reads the field.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// InterfaceList returns all interfaces matching the given type filter (0 for all).
func (c *Client) InterfaceList(req InterfaceListRequest) ([]InterfaceInfo, error) {
	// gr_iface_list_req: uint8_t type
	payload := []byte{uint8(req.Type)}

	items, err := c.requestStream(TypeIfaceList, payload)
	if err != nil {
		return nil, fmt.Errorf("interface list: %w", err)
	}

	result := make([]InterfaceInfo, 0, len(items))
	for _, data := range items {
		if len(data) < ifaceSize {
			continue
		}
		id := byteOrder.Uint16(data[offIfaceID : offIfaceID+2])
		ifType := data[offIfaceType]
		domain := byteOrder.Uint16(data[offIfaceDomain : offIfaceDomain+2])
		name := cString(data[offIfaceName : offIfaceName+ifNameSize])
		result = append(result, InterfaceInfo{
			ID:     id,
			Type:   InterfaceType(ifType),
			Name:   name,
			Domain: domain,
		})
	}
	return result, nil
}

// InterfaceAdd adds a new interface to grout. Ports/TAPs carry a
// gr_iface_info_port section (with devargs); a bridge is the gr_iface base only.
func (c *Client) InterfaceAdd(req InterfaceAddRequest) (*InterfaceAddResponse, error) {
	isBridge := req.Type == InterfaceTypeBridge
	isVXLAN := req.Type == InterfaceTypeVXLAN

	// gr_iface_add_req = gr_iface (292) + the type-specific info section
	// (gr_iface_info_port for ports, gr_iface_info_bridge for bridges,
	// gr_iface_info_vxlan for VXLAN).
	size := ifaceSize
	switch {
	case isBridge:
		size += bridgeInfoSize
	case isVXLAN:
		size += vxlanInfoSize
	default:
		size += portInfoSize
	}
	payload := make([]byte, size)

	// gr_iface base
	byteOrder.PutUint16(payload[offIfaceID:], grIfaceIDUndef)
	payload[offIfaceType] = uint8(req.Type)
	byteOrder.PutUint16(payload[offIfaceFlags:], grIfaceFlagUp)
	// domain_id makes the interface a member of that bridge; grout derives the
	// mode from the domain interface's type.
	if req.DomainID != 0 {
		byteOrder.PutUint16(payload[offIfaceDomain:], req.DomainID)
	}
	if req.MTU != 0 {
		byteOrder.PutUint16(payload[offIfaceMTU:], req.MTU)
	}
	copy(payload[offIfaceName:offIfaceName+ifNameSize], req.Name)

	if isBridge {
		// gr_iface_info_bridge: provide an explicit, valid locally-administered
		// unicast MAC. Without an info section grout reads an uninitialized MAC
		// (often with the multicast bit set), which the kernel rejects, leaving
		// the bridge in a broken state.
		copy(payload[offIfaceInfo+offBridgeInfoMAC:], bridgeMAC(req.Name))
	} else if isVXLAN {
		if req.VXLAN == nil {
			return nil, fmt.Errorf("interface add %q: VXLAN config required for a VXLAN interface", req.Name)
		}
		info := payload[offIfaceInfo:]
		byteOrder.PutUint32(info[offVxlanVNI:], req.VXLAN.VNI)
		byteOrder.PutUint16(info[offVxlanEncapVRF:], req.VXLAN.EncapVRFID)
		byteOrder.PutUint16(info[offVxlanDstPort:], req.VXLAN.DstPort)
		if err := putL3AddrIPv4(info[offVxlanLocal:offVxlanLocal+l3AddrSize], req.VXLAN.LocalAddr); err != nil {
			return nil, fmt.Errorf("interface add %q: VXLAN local address: %w", req.Name, err)
		}
	} else {
		// gr_iface_info_port: set devargs. The DPDK vdev id (net_tap_<id> /
		// net_vhost_<id>) must be unique per port, so derive it from the
		// (already unique) interface name. A TAP uses iface=<kernel netdev name>;
		// a vhost-user port uses iface=<socket path> for DPDK/virtio pods.
		devargs := tapDevargs(req.Name)
		if req.VhostUserPath != "" {
			devargs = vhostDevargs(req.Name, req.VhostUserPath)
		}
		if len(devargs) >= portDevargsSize {
			return nil, fmt.Errorf("interface add %q: devargs too long (%d >= %d)", req.Name, len(devargs), portDevargsSize)
		}
		copy(payload[offPortDevargs:offPortDevargs+portDevargsSize], devargs)
	}

	var wireResp struct {
		IfaceID uint16
	}
	if err := c.request(TypeIfaceAdd, payload, &wireResp); err != nil {
		return nil, fmt.Errorf("interface add %q: %w", req.Name, err)
	}
	return &InterfaceAddResponse{IfaceID: wireResp.IfaceID}, nil
}

// bridgeMAC derives a deterministic, locally-administered unicast MAC for a
// bridge from its name (02:xx:xx:xx:xx:xx — bit0 clear = unicast, bit1 set =
// local). Stable per name so re-creates reuse the same address.
func bridgeMAC(name string) []byte {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	s := h.Sum64()
	return []byte{0x02, byte(s), byte(s >> 8), byte(s >> 16), byte(s >> 24), byte(s >> 32)}
}

// tapDevargs builds the DPDK devargs for a TAP port (iface = kernel netdev name).
func tapDevargs(name string) string {
	return "net_tap_" + vdevID(name) + ",iface=" + name
}

// vhostDevargs builds the DPDK devargs for a vhost-user port (iface = unix
// socket path) used by DPDK/virtio pods.
func vhostDevargs(name, socketPath string) string {
	return "net_vhost_" + vdevID(name) + ",iface=" + socketPath
}

// vdevID derives a unique DPDK vdev id from an interface name (alphanumerics
// only, lowercased) so distinct ports do not collide.
func vdevID(name string) string {
	var id strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			id.WriteRune(r)
		}
	}
	return id.String()
}

// InterfaceDel removes an interface from grout.
func (c *Client) InterfaceDel(req InterfaceDelRequest) error {
	// gr_iface_del_req: uint16_t iface_id
	payload := make([]byte, 2)
	byteOrder.PutUint16(payload[0:2], req.IfaceID)
	if err := c.request(TypeIfaceDel, payload, nil); err != nil {
		return fmt.Errorf("interface del %d: %w", req.IfaceID, err)
	}
	return nil
}

// AddressAdd adds an IPv4 or IPv6 address to an interface in grout.
func (c *Client) AddressAdd(req AddressAddRequest) error {
	ones, _ := req.Address.Mask.Size()

	if ip4 := req.Address.IP.To4(); ip4 != nil {
		// gr_ip4_addr_add_req layout (16 bytes)
		payload := make([]byte, 16)
		byteOrder.PutUint16(payload[0:2], req.IfaceID)
		copy(payload[4:8], ip4)
		payload[8] = uint8(ones)
		if req.ExistOK {
			payload[12] = 1
		}
		if err := c.request(typeIP4AddrAdd, payload, nil); err != nil {
			return fmt.Errorf("address add %s on iface %d: %w", req.Address.String(), req.IfaceID, err)
		}
		return nil
	}

	ip6 := req.Address.IP.To16()
	if ip6 == nil {
		return fmt.Errorf("address add: invalid IP address: %s", req.Address.IP)
	}
	payload := make([]byte, ip6AddrReqSize)
	byteOrder.PutUint16(payload[offIP6IfaceID:], req.IfaceID)
	copy(payload[offIP6Addr:offIP6Addr+16], ip6)
	payload[offIP6Prefix] = uint8(ones)
	if req.ExistOK {
		payload[offIP6Flag] = 1
	}
	if err := c.request(typeIP6AddrAdd, payload, nil); err != nil {
		return fmt.Errorf("address add %s on iface %d: %w", req.Address.String(), req.IfaceID, err)
	}
	return nil
}

// AddressDel removes an IPv4 or IPv6 address from an interface in grout.
func (c *Client) AddressDel(req AddressDelRequest) error {
	ones, _ := req.Address.Mask.Size()

	if ip4 := req.Address.IP.To4(); ip4 != nil {
		payload := make([]byte, 16)
		byteOrder.PutUint16(payload[0:2], req.IfaceID)
		copy(payload[4:8], ip4)
		payload[8] = uint8(ones)
		if req.MissingOK {
			payload[12] = 1
		}
		if err := c.request(typeIP4AddrDel, payload, nil); err != nil {
			return fmt.Errorf("address del %s on iface %d: %w", req.Address.String(), req.IfaceID, err)
		}
		return nil
	}

	ip6 := req.Address.IP.To16()
	if ip6 == nil {
		return fmt.Errorf("address del: invalid IP address: %s", req.Address.IP)
	}
	payload := make([]byte, ip6AddrReqSize)
	byteOrder.PutUint16(payload[offIP6IfaceID:], req.IfaceID)
	copy(payload[offIP6Addr:offIP6Addr+16], ip6)
	payload[offIP6Prefix] = uint8(ones)
	if req.MissingOK {
		payload[offIP6Flag] = 1
	}
	if err := c.request(typeIP6AddrDel, payload, nil); err != nil {
		return fmt.Errorf("address del %s on iface %d: %w", req.Address.String(), req.IfaceID, err)
	}
	return nil
}

// AddressFlush removes all IPv4 and IPv6 addresses from an interface.
// grout refuses to delete an interface that still holds addresses, so this is
// used to clear a bridge's gateway addresses before garbage-collecting it.
func (c *Client) AddressFlush(ifaceID uint16) error {
	payload := make([]byte, 2)
	byteOrder.PutUint16(payload[0:2], ifaceID)
	if err := c.request(typeIP4AddrFlush, payload, nil); err != nil {
		return fmt.Errorf("IPv4 address flush on iface %d: %w", ifaceID, err)
	}
	p6 := make([]byte, 2)
	byteOrder.PutUint16(p6[0:2], ifaceID)
	if err := c.request(typeIP6AddrFlush, p6, nil); err != nil {
		return fmt.Errorf("IPv6 address flush on iface %d: %w", ifaceID, err)
	}
	return nil
}

// RouteAdd adds an IPv4 or IPv6 route in grout.
func (c *Client) RouteAdd(req RouteAddRequest) error {
	ones, _ := req.Destination.Mask.Size()
	vrfID := req.VRFID
	if vrfID == 0 {
		vrfID = grVRFDefaultID
	}

	if destIP := req.Destination.IP.To4(); destIP != nil {
		nhIP := req.NextHop.To4()
		if nhIP == nil {
			return fmt.Errorf("route add: IPv4 destination with non-IPv4 next hop: %s", req.NextHop)
		}
		payload := make([]byte, 24)
		byteOrder.PutUint16(payload[0:2], vrfID)
		copy(payload[4:8], destIP)
		payload[8] = uint8(ones)
		copy(payload[12:16], nhIP)
		payload[20] = 4 // GR_NH_ORIGIN_STATIC
		if req.ExistOK {
			payload[21] = 1
		}
		if err := c.request(typeIP4RouteAdd, payload, nil); err != nil {
			return fmt.Errorf("route add %s via %s: %w", req.Destination.String(), req.NextHop, err)
		}
		return nil
	}

	destIP6 := req.Destination.IP.To16()
	if destIP6 == nil {
		return fmt.Errorf("route add: invalid destination: %s", req.Destination.IP)
	}
	nhIP6 := req.NextHop.To16()
	if nhIP6 == nil {
		return fmt.Errorf("route add: invalid next hop: %s", req.NextHop)
	}
	payload := make([]byte, ip6RouteAddReqSize)
	byteOrder.PutUint16(payload[offIP6RteVRF:], vrfID)
	copy(payload[offIP6RteDest:offIP6RteDest+16], destIP6)
	payload[offIP6RteDestPfx] = uint8(ones)
	copy(payload[offIP6RteNH:offIP6RteNH+16], nhIP6)
	payload[offIP6RteOrigin] = 4 // GR_NH_ORIGIN_STATIC
	if req.ExistOK {
		payload[offIP6RteExistOK] = 1
	}
	if err := c.request(typeIP6RouteAdd, payload, nil); err != nil {
		return fmt.Errorf("route add %s via %s: %w", req.Destination.String(), req.NextHop, err)
	}
	return nil
}

// RouteDel removes an IPv4 or IPv6 route from grout.
func (c *Client) RouteDel(req RouteDelRequest) error {
	ones, _ := req.Destination.Mask.Size()
	vrfID := req.VRFID
	if vrfID == 0 {
		vrfID = grVRFDefaultID
	}

	if destIP := req.Destination.IP.To4(); destIP != nil {
		payload := make([]byte, 16)
		byteOrder.PutUint16(payload[0:2], vrfID)
		copy(payload[4:8], destIP)
		payload[8] = uint8(ones)
		if req.MissingOK {
			payload[12] = 1
		}
		if err := c.request(typeIP4RouteDel, payload, nil); err != nil {
			return fmt.Errorf("route del %s: %w", req.Destination.String(), err)
		}
		return nil
	}

	destIP6 := req.Destination.IP.To16()
	if destIP6 == nil {
		return fmt.Errorf("route del: invalid destination: %s", req.Destination.IP)
	}
	payload := make([]byte, ip6RouteDelReqSize)
	byteOrder.PutUint16(payload[offIP6RteVRF:], vrfID)
	copy(payload[offIP6RteDest:offIP6RteDest+16], destIP6)
	payload[offIP6RteDestPfx] = uint8(ones)
	if req.MissingOK {
		payload[offIP6RteDelMissing] = 1
	}
	if err := c.request(typeIP6RouteDel, payload, nil); err != nil {
		return fmt.Errorf("route del %s: %w", req.Destination.String(), err)
	}
	return nil
}

// putL3AddrIPv4 encodes an IPv4 struct l3_addr (api/gr_net_types.h) into buf,
// which must be at least l3AddrSize bytes.
func putL3AddrIPv4(buf []byte, ip net.IP) error {
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("not an IPv4 address: %s", ip)
	}
	buf[offL3AddrAF] = afIP4
	copy(buf[offL3AddrIPv4:offL3AddrIPv4+4], ip4)
	return nil
}

// l3AddrIPv4 decodes an IPv4 struct l3_addr, or nil if it is not IPv4.
func l3AddrIPv4(buf []byte) net.IP {
	if buf[offL3AddrAF] != afIP4 {
		return nil
	}
	ip := make(net.IP, 4)
	copy(ip, buf[offL3AddrIPv4:offL3AddrIPv4+4])
	return ip
}

// encodeFloodEntry writes a gr_flood_entry (modules/l2/api/gr_l2.h) with
// type=GR_FLOOD_T_VTEP into buf, which must be at least floodEntrySize bytes.
func encodeFloodEntry(buf []byte, vrfID uint16, vni uint32, addr net.IP) error {
	buf[offFloodType] = floodTypeVTEP
	byteOrder.PutUint16(buf[offFloodVRFID:], vrfID)
	byteOrder.PutUint32(buf[offFloodVTEPVNI:], vni)
	return putL3AddrIPv4(buf[offFloodVTEPAddr:offFloodVTEPAddr+l3AddrSize], addr)
}

// FloodAdd adds a remote VTEP to a VNI's head-end-replication flood list, so
// BUM traffic on a VXLAN interface reaches that peer (cross-node mesh
// membership).
func (c *Client) FloodAdd(req FloodAddRequest) error {
	vrfID := req.VRFID
	if vrfID == 0 {
		vrfID = grVRFDefaultID
	}
	// gr_flood_add_req: gr_flood_entry (28) + bool exist_ok, padded to 32.
	payload := make([]byte, floodReqSize)
	if err := encodeFloodEntry(payload, vrfID, req.VNI, req.Addr); err != nil {
		return fmt.Errorf("flood add vni %d addr %s: %w", req.VNI, req.Addr, err)
	}
	if req.ExistOK {
		payload[floodEntrySize] = 1
	}
	if err := c.request(typeFloodAdd, payload, nil); err != nil {
		return fmt.Errorf("flood add vni %d addr %s: %w", req.VNI, req.Addr, err)
	}
	return nil
}

// FloodDel removes a remote VTEP from a VNI's flood list.
func (c *Client) FloodDel(req FloodDelRequest) error {
	vrfID := req.VRFID
	if vrfID == 0 {
		vrfID = grVRFDefaultID
	}
	// gr_flood_del_req: gr_flood_entry (28) + bool missing_ok, padded to 32.
	payload := make([]byte, floodReqSize)
	if err := encodeFloodEntry(payload, vrfID, req.VNI, req.Addr); err != nil {
		return fmt.Errorf("flood del vni %d addr %s: %w", req.VNI, req.Addr, err)
	}
	if req.MissingOK {
		payload[floodEntrySize] = 1
	}
	if err := c.request(typeFloodDel, payload, nil); err != nil {
		return fmt.Errorf("flood del vni %d addr %s: %w", req.VNI, req.Addr, err)
	}
	return nil
}

// FloodList returns the remote-VTEP flood-list entries for a VRF.
func (c *Client) FloodList(req FloodListRequest) ([]FloodVTEPInfo, error) {
	vrfID := req.VRFID
	if vrfID == 0 {
		vrfID = grVRFDefaultID
	}
	// gr_flood_list_req: uint8_t type + padding + uint16_t vrf_id (4 bytes).
	payload := make([]byte, 4)
	payload[offFloodType] = floodTypeVTEP
	byteOrder.PutUint16(payload[offFloodVRFID:], vrfID)

	items, err := c.requestStream(typeFloodList, payload)
	if err != nil {
		return nil, fmt.Errorf("flood list: %w", err)
	}

	result := make([]FloodVTEPInfo, 0, len(items))
	for _, data := range items {
		if len(data) < floodEntrySize {
			continue
		}
		vni := byteOrder.Uint32(data[offFloodVTEPVNI:])
		addr := l3AddrIPv4(data[offFloodVTEPAddr : offFloodVTEPAddr+l3AddrSize])
		result = append(result, FloodVTEPInfo{VNI: vni, Addr: addr})
	}
	return result, nil
}
