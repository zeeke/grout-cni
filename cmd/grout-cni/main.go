// Package main implements the grout-cni binary, a CNI plugin that manages
// pod network interfaces through grout's Unix socket API.
package main

import (
	"fmt"
	"log/slog"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/zeeke/grout-cni/pkg/cni"
	"github.com/zeeke/grout-cni/pkg/groutapi"
)

// 1.1.0 is required for the GC and STATUS verbs; the runtime only dispatches
// them when the network config's cniVersion is >= 1.1.0.
var supportedVersions = []string{"0.3.1", "0.4.0", "1.0.0", "1.1.0"}

// Package-level function variables default to nil (use production implementation).
// Tests can override with a defer/restore pattern.
var (
	ipamAddFunc           cni.IPAMAddFunc
	ipamDelFunc           cni.IPAMDelFunc
	moveLinkFunc          cni.MoveLinkFunc
	configurePodIfaceFunc cni.ConfigurePodIfaceFunc
)

func cmdAdd(args *skel.CmdArgs) error {
	config, err := cni.LoadConfig(args.StdinData)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	cni.ConfigureLogging(config)

	slog.Debug("connecting to grout", "socket", config.GroutSocketPath)
	client, err := groutapi.Dial(config.GroutSocketPath)
	if err != nil {
		return fmt.Errorf("connecting to grout: %w", err)
	}
	defer func() { _ = client.Close() }()

	result, err := cni.HandleAdd(&cni.AddConfig{
		Client:            client,
		Config:            config,
		Args:              args,
		IPAMAdd:           ipamAddFunc,
		IPAMDel:           ipamDelFunc,
		MoveLink:          moveLinkFunc,
		ConfigurePodIface: configurePodIfaceFunc,
	})
	if err != nil {
		return fmt.Errorf("CNI ADD: %w", err)
	}

	return result.Print()
}

func cmdDel(args *skel.CmdArgs) error {
	config, err := cni.LoadConfig(args.StdinData)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	cni.ConfigureLogging(config)

	slog.Debug("connecting to grout", "socket", config.GroutSocketPath)
	client, err := groutapi.Dial(config.GroutSocketPath)
	if err != nil {
		return fmt.Errorf("connecting to grout: %w", err)
	}
	defer func() { _ = client.Close() }()

	return cni.HandleDel(&cni.DelConfig{
		Client:  client,
		Config:  config,
		Args:    args,
		IPAMDel: ipamDelFunc,
	})
}

func cmdCheck(args *skel.CmdArgs) error {
	config, err := cni.LoadConfig(args.StdinData)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	cni.ConfigureLogging(config)

	client, err := groutapi.Dial(config.GroutSocketPath)
	if err != nil {
		return fmt.Errorf("connecting to grout: %w", err)
	}
	defer func() { _ = client.Close() }()

	return cni.HandleCheck(&cni.DelConfig{
		Client: client,
		Config: config,
		Args:   args,
	})
}

// cmdGC implements CNI GC: reap grout ports on this network's bridge that the
// runtime no longer lists as valid.
func cmdGC(args *skel.CmdArgs) error {
	config, err := cni.LoadConfig(args.StdinData)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	cni.ConfigureLogging(config)

	client, err := groutapi.Dial(config.GroutSocketPath)
	if err != nil {
		return fmt.Errorf("connecting to grout: %w", err)
	}
	defer func() { _ = client.Close() }()

	return cni.HandleGC(&cni.GCConfig{
		Client: client,
		Config: config,
		Args:   args,
	})
}

// cmdStatus implements CNI STATUS: report whether the plugin can currently
// service ADD requests, which for grout-k means grout's control socket is
// reachable.
func cmdStatus(args *skel.CmdArgs) error {
	config, err := cni.LoadConfig(args.StdinData)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	cni.ConfigureLogging(config)

	client, err := groutapi.Dial(config.GroutSocketPath)
	if err != nil {
		return types.NewError(cni.ErrPluginNotAvailable,
			fmt.Sprintf("grout not reachable at %s: %v", config.GroutSocketPath, err), "")
	}
	_ = client.Close()
	return nil
}

func main() {
	skel.PluginMainFuncs(skel.CNIFuncs{
		Add:    cmdAdd,
		Del:    cmdDel,
		Check:  cmdCheck,
		GC:     cmdGC,
		Status: cmdStatus,
	}, version.PluginSupports(supportedVersions...), "grout-cni")
}
