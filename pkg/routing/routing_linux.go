//go:build linux

package routing

import (
	"errors"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// EnsureDirectRoute adds or replaces an L3 route to a peer node's PodCIDR.
func EnsureDirectRoute(linkIndex int, dst *net.IPNet, gw net.IP) error {
	if dst == nil || gw == nil {
		return errors.New("destination CIDR and gateway IP are required")
	}
	return netlink.RouteReplace(&netlink.Route{
		LinkIndex: linkIndex,
		Dst:       dst,
		Gw:        gw.To4(),
		Protocol:  netlink.RouteProtocol(RouteProtocolMyCNI),
		Scope:     netlink.SCOPE_UNIVERSE,
	})
}

// DeleteDirectRoute removes the route for a given destination CIDR.
func DeleteDirectRoute(dst *net.IPNet) error {
	if dst == nil {
		return nil
	}
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("failed to list routes: %w", err)
	}
	for _, r := range routes {
		if int(r.Protocol) == RouteProtocolMyCNI && r.Dst != nil && r.Dst.String() == dst.String() {
			rCopy := r
			_ = netlink.RouteDel(&rCopy)
		}
	}
	return nil
}
