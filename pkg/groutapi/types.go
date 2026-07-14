package groutapi

import "net"

// InterfaceAddRequest contains parameters for adding an interface to grout.
type InterfaceAddRequest struct {
	Name string
	Type InterfaceType
	// DomainID, when non-zero, makes the new interface a member of the bridge
	// (link domain) with that interface id. grout derives the interface mode
	// (e.g. bridge) from the domain interface's type.
	DomainID uint16
	// MTU, when non-zero, sets the interface MTU.
	MTU uint16
	// VhostUserPath, when set, creates the port with net_vhost devargs exposing
	// a vhost-user socket at this path (for DPDK/virtio pods) instead of a TAP.
	VhostUserPath string
	// VXLAN, when Type is InterfaceTypeVXLAN, configures the VXLAN interface.
	VXLAN *VXLANConfig
}

// VXLANConfig configures a VXLAN interface (gr_iface_info_vxlan in
// modules/l2/api/gr_l2.h). One VXLAN interface exists per VNI; cross-node
// flooding to other VTEPs is managed separately via FloodAdd/FloodDel.
type VXLANConfig struct {
	VNI uint32
	// EncapVRFID is the VRF the encapsulated (outer) packet is routed
	// through to reach remote VTEPs.
	EncapVRFID uint16
	// DstPort is the outer UDP destination port; 0 lets grout use its
	// default (4789).
	DstPort uint16
	// LocalAddr is this node's VTEP address (the outer packet's source IP).
	LocalAddr net.IP
}

// FloodAddRequest adds a remote VTEP to a VNI's head-end-replication
// flood list (gr_flood_add_req), used for cross-node VXLAN BUM traffic.
type FloodAddRequest struct {
	VRFID   uint16
	VNI     uint32
	Addr    net.IP
	ExistOK bool
}

// FloodDelRequest removes a remote VTEP from a VNI's flood list
// (gr_flood_del_req).
type FloodDelRequest struct {
	VRFID     uint16
	VNI       uint32
	Addr      net.IP
	MissingOK bool
}

// FloodListRequest lists the remote-VTEP flood entries for a VRF
// (gr_flood_list_req).
type FloodListRequest struct {
	VRFID uint16
}

// FloodVTEPInfo describes one remote VTEP flood-list entry returned by
// FloodList.
type FloodVTEPInfo struct {
	VNI  uint32
	Addr net.IP
}

// InterfaceAddResponse contains the result of adding an interface.
type InterfaceAddResponse struct {
	IfaceID uint16
}

// InterfaceDelRequest contains parameters for removing an interface.
type InterfaceDelRequest struct {
	IfaceID uint16
}

// InterfaceListRequest contains parameters for listing interfaces.
type InterfaceListRequest struct {
	Type InterfaceType // 0 for all types
}

// InterfaceInfo describes an interface returned by InterfaceList.
type InterfaceInfo struct {
	ID     uint16
	Type   InterfaceType
	Name   string
	Domain uint16 // domain_id: the bridge this interface is a member of (0 = none)
}

// AddressAddRequest contains parameters for adding an IP address to an interface.
type AddressAddRequest struct {
	IfaceID uint16
	Address net.IPNet
	ExistOK bool
}

// AddressDelRequest contains parameters for removing an IP address.
type AddressDelRequest struct {
	IfaceID   uint16
	Address   net.IPNet
	MissingOK bool
}

// RouteAddRequest contains parameters for adding a route.
type RouteAddRequest struct {
	VRFID       uint16
	Destination net.IPNet
	NextHop     net.IP
	IfaceID     uint16
	ExistOK     bool
}

// RouteDelRequest contains parameters for removing a route.
type RouteDelRequest struct {
	VRFID       uint16
	Destination net.IPNet
	MissingOK   bool
}

// InterfaceType identifies the type of a grout interface.
type InterfaceType uint8

const (
	InterfaceTypeUndef  InterfaceType = 0
	InterfaceTypeVRF    InterfaceType = 1
	InterfaceTypePort   InterfaceType = 2
	InterfaceTypeTAP    InterfaceType = 2 // TAP is a PORT with net_tap devargs
	InterfaceTypeVLAN   InterfaceType = 3
	InterfaceTypeIPIP   InterfaceType = 4
	InterfaceTypeBond   InterfaceType = 5
	InterfaceTypeBridge InterfaceType = 6
	InterfaceTypeVXLAN  InterfaceType = 7
)
