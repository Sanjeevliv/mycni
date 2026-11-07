//go:build !linux

package direct

import (
	"net"

	"github.com/sanjeevliv/mycni/pkg/routing"
)

type stubDataplane struct {
	routes map[string]string
}

func newDefaultDataplane() Dataplane {
	return &stubDataplane{
		routes: make(map[string]string),
	}
}

func (d *stubDataplane) FindHostLinkIndex(nodeIP net.IP) (int, error) {
	return 1, nil
}

func (d *stubDataplane) RouteReplace(linkIndex int, dst *net.IPNet, gw net.IP, protocol int) error {
	d.routes[dst.String()] = gw.String()
	return nil
}

func (d *stubDataplane) RouteDel(dst *net.IPNet, protocol int) error {
	delete(d.routes, dst.String())
	return nil
}

func (d *stubDataplane) ListRoutesByProtocol(protocol int) ([]routing.RouteSpec, error) {
	var specs []routing.RouteSpec
	for dst, gw := range d.routes {
		specs = append(specs, routing.RouteSpec{
			DstCIDR: dst,
			GwIP:    gw,
		})
	}
	return specs, nil
}
