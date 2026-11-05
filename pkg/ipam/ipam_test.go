package ipam

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
)

func setupTestIPAM(t *testing.T, cidrStr string) (*IPAM, string) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "mycni-ipam-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	_, ipnet, err := net.ParseCIDR(cidrStr)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("invalid CIDR %q: %v", cidrStr, err)
	}

	ipam, err := New(tmpDir, ipnet)
	if err != nil {
		os.RemoveAll(tmpDir)
		t.Fatalf("failed to create IPAM: %v", err)
	}

	return ipam, tmpDir
}

func TestIPAMAllocationAndGateway(t *testing.T) {
	ipam, tmpDir := setupTestIPAM(t, "10.244.1.0/24")
	defer os.RemoveAll(tmpDir)

	gw := ipam.GetGatewayIP()
	if gw.String() != "10.244.1.1" {
		t.Errorf("expected gateway 10.244.1.1, got %s", gw.String())
	}

	// First allocation should be .2
	ip1, err := ipam.Allocate("container-1", "eth0")
	if err != nil {
		t.Fatalf("failed to allocate ip1: %v", err)
	}
	if ip1.IP.String() != "10.244.1.2" {
		t.Errorf("expected 10.244.1.2, got %s", ip1.IP.String())
	}

	// Second allocation should be .3
	ip2, err := ipam.Allocate("container-2", "eth0")
	if err != nil {
		t.Fatalf("failed to allocate ip2: %v", err)
	}
	if ip2.IP.String() != "10.244.1.3" {
		t.Errorf("expected 10.244.1.3, got %s", ip2.IP.String())
	}

	// Idempotency: re-allocating container-1 should return 10.244.1.2
	ip1Repeat, err := ipam.Allocate("container-1", "eth0")
	if err != nil {
		t.Fatalf("failed repeat allocate: %v", err)
	}
	if ip1Repeat.IP.String() != ip1.IP.String() {
		t.Errorf("expected idempotent IP %s, got %s", ip1.IP.String(), ip1Repeat.IP.String())
	}
}

func TestIPAMReleaseAndReuse(t *testing.T) {
	ipam, tmpDir := setupTestIPAM(t, "10.244.1.0/24")
	defer os.RemoveAll(tmpDir)

	ip1, err := ipam.Allocate("container-1", "eth0")
	if err != nil {
		t.Fatalf("failed to allocate: %v", err)
	}
	if ip1.IP.String() != "10.244.1.2" {
		t.Errorf("expected 10.244.1.2, got %s", ip1.IP.String())
	}

	// Release container-1
	if err := ipam.Release("container-1", "eth0"); err != nil {
		t.Fatalf("failed to release: %v", err)
	}

	// Release again must be idempotent
	if err := ipam.Release("container-1", "eth0"); err != nil {
		t.Fatalf("idempotent release failed: %v", err)
	}

	// Re-allocate should reuse 10.244.1.2
	ipNew, err := ipam.Allocate("container-new", "eth0")
	if err != nil {
		t.Fatalf("failed to allocate new container: %v", err)
	}
	if ipNew.IP.String() != "10.244.1.2" {
		t.Errorf("expected reused IP 10.244.1.2, got %s", ipNew.IP.String())
	}
}

func TestIPAMPoolExhaustion(t *testing.T) {
	// A /29 subnet: 8 total IPs (.0 to .7)
	// .0 network, .1 gateway
	// usable: .2, .3, .4, .5, .6 (5 usable IPs)
	// .7 broadcast
	ipam, tmpDir := setupTestIPAM(t, "10.244.1.0/29")
	defer os.RemoveAll(tmpDir)

	for i := 1; i <= 5; i++ {
		cid := fmt.Sprintf("cid-%d", i)
		ip, err := ipam.Allocate(cid, "eth0")
		if err != nil {
			t.Fatalf("failed to allocate for %s: %v", cid, err)
		}
		expectedIP := fmt.Sprintf("10.244.1.%d", i+1)
		if ip.IP.String() != expectedIP {
			t.Errorf("expected %s, got %s", expectedIP, ip.IP.String())
		}
	}

	// 6th allocation must fail with ErrPoolExhausted
	_, err := ipam.Allocate("cid-overflow", "eth0")
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted, got %v", err)
	}

	// Free one IP and allocate again
	if err := ipam.Release("cid-3", "eth0"); err != nil {
		t.Fatalf("failed release: %v", err)
	}

	ipFreed, err := ipam.Allocate("cid-overflow", "eth0")
	if err != nil {
		t.Fatalf("expected successful allocation after release: %v", err)
	}
	if ipFreed.IP.String() != "10.244.1.4" {
		t.Errorf("expected 10.244.1.4, got %s", ipFreed.IP.String())
	}
}

func TestIPAMConcurrentAllocations(t *testing.T) {
	ipam, tmpDir := setupTestIPAM(t, "10.244.1.0/24")
	defer os.RemoveAll(tmpDir)

	const concurrency = 30
	var wg sync.WaitGroup
	errCh := make(chan error, concurrency)
	ipMap := sync.Map{}

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cid := fmt.Sprintf("concur-cid-%02d", id)
			ipNet, err := ipam.Allocate(cid, "eth0")
			if err != nil {
				errCh <- fmt.Errorf("cid %s error: %w", cid, err)
				return
			}
			ipStr := ipNet.IP.String()
			if _, exists := ipMap.LoadOrStore(ipStr, cid); exists {
				errCh <- fmt.Errorf("duplicate IP allocated: %s for %s", ipStr, cid)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrency error: %v", err)
	}

	leases, err := ipam.ListLeases()
	if err != nil {
		t.Fatalf("failed to list leases: %v", err)
	}
	if len(leases) != concurrency {
		t.Errorf("expected %d leases, got %d", concurrency, len(leases))
	}
}

func TestIPAMBackwardCompatibility(t *testing.T) {
	ipam, tmpDir := setupTestIPAM(t, "10.244.1.0/24")
	defer os.RemoveAll(tmpDir)

	// Simulate an older M1 lease file with only containerID and ifName
	legacyLeasePath := fmt.Sprintf("%s/leases/10.244.1.2", tmpDir)
	legacyContent := "legacy-container-123\neth0\n"
	if err := os.WriteFile(legacyLeasePath, []byte(legacyContent), 0644); err != nil {
		t.Fatalf("failed to write legacy lease: %v", err)
	}

	// Verify GetLease reads it properly
	ipNet, err := ipam.GetLease("legacy-container-123")
	if err != nil {
		t.Fatalf("failed to get legacy lease: %v", err)
	}
	if ipNet.IP.String() != "10.244.1.2" {
		t.Errorf("expected 10.244.1.2, got %s", ipNet.IP.String())
	}

	// Verify next allocation skips 10.244.1.2 and gets 10.244.1.3
	newIP, err := ipam.AllocateWithMetadata("new-container-456", "eth0", "default", "pod-x")
	if err != nil {
		t.Fatalf("failed to allocate new IP: %v", err)
	}
	if newIP.IP.String() != "10.244.1.3" {
		t.Errorf("expected 10.244.1.3, got %s", newIP.IP.String())
	}

	// Verify DetailedLeases returns both
	detailed, err := ipam.ListDetailedLeases()
	if err != nil {
		t.Fatalf("failed to list detailed leases: %v", err)
	}
	if len(detailed) != 2 {
		t.Fatalf("expected 2 detailed leases, got %d", len(detailed))
	}
}

func TestIPAMGarbageCollection(t *testing.T) {
	ipam, tmpDir := setupTestIPAM(t, "10.244.1.0/24")
	defer os.RemoveAll(tmpDir)

	// Allocate 3 containers
	ip1, err := ipam.AllocateWithMetadata("live-cid-1", "eth0", "default", "live-pod-1")
	if err != nil {
		t.Fatalf("failed alloc 1: %v", err)
	}
	ip2, err := ipam.AllocateWithMetadata("live-cid-2", "eth0", "default", "live-pod-2")
	if err != nil {
		t.Fatalf("failed alloc 2: %v", err)
	}
	ip3, err := ipam.AllocateWithMetadata("crashed-cid-3", "eth0", "default", "dead-pod-3")
	if err != nil {
		t.Fatalf("failed alloc 3: %v", err)
	}

	activePodIPs := map[string]bool{
		ip1.IP.String(): true,
	}
	activeContainerIDs := map[string]bool{
		"live-cid-2": true,
	}

	// Run GC with no grace period
	reclaimed, err := ipam.GC(activePodIPs, activeContainerIDs, 0)
	if err != nil {
		t.Fatalf("GC failed: %v", err)
	}

	if len(reclaimed) != 1 {
		t.Fatalf("expected 1 reclaimed IP, got %d (%v)", len(reclaimed), reclaimed)
	}
	if reclaimed[0] != ip3.IP.String() {
		t.Errorf("expected %s reclaimed, got %s", ip3.IP.String(), reclaimed[0])
	}

	// Verify crashed container lease is gone
	_, err = ipam.GetLease("crashed-cid-3")
	if !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("expected ErrLeaseNotFound for dead container, got %v", err)
	}

	// Live pods still retain their leases
	l1, err := ipam.GetLease("live-cid-1")
	if err != nil || l1.IP.String() != ip1.IP.String() {
		t.Errorf("live-cid-1 lease lost or corrupted")
	}
	l2, err := ipam.GetLease("live-cid-2")
	if err != nil || l2.IP.String() != ip2.IP.String() {
		t.Errorf("live-cid-2 lease lost or corrupted")
	}

	// Test direct ReleaseIP
	if err := ipam.ReleaseIP(ip2.IP); err != nil {
		t.Fatalf("ReleaseIP failed: %v", err)
	}
	_, err = ipam.GetLease("live-cid-2")
	if !errors.Is(err, ErrLeaseNotFound) {
		t.Errorf("expected ErrLeaseNotFound after ReleaseIP, got %v", err)
	}
}
