package routing

import (
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExtractNodeInfo(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mycni-worker",
			Annotations: map[string]string{
				AnnotationVtepMAC: "02:42:ac:12:00:03",
			},
		},
		Spec: corev1.NodeSpec{
			PodCIDR: "10.244.1.0/24",
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{
					Type:    corev1.NodeHostName,
					Address: "mycni-worker",
				},
				{
					Type:    corev1.NodeInternalIP,
					Address: "172.18.0.3",
				},
			},
		},
	}

	info, ok := ExtractNodeInfo(node)
	if !ok {
		t.Fatal("expected ExtractNodeInfo to return true")
	}
	if info.Name != "mycni-worker" {
		t.Errorf("expected Name mycni-worker, got %s", info.Name)
	}
	if info.NodeIP.String() != "172.18.0.3" {
		t.Errorf("expected NodeIP 172.18.0.3, got %s", info.NodeIP.String())
	}
	if info.PodCIDR.String() != "10.244.1.0/24" {
		t.Errorf("expected PodCIDR 10.244.1.0/24, got %s", info.PodCIDR.String())
	}
	if info.VtepMAC.String() != "02:42:ac:12:00:03" {
		t.Errorf("expected VtepMAC 02:42:ac:12:00:03, got %s", info.VtepMAC.String())
	}

	// Missing PodCIDR
	noCIDRNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "incomplete-node"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "172.18.0.5"},
			},
		},
	}
	if _, ok := ExtractNodeInfo(noCIDRNode); ok {
		t.Error("expected false for node without PodCIDR")
	}
}

func TestDiffRoutes(t *testing.T) {
	_, cidr1, _ := net.ParseCIDR("10.244.1.0/24")
	_, cidr2, _ := net.ParseCIDR("10.244.2.0/24")
	_, cidr3, _ := net.ParseCIDR("10.244.3.0/24")

	existing := []RouteSpec{
		{DstCIDR: "10.244.1.0/24", GwIP: "172.18.0.3"}, // unchanged
		{DstCIDR: "10.244.2.0/24", GwIP: "172.18.0.9"}, // gateway IP changed
		{DstCIDR: "10.244.9.0/24", GwIP: "172.18.0.99"}, // stale route
	}

	peers := []NodeInfo{
		{Name: "worker1", NodeIP: net.ParseIP("172.18.0.3"), PodCIDR: cidr1},
		{Name: "worker2", NodeIP: net.ParseIP("172.18.0.2"), PodCIDR: cidr2},
		{Name: "worker3", NodeIP: net.ParseIP("172.18.0.4"), PodCIDR: cidr3},
	}

	toUpsert, toDelete := DiffRoutes(existing, peers)

	if len(toUpsert) != 2 {
		t.Fatalf("expected 2 routes to upsert (worker2 updated, worker3 new), got %d", len(toUpsert))
	}
	if toUpsert[0].Name != "worker2" || toUpsert[1].Name != "worker3" {
		t.Errorf("unexpected upsert peers: %+v", toUpsert)
	}

	if len(toDelete) != 1 || toDelete[0] != "10.244.9.0/24" {
		t.Errorf("expected stale CIDR 10.244.9.0/24 to be deleted, got %v", toDelete)
	}
}
