package cni

import (
	"encoding/json"
	"fmt"
	"hash/fnv"

	"github.com/containernetworking/cni/pkg/types"
)

const (
	DefaultSocketPath = "/run/grout/grout.sock"

	// grIfaceNameSize is grout's interface name limit (IFNAMSIZ, incl. NUL).
	grIfaceNameSize = 16

	// ErrPluginNotAvailable is the CNI STATUS well-known error code meaning the
	// plugin cannot currently service ADD requests (CNI spec §"STATUS", code 50).
	ErrPluginNotAvailable uint = 50
)

type PluginConf struct {
	types.NetConf
	GroutSocketPath string `json:"groutSocketPath,omitempty"`
	// Bridge is the name of the grout bridge to attach pod interfaces to. Pods
	// on the same bridge form one L2 domain and reach each other through grout.
	// Defaults to a name derived from the network name.
	Bridge string `json:"bridge,omitempty"`
	// MTU, when non-zero, is applied to the pod interface and the grout member
	// port. types.NetConf has no MTU field, so it is declared here.
	MTU int `json:"mtu,omitempty"`
	// InterfaceType selects the pod interface kind: "tap" (default; a kernel TAP
	// moved into the pod netns) or "virtio" (a vhost-user socket for a DPDK app
	// in the pod, no kernel netdev).
	InterfaceType string `json:"interfaceType,omitempty"`
	// LogLevel sets the plugin's slog level: "debug", "info", "warn" (default)
	// or "error". A CNI plugin is a short-lived kubelet-invoked process, so this
	// travels in the network config rather than an env var the operator cannot
	// easily set per node. Logs go to stderr, which the runtime captures.
	LogLevel string `json:"logLevel,omitempty"`
}

const (
	InterfaceTypeTAP    = "tap"
	InterfaceTypeVirtio = "virtio"
)

func LoadConfig(stdin []byte) (*PluginConf, error) {
	conf := &PluginConf{
		GroutSocketPath: DefaultSocketPath,
	}
	if err := json.Unmarshal(stdin, conf); err != nil {
		return nil, fmt.Errorf("parsing CNI config: %w", err)
	}
	if conf.IPAM.IsEmpty() {
		return nil, fmt.Errorf("IPAM configuration is required (specify an ipam section)")
	}
	if conf.Name == "" {
		return nil, fmt.Errorf("network name is required")
	}
	// MTU is cast to uint16 downstream (grout iface base); reject values that
	// would wrap.
	if conf.MTU < 0 || conf.MTU > 65535 {
		return nil, fmt.Errorf("mtu must be between 0 and 65535, got %d", conf.MTU)
	}
	if conf.Bridge == "" {
		conf.Bridge = bridgeNameFor(conf.Name)
	} else if len(conf.Bridge) >= grIfaceNameSize {
		return nil, fmt.Errorf("bridge name %q exceeds %d bytes", conf.Bridge, grIfaceNameSize-1)
	}
	switch conf.InterfaceType {
	case "":
		conf.InterfaceType = InterfaceTypeTAP
	case InterfaceTypeTAP, InterfaceTypeVirtio:
	default:
		return nil, fmt.Errorf("interfaceType must be %q or %q, got %q", InterfaceTypeTAP, InterfaceTypeVirtio, conf.InterfaceType)
	}
	if conf.LogLevel != "" && !validLogLevel(conf.LogLevel) {
		return nil, fmt.Errorf("logLevel must be one of debug, info, warn, error, got %q", conf.LogLevel)
	}
	return conf, nil
}

// bridgeNameFor derives a grout bridge name from the network name, capped to
// grout's interface name limit. Names too long to fit are truncated with a hash
// suffix so distinct networks do not collapse onto the same bridge.
func bridgeNameFor(network string) string {
	name := "br-" + network
	maxLen := grIfaceNameSize - 1 // leave room for the NUL terminator
	if len(name) <= maxLen {
		return name
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(network))
	suffix := fmt.Sprintf("-%04x", h.Sum32()&0xffff)
	head := maxLen - len(suffix)
	if head < len("br-") {
		head = len("br-")
	}
	return name[:head] + suffix
}
