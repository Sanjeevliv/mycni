//go:build linux

package direct

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"

	"github.com/sanjeevliv/mycni/pkg/routing"
)

type linuxDataplane struct{}

func newDefaultDataplane() Dataplane {
	return &linuxDataplane{}
}

func (d *linuxDataplane) FindHostLinkIndex(nodeIP net.IP) (int, error) {
	if nodeIP != nil {
		links, err := netlink.LinkList()
		if err == nil {
			for _, link := range links {
				addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
				if err != nil {
					continue
				}
				for _, addr := range addrs {
					if addr.IP != nil && addr.IP.Equal(nodeIP) {
						return link.Attrs().Index, nil
					}
				}
			}
		}
	}

	// Fallback to eth0
	eth0, err := netlink.LinkByName("eth0")
	if err != nil {
		return 0, fmt.Errorf("could not find interface with IP %v or fallback eth0: %w", nodeIP, err)
	}
	return eth0.Attrs().Index, nil
}

func (d *linuxDataplane) RouteReplace(linkIndex int, dst *net.IPNet, gw net.IP, protocol int) error {
	route := &netlink.Route{
		LinkIndex: linkIndex,
		Dst:       dst,
		Gw:        gw.To4(),
		Protocol:  netlink.RouteProtocol(protocol),
		Scope:     netlink.SCOPE_UNIVERSE,
	}
	return netlink.RouteReplace(route)
}

func (d *linuxDataplane) RouteDel(dst *net.IPNet, protocol int) error {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("failed to list routes for deletion: %w", err)
	}

	targetCIDR := dst.String()
	for _, r := range routes {
		if int(r.Protocol) == protocol && r.Dst != nil && r.Dst.String() == targetCIDR {
			rCopy := r
			if err := netlink.RouteDel(&rCopy); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		}
	}
	return nil
}

func (d *linuxDataplane) ListRoutesByProtocol(protocol int) ([]routing.RouteSpec, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("failed to list IPv4 routes: %w", err)
	}

	var specs []routing.RouteSpec
	for _, r := range routes {
		if int(r.Protocol) == protocol && r.Dst != nil {
			gwStr := ""
			if r.Gw != nil {
				gwStr = r.Gw.String()
			}
			specs = append(specs, routing.RouteSpec{
				DstCIDR: r.Dst.String(),
				GwIP:    gwStr,
			})
		}
	}
	return specs, nil
}
