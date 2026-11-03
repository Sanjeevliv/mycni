package ipam

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var (
	// ErrPoolExhausted is returned when all allocatable IPs in the CIDR are in use.
	ErrPoolExhausted = errors.New("IPAM pool exhausted: no available IP addresses")
	// ErrLeaseNotFound is returned when looking up a lease that does not exist.
	ErrLeaseNotFound = errors.New("IPAM lease not found")
)

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

		for cur := startIP; cur <= endIP; cur++ {
			candidateIP := uint32ToIP(cur)
			leasePath := filepath.Join(ipam.leaseDir, candidateIP.String())

			if _, err := os.Stat(leasePath); os.IsNotExist(err) {
				// Free IP found, claim it
				content := fmt.Sprintf("%s\n%s\n", containerID, ifName)
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

			cid := readContainerID(data)
			if cid == containerID {
				_ = os.Remove(leasePath)
			}
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
			cid := readContainerID(data)
			if cid != "" {
				leases[entry.Name()] = cid
			}
		}
		return nil
	})
	return leases, err
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
		if readContainerID(data) == containerID {
			ip := net.ParseIP(entry.Name())
			if ip != nil {
				return ip, nil
			}
		}
	}
	return nil, ErrLeaseNotFound
}

func readContainerID(data []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text())
	}
	return ""
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
