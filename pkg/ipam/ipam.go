package ipam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var (
	// ErrPoolExhausted is returned when all allocatable IPs in the CIDR are in use.
	ErrPoolExhausted = errors.New("IPAM pool exhausted: no available IP addresses")
	// ErrLeaseNotFound is returned when looking up a lease that does not exist.
	ErrLeaseNotFound = errors.New("IPAM lease not found")
)

// Lease represents an allocated IP lease and its metadata.
type Lease struct {
	IP           net.IP    `json:"ip"`
	ContainerID  string    `json:"containerID"`
	IfName       string    `json:"ifName"`
	PodNamespace string    `json:"podNamespace,omitempty"`
	PodName      string    `json:"podName,omitempty"`
	AllocatedAt  time.Time `json:"allocatedAt,omitempty"`
}

// IPAM manages IP address allocation and persistence per node.
type IPAM struct {
	dataDir  string
	podCIDR  *net.IPNet
	lockPath string
	leaseDir string
}

// New creates an IPAM instance for the given data directory and pod CIDR.
func New(dataDir string, podCIDR *net.IPNet) (*IPAM, error) {
	if podCIDR == nil || podCIDR.IP == nil {
		return nil, fmt.Errorf("invalid pod CIDR")
	}

	ipv4 := podCIDR.IP.To4()
	if ipv4 == nil {
		return nil, fmt.Errorf("only IPv4 CIDRs are currently supported, got %v", podCIDR)
	}

	leaseDir := filepath.Join(dataDir, "leases")
	if err := os.MkdirAll(leaseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create IPAM lease directory %q: %w", leaseDir, err)
	}

	return &IPAM{
		dataDir:  dataDir,
		podCIDR:  podCIDR,
		lockPath: filepath.Join(dataDir, "ipam.lock"),
		leaseDir: leaseDir,
	}, nil
}

// withLock executes fn while holding an exclusive flock on the lock file.
func (ipam *IPAM) withLock(fn func() error) error {
	lockFile, err := os.OpenFile(ipam.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("failed to open lock file %q: %w", ipam.lockPath, err)
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("failed to acquire flock on %q: %w", ipam.lockPath, err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	return fn()
}

// GetGatewayIP returns the default gateway IP (.1 in the subnet).
func (ipam *IPAM) GetGatewayIP() net.IP {
	netUint, _ := cidrBounds(ipam.podCIDR)
	return uint32ToIP(netUint + 1)
}

// GatewayIPNet returns the gateway IP with the subnet mask.
func (ipam *IPAM) GatewayIPNet() *net.IPNet {
	return &net.IPNet{
		IP:   ipam.GetGatewayIP(),
		Mask: ipam.podCIDR.Mask,
	}
}

// Allocate returns an IP address for the container.
// It is idempotent: if containerID already has a lease, that lease is returned.
func (ipam *IPAM) Allocate(containerID, ifName string) (*net.IPNet, error) {
	return ipam.AllocateWithMetadata(containerID, ifName, "", "")
}

// AllocateWithMetadata returns an IP address for the container along with pod metadata.
func (ipam *IPAM) AllocateWithMetadata(containerID, ifName, podNamespace, podName string) (*net.IPNet, error) {
	if containerID == "" {
		return nil, fmt.Errorf("containerID cannot be empty")
	}

	var allocatedIP net.IP

	err := ipam.withLock(func() error {
		// 1. Check if containerID already has an allocated IP
		existingIP, err := ipam.findContainerLease(containerID)
		if err == nil && existingIP != nil {
			allocatedIP = existingIP
			return nil
		}

		// 2. Iterate through usable IPs
		netUint, bcastUint := cidrBounds(ipam.podCIDR)
		startIP := netUint + 2 // .0 is network, .1 is bridge gateway
		endIP := bcastUint - 1 // .255 is broadcast

		if startIP > endIP {
			return fmt.Errorf("CIDR %v too small for allocation", ipam.podCIDR)
		}

		now := time.Now().UTC().Format(time.RFC3339)
		for cur := startIP; cur <= endIP; cur++ {
			candidateIP := uint32ToIP(cur)
			leasePath := filepath.Join(ipam.leaseDir, candidateIP.String())

			if _, err := os.Stat(leasePath); os.IsNotExist(err) {
				// Free IP found, claim it
				content := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n",
					containerID,
					ifName,
					podNamespace,
					podName,
					now,
				)
				if err := os.WriteFile(leasePath, []byte(content), 0644); err != nil {
					return fmt.Errorf("failed to write lease file %q: %w", leasePath, err)
				}
				allocatedIP = candidateIP
				return nil
			}
		}

		return ErrPoolExhausted
	})

	if err != nil {
		return nil, err
	}

	return &net.IPNet{
		IP:   allocatedIP,
		Mask: ipam.podCIDR.Mask,
	}, nil
}

// Release frees any IP lease associated with containerID.
// It is idempotent: if no lease exists, it returns nil.
func (ipam *IPAM) Release(containerID, ifName string) error {
	if containerID == "" {
		return nil
	}

	return ipam.withLock(func() error {
		entries, err := os.ReadDir(ipam.leaseDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}

			leasePath := filepath.Join(ipam.leaseDir, entry.Name())
			data, err := os.ReadFile(leasePath)
			if err != nil {
				continue
			}

			lease := parseLeaseData(net.ParseIP(entry.Name()), data)
			if lease.ContainerID == containerID {
				_ = os.Remove(leasePath)
			}
		}

		return nil
	})
}

// ReleaseIP frees a specific IP lease directly.
func (ipam *IPAM) ReleaseIP(ip net.IP) error {
	if ip == nil {
		return nil
	}

	return ipam.withLock(func() error {
		leasePath := filepath.Join(ipam.leaseDir, ip.String())
		if err := os.Remove(leasePath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}

// GetLease retrieves the leased IP for a given containerID.
func (ipam *IPAM) GetLease(containerID string) (*net.IPNet, error) {
	var leasedIP net.IP
	err := ipam.withLock(func() error {
		ip, err := ipam.findContainerLease(containerID)
		if err != nil {
			return err
		}
		leasedIP = ip
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &net.IPNet{
		IP:   leasedIP,
		Mask: ipam.podCIDR.Mask,
	}, nil
}

// ListLeases returns all active leases mapped by IP string to containerID.
func (ipam *IPAM) ListLeases() (map[string]string, error) {
	leases := make(map[string]string)
	err := ipam.withLock(func() error {
		entries, err := os.ReadDir(ipam.leaseDir)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(ipam.leaseDir, entry.Name()))
			if err != nil {
				continue
			}
			lease := parseLeaseData(net.ParseIP(entry.Name()), data)
			if lease.ContainerID != "" {
				leases[entry.Name()] = lease.ContainerID
			}
		}
		return nil
	})
	return leases, err
}

// ListDetailedLeases returns all active leases with their parsed metadata.
func (ipam *IPAM) ListDetailedLeases() ([]Lease, error) {
	var leases []Lease
	err := ipam.withLock(func() error {
		entries, err := os.ReadDir(ipam.leaseDir)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			ip := net.ParseIP(entry.Name())
			if ip == nil {
				continue
			}

			data, err := os.ReadFile(filepath.Join(ipam.leaseDir, entry.Name()))
			if err != nil {
				continue
			}
			lease := parseLeaseData(ip, data)
			leases = append(leases, lease)
		}
		return nil
	})
	return leases, err
}

// GC reconciles stored IPAM lease files against active pods on the node.
// It removes any lease whose IP is not in activePodIPs AND whose ContainerID is not in activeContainerIDs.
// If gracePeriod > 0, leases created within the grace window are spared to allow pod startups to complete.
func (ipam *IPAM) GC(activePodIPs map[string]bool, activeContainerIDs map[string]bool, gracePeriod time.Duration) ([]string, error) {
	var reclaimed []string
	err := ipam.withLock(func() error {
		entries, err := os.ReadDir(ipam.leaseDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}

		now := time.Now()
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}

			ipStr := entry.Name()
			ip := net.ParseIP(ipStr)
			if ip == nil {
				continue
			}

			leasePath := filepath.Join(ipam.leaseDir, ipStr)
			data, err := os.ReadFile(leasePath)
			if err != nil {
				continue
			}

			lease := parseLeaseData(ip, data)

			// Honor grace period for freshly allocated leases
			if gracePeriod > 0 && !lease.AllocatedAt.IsZero() {
				if now.Sub(lease.AllocatedAt) < gracePeriod {
					continue
				}
			}

			isActive := false
			if activePodIPs != nil && activePodIPs[ipStr] {
				isActive = true
			}
			if activeContainerIDs != nil && activeContainerIDs[lease.ContainerID] {
				isActive = true
			}

			// If neither IP nor containerID matches an active pod, reclaim lease
			if !isActive {
				if err := os.Remove(leasePath); err == nil {
					reclaimed = append(reclaimed, ipStr)
				}
			}
		}
		return nil
	})
	return reclaimed, err
}

func (ipam *IPAM) findContainerLease(containerID string) (net.IP, error) {
	entries, err := os.ReadDir(ipam.leaseDir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(ipam.leaseDir, entry.Name()))
		if err != nil {
			continue
		}
		lease := parseLeaseData(net.ParseIP(entry.Name()), data)
		if lease.ContainerID == containerID {
			return lease.IP, nil
		}
	}
	return nil, ErrLeaseNotFound
}

func parseLeaseData(ip net.IP, data []byte) Lease {
	lease := Lease{IP: ip}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 0 {
		lease.ContainerID = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		lease.IfName = strings.TrimSpace(lines[1])
	}
	if len(lines) > 2 {
		lease.PodNamespace = strings.TrimSpace(lines[2])
	}
	if len(lines) > 3 {
		lease.PodName = strings.TrimSpace(lines[3])
	}
	if len(lines) > 4 {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(lines[4])); err == nil {
			lease.AllocatedAt = t
		}
	}
	return lease
}

func cidrBounds(ipnet *net.IPNet) (netUint, bcastUint uint32) {
	ipUint := ipToUint32(ipnet.IP.To4())
	maskUint := ipToUint32(net.IP(ipnet.Mask).To4())
	netUint = ipUint & maskUint
	bcastUint = netUint | (^maskUint)
	return
}

func ipToUint32(ip net.IP) uint32 {
	return binary.BigEndian.Uint32(ip.To4())
}

func uint32ToIP(n uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, n)
	return ip
}
