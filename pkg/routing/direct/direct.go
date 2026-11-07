package direct

import (
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/sanjeevliv/mycni/pkg/routing"
)

// Dataplane abstracts kernel netlink route operations so the direct routing
// backend can be unit-tested on any OS without root privileges.
type Dataplane interface {
	FindHostLinkIndex(nodeIP net.IP) (int, error)
	RouteReplace(linkIndex int, dst *net.IPNet, gw net.IP, protocol int) error
	RouteDel(dst *net.IPNet, protocol int) error
	ListRoutesByProtocol(protocol int) ([]routing.RouteSpec, error)
}

// Backend implements routing.Backend using direct L3 host routes (proto 77).
type Backend struct {
	mu            sync.Mutex
	local         routing.NodeInfo
	hostLinkIndex int
	dp            Dataplane
	logger        *slog.Logger
}

// New creates a new Direct Routing backend.
func New(logger *slog.Logger) *Backend {
	if logger == nil {
		logger = slog.Default()
	}
	return &Backend{
		dp:     newDefaultDataplane(),
		logger: logger,
	}
}

// NewWithDataplane creates a Direct Routing backend with a custom Dataplane (for testing).
func NewWithDataplane(dp Dataplane, logger *slog.Logger) *Backend {
	if logger == nil {
		logger = slog.Default()
	}
	return &Backend{
		dp:     dp,
		logger: logger,
	}
}

// Name returns the backend identifier.
func (b *Backend) Name() string {
	return "direct"
}

// Init initializes the direct routing backend with the local node's information.
func (b *Backend) Init(local routing.NodeInfo) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if local.NodeIP == nil {
		return fmt.Errorf("local node IP cannot be nil")
	}
	if local.PodCIDR == nil {
		return fmt.Errorf("local podCIDR cannot be nil")
	}

	linkIdx, err := b.dp.FindHostLinkIndex(local.NodeIP)
	if err != nil {
		return fmt.Errorf("failed to find host network interface for node IP %s: %w", local.NodeIP, err)
	}

	b.local = local
	b.hostLinkIndex = linkIdx

	b.logger.Info("initialized direct routing backend",
		"node", local.Name,
		"nodeIP", local.NodeIP.String(),
		"podCIDR", local.PodCIDR.String(),
		"hostLinkIndex", linkIdx,
	)
	return nil
}

// AddPeer programs an idempotent host route:
// ip route replace <peerPodCIDR> via <peerNodeIP> dev <hostIface> proto 77
func (b *Backend) AddPeer(peer routing.NodeInfo) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if peer.PodCIDR == nil || peer.NodeIP == nil {
		return fmt.Errorf("invalid peer NodeInfo %+v: missing PodCIDR or NodeIP", peer)
	}

	// Skip self
	if b.isLocalNode(peer) {
		return nil
	}

	if err := b.dp.RouteReplace(b.hostLinkIndex, peer.PodCIDR, peer.NodeIP, routing.RouteProtocolMyCNI); err != nil {
		return fmt.Errorf("failed to replace route for peer %s (%s via %s): %w",
			peer.Name, peer.PodCIDR.String(), peer.NodeIP.String(), err)
	}

	b.logger.Info("programmed direct peer route",
		"peer", peer.Name,
		"dst", peer.PodCIDR.String(),
		"via", peer.NodeIP.String(),
		"proto", routing.RouteProtocolMyCNI,
	)
	return nil
}

// RemovePeer deletes the kernel route for the given peer node's PodCIDR.
func (b *Backend) RemovePeer(peer routing.NodeInfo) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if peer.PodCIDR == nil {
		return nil
	}

	if b.isLocalNode(peer) {
		return nil
	}

	if err := b.dp.RouteDel(peer.PodCIDR, routing.RouteProtocolMyCNI); err != nil {
		return fmt.Errorf("failed to delete route for peer %s (%s): %w",
			peer.Name, peer.PodCIDR.String(), err)
	}

	b.logger.Info("removed direct peer route",
		"peer", peer.Name,
		"dst", peer.PodCIDR.String(),
		"proto", routing.RouteProtocolMyCNI,
	)
	return nil
}

// Reconcile diffs kernel routes tagged with proto 77 against the live peer list,
// adding/updating missing routes and pruning stale ones.
func (b *Backend) Reconcile(peers []routing.NodeInfo) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	var remotePeers []routing.NodeInfo
	for _, peer := range peers {
		if b.isLocalNode(peer) {
			continue
		}
		if peer.PodCIDR != nil && peer.NodeIP != nil {
			remotePeers = append(remotePeers, peer)
		}
	}

	existing, err := b.dp.ListRoutesByProtocol(routing.RouteProtocolMyCNI)
	if err != nil {
		return fmt.Errorf("failed to list proto %d kernel routes: %w", routing.RouteProtocolMyCNI, err)
	}

	toUpsert, toDelete := routing.DiffRoutes(existing, remotePeers)

	var firstErr error
	for _, peer := range toUpsert {
		if err := b.dp.RouteReplace(b.hostLinkIndex, peer.PodCIDR, peer.NodeIP, routing.RouteProtocolMyCNI); err != nil {
			b.logger.Error("reconcile: failed to upsert route",
				"peer", peer.Name,
				"dst", peer.PodCIDR.String(),
				"via", peer.NodeIP.String(),
				"error", err,
			)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			b.logger.Info("reconcile: upserted peer route",
				"peer", peer.Name,
				"dst", peer.PodCIDR.String(),
				"via", peer.NodeIP.String(),
			)
		}
	}

	for _, cidrStr := range toDelete {
		_, dstNet, err := net.ParseCIDR(cidrStr)
		if err != nil || dstNet == nil {
			continue
		}
		if err := b.dp.RouteDel(dstNet, routing.RouteProtocolMyCNI); err != nil {
			b.logger.Error("reconcile: failed to delete stale route",
				"dst", cidrStr,
				"error", err,
			)
			if firstErr == nil {
				firstErr = err
			}
		} else {
			b.logger.Info("reconcile: pruned stale route",
				"dst", cidrStr,
				"proto", routing.RouteProtocolMyCNI,
			)
		}
	}

	return firstErr
}

func (b *Backend) isLocalNode(peer routing.NodeInfo) bool {
	if b.local.Name != "" && peer.Name == b.local.Name {
		return true
	}
	if b.local.NodeIP != nil && peer.NodeIP != nil && b.local.NodeIP.Equal(peer.NodeIP) {
		return true
	}
	if b.local.PodCIDR != nil && peer.PodCIDR != nil && b.local.PodCIDR.String() == peer.PodCIDR.String() {
		return true
	}
	return false
}
