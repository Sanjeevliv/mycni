package direct

import (
	"net"
	"sync"
	"testing"

	"github.com/sanjeevliv/mycni/pkg/routing"
)

type mockDataplane struct {
	mu     sync.Mutex
	routes map[string]string
}

func newMockDataplane() *mockDataplane {
	return &mockDataplane{
		routes: make(map[string]string),
	}
}

func (m *mockDataplane) FindHostLinkIndex(nodeIP net.IP) (int, error) {
	return 2, nil
}

func (m *mockDataplane) RouteReplace(linkIndex int, dst *net.IPNet, gw net.IP, protocol int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes[dst.String()] = gw.String()
	return nil
}

func (m *mockDataplane) RouteDel(dst *net.IPNet, protocol int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.routes, dst.String())
	return nil
}

func (m *mockDataplane) ListRoutesByProtocol(protocol int) ([]routing.RouteSpec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var specs []routing.RouteSpec
	for dst, gw := range m.routes {
		specs = append(specs, routing.RouteSpec{
			DstCIDR: dst,
			GwIP:    gw,
		})
	}
	return specs, nil
}

func mustCIDR(s string) *net.IPNet {
	_, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return ipnet
}

func TestDirectBackendLifecycle(t *testing.T) {
	dp := newMockDataplane()
	b := NewWithDataplane(dp, nil)

	if b.Name() != "direct" {
		t.Fatalf("expected name direct, got %s", b.Name())
	}

	local := routing.NodeInfo{
		Name:    "mycni-worker",
		NodeIP:  net.ParseIP("172.18.0.3"),
		PodCIDR: mustCIDR("10.244.1.0/24"),
	}
	if err := b.Init(local); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Adding local node should be a no-op
	if err := b.AddPeer(local); err != nil {
		t.Fatalf("AddPeer(local) failed: %v", err)
	}
	if len(dp.routes) != 0 {
		t.Fatalf("expected 0 routes after adding self, got %d", len(dp.routes))
	}

	// Add peer 1 (control-plane) and peer 2 (worker2)
	peer1 := routing.NodeInfo{
		Name:    "mycni-control-plane",
		NodeIP:  net.ParseIP("172.18.0.4"),
		PodCIDR: mustCIDR("10.244.0.0/24"),
	}
	peer2 := routing.NodeInfo{
		Name:    "mycni-worker2",
		NodeIP:  net.ParseIP("172.18.0.2"),
		PodCIDR: mustCIDR("10.244.2.0/24"),
	}

	if err := b.AddPeer(peer1); err != nil {
		t.Fatalf("AddPeer(peer1) failed: %v", err)
	}
	if err := b.AddPeer(peer2); err != nil {
		t.Fatalf("AddPeer(peer2) failed: %v", err)
	}

	if len(dp.routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(dp.routes))
	}
	if dp.routes["10.244.0.0/24"] != "172.18.0.4" {
		t.Errorf("unexpected route for 10.244.0.0/24: %s", dp.routes["10.244.0.0/24"])
	}
	if dp.routes["10.244.2.0/24"] != "172.18.0.2" {
		t.Errorf("unexpected route for 10.244.2.0/24: %s", dp.routes["10.244.2.0/24"])
	}

	// Remove peer2
	if err := b.RemovePeer(peer2); err != nil {
		t.Fatalf("RemovePeer(peer2) failed: %v", err)
	}
	if _, exists := dp.routes["10.244.2.0/24"]; exists {
		t.Error("expected 10.244.2.0/24 route to be removed")
	}

	// Inject a stale route into kernel and reconcile with [local, peer1, peer2]
	dp.routes["10.244.99.0/24"] = "172.18.0.99"

	if err := b.Reconcile([]routing.NodeInfo{local, peer1, peer2}); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if _, exists := dp.routes["10.244.99.0/24"]; exists {
		t.Error("expected stale route 10.244.99.0/24 to be pruned by Reconcile")
	}
	if dp.routes["10.244.2.0/24"] != "172.18.0.2" {
		t.Error("expected missing peer2 route 10.244.2.0/24 to be restored by Reconcile")
	}
}
