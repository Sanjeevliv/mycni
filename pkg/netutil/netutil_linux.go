//go:build linux

package netutil

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
)

// EnsureBridge creates or verifies the bridge interface with the given gateway IP and MTU.
func EnsureBridge(bridgeName string, gateway *net.IPNet, mtu int) (*netlink.Bridge, error) {
	link, err := netlink.LinkByName(bridgeName)
	var bridge *netlink.Bridge

	if err != nil {
		bridge = &netlink.Bridge{
			LinkAttrs: netlink.LinkAttrs{
				Name: bridgeName,
				MTU:  mtu,
			},
		}
		if err := netlink.LinkAdd(bridge); err != nil {
			return nil, fmt.Errorf("failed to create bridge %q: %w", bridgeName, err)
		}
		// Refresh link object
		link, err = netlink.LinkByName(bridgeName)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve bridge %q after creation: %w", bridgeName, err)
		}
	}

	var ok bool
	bridge, ok = link.(*netlink.Bridge)
	if !ok {
		return nil, fmt.Errorf("interface %q exists but is not a bridge (type %T)", bridgeName, link)
	}

	// Ensure bridge MTU
	if bridge.MTU != mtu {
		if err := netlink.LinkSetMTU(bridge, mtu); err != nil {
			return nil, fmt.Errorf("failed to set bridge MTU: %w", err)
		}
	}

	// Bring bridge UP
	if err := netlink.LinkSetUp(bridge); err != nil {
		return nil, fmt.Errorf("failed to set bridge %q UP: %w", bridgeName, err)
	}

	// Ensure gateway address is configured on the bridge
	addrs, err := netlink.AddrList(bridge, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses for bridge %q: %w", bridgeName, err)
	}

	hasGW := false
	for _, addr := range addrs {
		if addr.IP.Equal(gateway.IP) && addr.Mask.String() == gateway.Mask.String() {
			hasGW = true
			break
		}
	}

	if !hasGW {
		gwAddr := &netlink.Addr{IPNet: gateway}
		if err := netlink.AddrAdd(bridge, gwAddr); err != nil && !os.IsExist(err) && !strings.Contains(err.Error(), "file exists") {
			return nil, fmt.Errorf("failed to add gateway %v to bridge %q: %w", gateway, bridgeName, err)
		}
	}

	return bridge, nil
}

// SetupPodVeth configures veth interfaces in both container and host network namespaces.
func SetupPodVeth(netnsPath string, ifName string, mtu int, bridge *netlink.Bridge, podIP *net.IPNet, gateway net.IP) (hostVeth netlink.Link, contMAC string, err error) {
	podNS, err := ns.GetNS(netnsPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to open netns %q: %w", netnsPath, err)
	}
	defer podNS.Close()

	hostNS, err := ns.GetCurrentNS()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get host netns: %w", err)
	}
	defer hostNS.Close()

	var hostIfaceName string

	err = podNS.Do(func(_ ns.NetNS) error {
		// Create veth pair; moves peer into hostNS
		hostIface, contIface, err := ip.SetupVeth(ifName, mtu, "", hostNS)
		if err != nil {
			return fmt.Errorf("failed to setup veth pair: %w", err)
		}
		hostIfaceName = hostIface.Name
		contMAC = contIface.HardwareAddr.String()

		// Bring lo UP
		loLink, err := netlink.LinkByName("lo")
		if err == nil {
			_ = netlink.LinkSetUp(loLink)
		}

		// Configure container veth
		contLink, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("failed to find container link %q: %w", ifName, err)
		}

		if err := netlink.LinkSetUp(contLink); err != nil {
			return fmt.Errorf("failed to set %q UP: %w", ifName, err)
		}

		// Add IP
		addr := &netlink.Addr{IPNet: podIP}
		if err := netlink.AddrAdd(contLink, addr); err != nil {
			return fmt.Errorf("failed to add address %v to %q: %w", podIP, ifName, err)
		}

		// Add default route via bridge gateway
		_, defaultDst, _ := net.ParseCIDR("0.0.0.0/0")
		route := &netlink.Route{
			LinkIndex: contLink.Attrs().Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Dst:       defaultDst,
			Gw:        gateway,
		}
		if err := netlink.RouteAdd(route); err != nil {
			return fmt.Errorf("failed to add default route via %v: %w", gateway, err)
		}

		return nil
	})

	if err != nil {
		return nil, "", err
	}

	// Configure host-side veth
	hLink, err := netlink.LinkByName(hostIfaceName)
	if err != nil {
		return nil, "", fmt.Errorf("failed to find host veth %q: %w", hostIfaceName, err)
	}

	if err := netlink.LinkSetMaster(hLink, bridge); err != nil {
		return nil, "", fmt.Errorf("failed to attach host veth %q to bridge %q: %w", hostIfaceName, bridge.Name, err)
	}

	if err := netlink.LinkSetUp(hLink); err != nil {
		return nil, "", fmt.Errorf("failed to set host veth %q UP: %w", hostIfaceName, err)
	}

	return hLink, contMAC, nil
}

// TeardownPodVeth cleans up the container interface inside the netns (if present).
// It is strictly idempotent.
func TeardownPodVeth(netnsPath string, ifName string) error {
	if netnsPath == "" {
		return nil
	}

	podNS, err := ns.GetNS(netnsPath)
	if err != nil {
		// If namespace no longer exists, exit cleanly
		return nil
	}
	defer podNS.Close()

	return podNS.Do(func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			// Interface already deleted
			return nil
		}
		return netlink.LinkDel(link)
	})
}

// CheckPodInterface verifies the interface exists in the netns and has the expected IP.
func CheckPodInterface(netnsPath string, ifName string, expectedIP *net.IPNet) error {
	podNS, err := ns.GetNS(netnsPath)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %w", netnsPath, err)
	}
	defer podNS.Close()

	return podNS.Do(func(_ ns.NetNS) error {
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("interface %q not found in netns: %w", ifName, err)
		}

		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("failed to list addrs on %q: %w", ifName, err)
		}

		for _, addr := range addrs {
			if addr.IP.Equal(expectedIP.IP) {
				return nil
			}
		}

		return fmt.Errorf("expected IP %v not found on %q (found: %v)", expectedIP, ifName, addrs)
	})
}

// EnableIPForward ensures net.ipv4.ip_forward is set to 1.
func EnableIPForward() error {
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
}

// EnableBridgeNetfilter enables bridge-nf-call-iptables if available.
func EnableBridgeNetfilter() error {
	path := "/proc/sys/net/bridge/bridge-nf-call-iptables"
	if _, err := os.Stat(path); err == nil {
		return os.WriteFile(path, []byte("1"), 0644)
	}
	return nil
}
