package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"

	"github.com/sanjeevliv/mycni/pkg/cni"
	"github.com/sanjeevliv/mycni/pkg/ipam"
	"github.com/sanjeevliv/mycni/pkg/netutil"
)

var logger *slog.Logger

func init() {
	logFile, err := os.OpenFile("/var/log/mycni-cni.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		logFile, err = os.OpenFile("/tmp/mycni-cni.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
			return
		}
	}
	logger = slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func cmdAdd(args *skel.CmdArgs) error {
	logger.Info("cmdAdd called", "containerID", args.ContainerID, "netns", args.Netns, "ifName", args.IfName)

	conf, err := cni.ParseConfig(args.StdinData)
	if err != nil {
		logger.Error("failed to parse config", "error", err)
		return err
	}

	ipamInstance, err := ipam.New(conf.DataDir, conf.PodIPNet)
	if err != nil {
		logger.Error("failed to initialize IPAM", "error", err)
		return err
	}

	// 1. Ensure bridge exists, has gateway IP, and is UP
	bridge, err := netutil.EnsureBridge(conf.Bridge, ipamInstance.GatewayIPNet(), conf.MTU)
	if err != nil {
		logger.Error("failed to ensure bridge", "error", err)
		return err
	}

	// 2. Allocate pod IP
	podIP, err := ipamInstance.Allocate(args.ContainerID, args.IfName)
	if err != nil {
		logger.Error("failed to allocate pod IP", "error", err)
		return err
	}
	logger.Info("allocated IP for pod", "ip", podIP.String(), "containerID", args.ContainerID)

	// 3. Setup veth interfaces
	hostVeth, contMAC, err := netutil.SetupPodVeth(args.Netns, args.IfName, conf.MTU, bridge, podIP, ipamInstance.GetGatewayIP())
	if err != nil {
		logger.Error("failed to setup veth interfaces", "error", err)
		_ = ipamInstance.Release(args.ContainerID, args.IfName)
		return err
	}

	// 4. Enable sysctl net.ipv4.ip_forward=1 and bridge netfilter
	if err := netutil.EnableIPForward(); err != nil {
		logger.Warn("failed to set net.ipv4.ip_forward", "error", err)
	}
	_ = netutil.EnableBridgeNetfilter()

	// 5. Construct Result
	result := &types100.Result{
		CNIVersion: conf.CNIVersion,
		Interfaces: []*types100.Interface{
			{
				Name: conf.Bridge,
				Mac:  bridge.Attrs().HardwareAddr.String(),
			},
			{
				Name: hostVeth.Attrs().Name,
				Mac:  hostVeth.Attrs().HardwareAddr.String(),
			},
			{
				Name:    args.IfName,
				Mac:     contMAC,
				Sandbox: args.Netns,
			},
		},
		IPs: []*types100.IPConfig{
			{
				Address:   *podIP,
				Gateway:   ipamInstance.GetGatewayIP(),
				Interface: types100.Int(2),
			},
		},
		Routes: []*types.Route{
			{
				Dst: net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
				GW:  ipamInstance.GetGatewayIP(),
			},
		},
	}

	logger.Info("cmdAdd succeeded", "podIP", podIP.String(), "hostVeth", hostVeth.Attrs().Name)
	return types.PrintResult(result, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	logger.Info("cmdDel called", "containerID", args.ContainerID, "netns", args.Netns, "ifName", args.IfName)

	conf, err := cni.ParseConfig(args.StdinData)
	if err != nil {
		logger.Error("failed to parse config in cmdDel", "error", err)
		return err
	}

	ipamInstance, err := ipam.New(conf.DataDir, conf.PodIPNet)
	if err != nil {
		logger.Error("failed to initialize IPAM in cmdDel", "error", err)
		return err
	}

	// Release IP lease
	if err := ipamInstance.Release(args.ContainerID, args.IfName); err != nil {
		logger.Error("failed to release IP in cmdDel", "error", err)
	}

	// Teardown container veth interface if netns provided
	if args.Netns != "" {
		if err := netutil.TeardownPodVeth(args.Netns, args.IfName); err != nil {
			logger.Warn("failed to teardown veth in cmdDel", "error", err)
		}
	}

	logger.Info("cmdDel completed successfully", "containerID", args.ContainerID)
	return nil
}

func cmdCheck(args *skel.CmdArgs) error {
	logger.Info("cmdCheck called", "containerID", args.ContainerID, "netns", args.Netns, "ifName", args.IfName)

	conf, err := cni.ParseConfig(args.StdinData)
	if err != nil {
		return err
	}

	ipamInstance, err := ipam.New(conf.DataDir, conf.PodIPNet)
	if err != nil {
		return err
	}

	podIP, err := ipamInstance.GetLease(args.ContainerID)
	if err != nil {
		return fmt.Errorf("no IP lease found for container %s: %w", args.ContainerID, err)
	}

	if args.Netns != "" {
		if err := netutil.CheckPodInterface(args.Netns, args.IfName, podIP); err != nil {
			return fmt.Errorf("interface check failed: %w", err)
		}
	}

	logger.Info("cmdCheck succeeded", "containerID", args.ContainerID)
	return nil
}

func main() {
	skel.PluginMainFuncs(
		skel.CNIFuncs{
			Add:   cmdAdd,
			Del:   cmdDel,
			Check: cmdCheck,
		},
		version.PluginSupports("0.3.0", "0.3.1", "0.4.0", "1.0.0"),
		bv.BuildString("mycni"),
	)
}
