package agent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/sanjeevliv/mycni/pkg/ipam"
)

func TestInstallCNIBinary(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	srcBin := filepath.Join(tmpDir, "dummy-cni")
	if err := os.WriteFile(srcBin, []byte("#!/bin/sh\necho mycni\n"), 0755); err != nil {
		t.Fatalf("failed to write dummy source binary: %v", err)
	}

	dstDir := filepath.Join(tmpDir, "host-opt-cni-bin")
	cfg := Config{
		NodeName:  "test-node",
		CNIBinSrc: srcBin,
		CNIBinDir: dstDir,
	}

	ag, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	if err := ag.InstallCNIBinary(); err != nil {
		t.Fatalf("InstallCNIBinary failed: %v", err)
	}

	installed := filepath.Join(dstDir, "mycni-cni")
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("installed binary not found: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("expected executable permissions on installed binary, got %v", info.Mode())
	}
}

func TestRenderCNIConfig(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	confDir := filepath.Join(tmpDir, "host-etc-cni-netd")
	cfg := Config{
		NodeName:   "worker-1",
		PodCIDR:    "10.244.3.0/24",
		BridgeName: "mycni0",
		MTU:        1450,
		CNIConfDir: confDir,
		CNIDataDir: "/var/lib/cni/mycni",
	}

	ag, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	if err := ag.RenderCNIConfig(); err != nil {
		t.Fatalf("RenderCNIConfig failed: %v", err)
	}

	confFile := filepath.Join(confDir, "10-mycni.conf")
	data, err := os.ReadFile(confFile)
	if err != nil {
		t.Fatalf("failed to read rendered config: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal rendered config: %v", err)
	}

	if parsed["podCIDR"] != "10.244.3.0/24" {
		t.Errorf("expected podCIDR 10.244.3.0/24, got %v", parsed["podCIDR"])
	}
	if parsed["bridge"] != "mycni0" {
		t.Errorf("expected bridge mycni0, got %v", parsed["bridge"])
	}
	if parsed["mtu"] != float64(1450) {
		t.Errorf("expected mtu 1450, got %v", parsed["mtu"])
	}
}

func TestFetchPodCIDR(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mycni-worker",
		},
		Spec: corev1.NodeSpec{
			PodCIDR: "10.244.1.0/24",
		},
	}

	fakeClient := fake.NewSimpleClientset(node)
	cfg := Config{
		NodeName: "mycni-worker",
	}

	ag, err := New(cfg, fakeClient, nil)
	if err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	cid, err := ag.FetchPodCIDR(context.Background())
	if err != nil {
		t.Fatalf("FetchPodCIDR failed: %v", err)
	}
	if cid != "10.244.1.0/24" {
		t.Errorf("expected 10.244.1.0/24, got %s", cid)
	}
}

func TestReconcileLeases(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "agent-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	_, ipnet, _ := net.ParseCIDR("10.244.1.0/24")
	dataDir := filepath.Join(tmpDir, "data")
	ipamInstance, err := ipam.New(dataDir, ipnet)
	if err != nil {
		t.Fatalf("failed to init IPAM: %v", err)
	}

	// Allocate 2 pods
	ip1, _ := ipamInstance.AllocateWithMetadata("cid-active", "eth0", "default", "pod-active")
	_, _ = ipamInstance.AllocateWithMetadata("cid-stale", "eth0", "default", "pod-stale")

	// Fake K8s only has active pod
	activePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-active",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "mycni-worker",
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip1.IP.String(),
			ContainerStatuses: []corev1.ContainerStatus{
				{
					ContainerID: "containerd://cid-active",
				},
			},
		},
	}

	fakeClient := fake.NewSimpleClientset(activePod)
	cfg := Config{
		NodeName:   "mycni-worker",
		CNIDataDir: dataDir,
		PodCIDR:    "10.244.1.0/24",
	}

	ag, err := New(cfg, fakeClient, nil)
	if err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}
	ag.ipam = ipamInstance

	// Backdate the stale lease so it falls outside the 30s grace window
	leases, _ := ipamInstance.ListDetailedLeases()
	for _, l := range leases {
		if l.ContainerID == "cid-stale" {
			leaseFile := filepath.Join(dataDir, "leases", l.IP.String())
			past := time.Now().Add(-5 * time.Minute).Format(time.RFC3339)
			content := "cid-stale\neth0\ndefault\npod-stale\n" + past + "\n"
			_ = os.WriteFile(leaseFile, []byte(content), 0644)
		}
	}

	if err := ag.ReconcileLeases(context.Background()); err != nil {
		t.Fatalf("ReconcileLeases failed: %v", err)
	}

	remaining, _ := ipamInstance.ListLeases()
	if len(remaining) != 1 {
		t.Errorf("expected exactly 1 active lease remaining, got %d", len(remaining))
	}
	if _, ok := remaining[ip1.IP.String()]; !ok {
		t.Errorf("expected active pod lease %s to remain", ip1.IP.String())
	}
}

// Silence unused variable warning
var _ = io.EOF

