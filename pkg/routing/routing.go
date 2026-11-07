package routing

import (
	"net"

	corev1 "k8s.io/api/core/v1"
)

const (
	// RouteProtocolMyCNI is the custom Linux kernel route protocol number (proto 77)
	// used to tag all routes programmed by MyCNI so reconciliation only touches MyCNI-owned routes.
	RouteProtocolMyCNI = 77

	// AnnotationVtepMAC is the Node annotation key used by VXLAN mode (M4).
	AnnotationVtepMAC = "mycni.io/vtep-mac"
)

// NodeInfo holds networking metadata for a cluster node.
type NodeInfo struct {
	Name    string
	NodeIP  net.IP
	PodCIDR *net.IPNet
	VtepMAC net.HardwareAddr
}

// Backend defines the cross-node routing abstraction implemented by direct (M3) and vxlan (M4).
type Backend interface {
	Name() string
	Init(local NodeInfo) error
	AddPeer(peer NodeInfo) error
	RemovePeer(peer NodeInfo) error
	Reconcile(peers []NodeInfo) error
}

// RouteSpec represents a simplified kernel route entry owned by MyCNI.
type RouteSpec struct {
	DstCIDR string
	GwIP    string
}

// ExtractNodeInfo extracts NodeInfo from a Kubernetes v1.Node object.
// Returns (info, true) if the node has both a valid IPv4 InternalIP and PodCIDR.
func ExtractNodeInfo(node *corev1.Node) (NodeInfo, bool) {
	if node == nil {
		return NodeInfo{}, false
	}

	cidrStr := node.Spec.PodCIDR
	if cidrStr == "" && len(node.Spec.PodCIDRs) > 0 {
		cidrStr = node.Spec.PodCIDRs[0]
	}
	if cidrStr == "" {
		return NodeInfo{}, false
	}

	_, podCIDR, err := net.ParseCIDR(cidrStr)
	if err != nil || podCIDR == nil || podCIDR.IP.To4() == nil {
		return NodeInfo{}, false
	}
	// Normalize IP to network address
	podCIDR.IP = podCIDR.IP.Mask(podCIDR.Mask).To4()

	var nodeIP net.IP
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			parsed := net.ParseIP(addr.Address)
			if parsed != nil && parsed.To4() != nil {
				nodeIP = parsed.To4()
				break
			}
		}
	}
	if nodeIP == nil {
		return NodeInfo{}, false
	}

	var vtepMAC net.HardwareAddr
	if node.Annotations != nil {
		if macStr, ok := node.Annotations[AnnotationVtepMAC]; ok && macStr != "" {
			if mac, err := net.ParseMAC(macStr); err == nil {
				vtepMAC = mac
			}
		}
	}

	return NodeInfo{
		Name:    node.Name,
		NodeIP:  nodeIP,
		PodCIDR: podCIDR,
		VtepMAC: vtepMAC,
	}, true
}

// DiffRoutes compares existing proto-77 kernel routes against the desired peer NodeInfo list.
// It returns peers that need to be added/updated and destination CIDRs that should be pruned.
func DiffRoutes(existing []RouteSpec, peers []NodeInfo) (toUpsert []NodeInfo, toDelete []string) {
	existingMap := make(map[string]string, len(existing))
	for _, r := range existing {
		existingMap[r.DstCIDR] = r.GwIP
	}

	desiredMap := make(map[string]NodeInfo, len(peers))
	for _, peer := range peers {
		if peer.PodCIDR == nil || peer.NodeIP == nil {
			continue
		}
		dst := peer.PodCIDR.String()
		desiredMap[dst] = peer

		if gw, exists := existingMap[dst]; !exists || gw != peer.NodeIP.String() {
			toUpsert = append(toUpsert, peer)
		}
	}

	for _, r := range existing {
		if _, wanted := desiredMap[r.DstCIDR]; !wanted {
			toDelete = append(toDelete, r.DstCIDR)
		}
	}

	return toUpsert, toDelete
}
