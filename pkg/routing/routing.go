package routing

import (
	"net"

	corev1 "k8s.io/api/core/v1"
)

const RouteProtocolMyCNI = 77

// NodeInfo holds networking metadata for a cluster node.
type NodeInfo struct {
	Name    string
	NodeIP  net.IP
	PodCIDR *net.IPNet
}

// ExtractNodeInfo extracts NodeInfo from a Kubernetes v1.Node.
func ExtractNodeInfo(node *corev1.Node) (NodeInfo, bool) {
	if node == nil || node.Spec.PodCIDR == "" {
		return NodeInfo{}, false
	}
	_, podCIDR, err := net.ParseCIDR(node.Spec.PodCIDR)
	if err != nil || podCIDR == nil {
		return NodeInfo{}, false
	}
	podCIDR.IP = podCIDR.IP.Mask(podCIDR.Mask).To4()

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			if ip := net.ParseIP(addr.Address); ip != nil && ip.To4() != nil {
				return NodeInfo{Name: node.Name, NodeIP: ip.To4(), PodCIDR: podCIDR}, true
			}
		}
	}
	return NodeInfo{}, false
}
