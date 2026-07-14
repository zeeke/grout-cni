package cni

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"

	"github.com/zeeke/grout-cni/pkg/groutapi"
)

// portPrefix marks a grout port as created by grout-k, so DEL/GC never touch
// grout interfaces owned by something else (physical ports, VXLAN, other apps).
const portPrefix = "grk-"

var portNameBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// groutPortName derives the deterministic grout port name for an attachment.
// It fits grout's IFNAMSIZ limit (grIfaceNameSize-1 = 15 bytes): the 4-byte
// prefix plus 11 base32 chars (~55 bits of sha256). Because the name includes
// ifName, a pod's second NAD gets a distinct port. Being deterministic, it lets
// grout — keyed by this name — serve as the source of truth for DEL/CHECK/GC
// without a node-local ref store. See docs/design/refless-cni-state.md.
func groutPortName(containerID, ifName string) string {
	sum := sha256.Sum256([]byte(containerID + "\x00" + ifName))
	return portPrefix + strings.ToLower(portNameBase32.EncodeToString(sum[:]))[:11]
}

// findPortByName returns the interface with the given name, if present. grout
// enforces globally-unique names, so a name match unambiguously identifies it.
func findPortByName(ifaces []groutapi.InterfaceInfo, name string) (groutapi.InterfaceInfo, bool) {
	for _, iface := range ifaces {
		if iface.Name == name {
			return iface, true
		}
	}
	return groutapi.InterfaceInfo{}, false
}
