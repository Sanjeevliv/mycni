//go:build !linux

package netutil

import (
	"errors"
	"net"

	"github.com/vishvananda/netlink"
)

var errNotImplemented = errors.New("netlink operations are only supported on Linux")

// EnsureBridge stub for non-linux systems.
func EnsureBridge(bridgeName string, gateway *net.IPNet, mtu int) (*netlink.Bridge, error) {
	return nil, errNotImplemented
}

// SetupPodVeth stub for non-linux systems.
func SetupPodVeth(netnsPath string, ifName string, mtu int, bridge *netlink.Bridge, podIP *net.IPNet, gateway net.IP) (netlink.Link, string, error) {
	return nil, "", errNotImplemented
}

// TeardownPodVeth stub for non-linux systems.
func TeardownPodVeth(netnsPath string, ifName string) error {
	return nil
}

// CheckPodInterface stub for non-linux systems.
func CheckPodInterface(netnsPath string, ifName string, expectedIP *net.IPNet) error {
	return errNotImplemented
}

// EnableIPForward stub for non-linux systems.
func EnableIPForward() error {
	return nil
}

// EnableBridgeNetfilter stub for non-linux systems.
func EnableBridgeNetfilter() error {
	return nil
}
