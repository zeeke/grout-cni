package groutapi

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Wire format matches grout's binary protocol.
// All fields are little-endian on x86-64 Linux.
var byteOrder = binary.LittleEndian

const (
	maxMsgLen = 128 * 1024
	// apiVersion is grout's GR_API_VERSION. grout rejects the hello handshake
	// (EBADMSG) when this does not match the server. grout 0.16.x speaks v3; the
	// iface/address/route wire layouts below are unchanged across v1..v3.
	apiVersion = 3

	// DefaultSocketPath is the default path to grout's Unix control socket.
	DefaultSocketPath = "/run/grout/grout.sock"
)

// Message type constants derived from grout's GR_MSG_TYPE(module, id) macro.
const (
	moduleMain  = 0xcafe
	moduleInfra = 0xacdc
	moduleIP4   = 0xf00d
	moduleIP6   = 0xfeed
	// moduleL2 is GR_L2_MODULE (modules/l2/api/gr_l2.h): bridge FDB and VXLAN
	// flood-VTEP (head-end replication) requests.
	moduleL2 = 0xbabe
)

// Exported message type constants, matching grout's GR_MSG_TYPE(module, id) macro.
const (
	TypeHello       = (moduleMain << 16) | 0x1981
	TypeIfaceAdd    = (moduleInfra << 16) | 0x0001
	TypeIfaceDel    = (moduleInfra << 16) | 0x0002
	TypeIfaceList   = (moduleInfra << 16) | 0x0004
	TypeIP4RouteAdd = (moduleIP4 << 16) | 0x0001
	TypeIP4RouteDel = (moduleIP4 << 16) | 0x0002
	TypeIP4AddrAdd  = (moduleIP4 << 16) | 0x0005
	TypeIP4AddrDel  = (moduleIP4 << 16) | 0x0006
	// GR_IP4_ADDR_FLUSH (gr_ip4.h enum: ADDR_ADD=5, DEL=6, LIST=7, FLUSH=8):
	// remove all IPv4 addresses from an interface.
	TypeIP4AddrFlush = (moduleIP4 << 16) | 0x0008
	TypeIP6RouteAdd  = (moduleIP6 << 16) | 0x0001
	TypeIP6RouteDel  = (moduleIP6 << 16) | 0x0002
	TypeIP6AddrAdd   = (moduleIP6 << 16) | 0x0005
	TypeIP6AddrDel   = (moduleIP6 << 16) | 0x0006
	TypeIP6AddrFlush = (moduleIP6 << 16) | 0x0008
	// gr_l2_requests enum order (gr_l2.h): GR_FDB_ADD=1, DEL, FLUSH, LIST,
	// CONFIG_GET, CONFIG_SET, then GR_FLOOD_ADD=7, DEL=8, LIST=9.
	TypeFloodAdd  = (moduleL2 << 16) | 0x0007
	TypeFloodDel  = (moduleL2 << 16) | 0x0008
	TypeFloodList = (moduleL2 << 16) | 0x0009
)

// Unexported const aliases for internal use.
const (
	typeHello        = TypeHello
	typeIfaceAdd     = TypeIfaceAdd
	typeIfaceDel     = TypeIfaceDel
	typeIfaceList    = TypeIfaceList
	typeIP4RouteAdd  = TypeIP4RouteAdd
	typeIP4RouteDel  = TypeIP4RouteDel
	typeIP4AddrAdd   = TypeIP4AddrAdd
	typeIP4AddrDel   = TypeIP4AddrDel
	typeIP4AddrFlush = TypeIP4AddrFlush
	typeIP6RouteAdd  = TypeIP6RouteAdd
	typeIP6RouteDel  = TypeIP6RouteDel
	typeIP6AddrAdd   = TypeIP6AddrAdd
	typeIP6AddrDel   = TypeIP6AddrDel
	typeIP6AddrFlush = TypeIP6AddrFlush
	typeFloodAdd     = TypeFloodAdd
	typeFloodDel     = TypeFloodDel
	typeFloodList    = TypeFloodList
)

type (
	// RequestHeader is the wire format header for requests.
	RequestHeader struct {
		ID         uint32
		Type       uint32
		PayloadLen uint32
	}
	// ResponseHeader is the wire format header for responses.
	ResponseHeader struct {
		ForID      uint32
		Status     uint32
		PayloadLen uint32
	}
)

type requestHeader = RequestHeader
type responseHeader = ResponseHeader

func writeRequest(w io.Writer, id, msgType uint32, payload []byte) error {
	hdr := requestHeader{
		ID:         id,
		Type:       msgType,
		PayloadLen: uint32(len(payload)),
	}
	if err := binary.Write(w, byteOrder, hdr); err != nil {
		return fmt.Errorf("writing request header: %w", err)
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return fmt.Errorf("writing request payload: %w", err)
		}
	}
	return nil
}

func readResponse(r io.Reader) (responseHeader, []byte, error) {
	var hdr responseHeader
	if err := binary.Read(r, byteOrder, &hdr); err != nil {
		return hdr, nil, fmt.Errorf("reading response header: %w", err)
	}
	if hdr.PayloadLen > maxMsgLen {
		return hdr, nil, fmt.Errorf("response payload too large: %d", hdr.PayloadLen)
	}
	var payload []byte
	if hdr.PayloadLen > 0 {
		payload = make([]byte, hdr.PayloadLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return hdr, nil, fmt.Errorf("reading response payload: %w", err)
		}
	}
	return hdr, payload, nil
}
