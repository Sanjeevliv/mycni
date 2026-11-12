package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/sanjeevliv/mycni/pkg/ipam"
	"github.com/sanjeevliv/mycni/pkg/netutil"
	"github.com/sanjeevliv/mycni/pkg/routing"
)

// Config holds the configuration options for the mycni-agent node daemon.
type Config struct {
	NodeName    string
	PodCIDR     string
	ClusterCIDR string
	BridgeName  string
	MTU         int
	CNIBinSrc   string
	CNIBinDir   string
	CNIConfDir  string
	CNIDataDir  string
	SyncPeriod  time.Duration
}

// DefaultConfig returns a Config with production defaults.
func DefaultConfig() Config {
	return Config{
		ClusterCIDR: "10.244.0.0/16",
		BridgeName:  "mycni0",
		MTU:         1500,
		CNIBinSrc:   "/bin/mycni-cni",
		CNIBinDir:   "/host/opt/cni/bin",
		CNIConfDir:  "/host/etc/cni/net.d",
		CNIDataDir:  "/var/lib/cni/mycni",
		SyncPeriod:  30 * time.Second,
	}
}

// Agent coordinates automated node bootstrapping, dataplane initialization,
// and IPAM lease reconciliation.
type Agent struct {
	cfg       Config
	k8sClient kubernetes.Interface
	logger    *slog.Logger
	ipam      *ipam.IPAM
}

// New creates a new Agent instance.
func New(cfg Config, client kubernetes.Interface, logger *slog.Logger) (*Agent, error) {
	if cfg.NodeName == "" {
		return nil, fmt.Errorf("nodeName cannot be empty")
	}
	if cfg.BridgeName == "" {
		cfg.BridgeName = "mycni0"
	}
	if cfg.MTU <= 0 {
		cfg.MTU = 1500
	}
	if cfg.SyncPeriod <= 0 {
		cfg.SyncPeriod = 30 * time.Second
	}
	if cfg.ClusterCIDR == "" {
		cfg.ClusterCIDR = "10.244.0.0/16"
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Agent{
		cfg:       cfg,
		k8sClient: client,
		logger:    logger,
	}, nil
}

// InstallCNIBinary atomically copies the CNI plugin executable to the host directory.
func (a *Agent) InstallCNIBinary() error {
	if err := os.MkdirAll(a.cfg.CNIBinDir, 0755); err != nil {
		return fmt.Errorf("failed to create CNI bin directory %q: %w", a.cfg.CNIBinDir, err)
	}

	srcFile, err := os.Open(a.cfg.CNIBinSrc)
	if err != nil {
		return fmt.Errorf("failed to open source CNI binary %q: %w", a.cfg.CNIBinSrc, err)
	}
	defer srcFile.Close()

	tmpPath := filepath.Join(a.cfg.CNIBinDir, fmt.Sprintf(".mycni-cni.tmp.%d", os.Getpid()))
	dstPath := filepath.Join(a.cfg.CNIBinDir, "mycni-cni")

	dstFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("failed to create temp CNI binary %q: %w", tmpPath, err)
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to copy CNI binary data: %w", err)
	}
	dstFile.Close()

	if err := os.Chmod(tmpPath, 0755); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to chmod temp CNI binary: %w", err)
	}

	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to atomically replace %q with %q: %w", dstPath, tmpPath, err)
	}

	a.logger.Info("atomically installed CNI binary", "src", a.cfg.CNIBinSrc, "dst", dstPath)
	return nil
}

// RenderCNIConfig atomically renders the CNI configuration JSON into the host directory.
func (a *Agent) RenderCNIConfig() error {
	if err := os.MkdirAll(a.cfg.CNIConfDir, 0755); err != nil {
		return fmt.Errorf("failed to create CNI conf directory %q: %w", a.cfg.CNIConfDir, err)
	}

	confMap := map[string]interface{}{
		"cniVersion": "1.0.0",
		"name":       "mycni",
		"type":       "mycni-cni",
		"bridge":     a.cfg.BridgeName,
		"mtu":        a.cfg.MTU,
		"podCIDR":    a.cfg.PodCIDR,
		"dataDir":    a.cfg.CNIDataDir,
	}

	data, err := json.MarshalIndent(confMap, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal CNI config JSON: %w", err)
	}

	tmpPath := filepath.Join(a.cfg.CNIConfDir, fmt.Sprintf(".10-mycni.conf.tmp.%d", os.Getpid()))
	dstPath := filepath.Join(a.cfg.CNIConfDir, "10-mycni.conf")

	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp CNI config %q: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("failed to atomically replace %q: %w", dstPath, err)
	}

	a.logger.Info("rendered CNI config", "path", dstPath, "podCIDR", a.cfg.PodCIDR)
	return nil
}

// FetchPodCIDR queries the Kubernetes API for this node to obtain the assigned podCIDR.
func (a *Agent) FetchPodCIDR(ctx context.Context) (string, error) {
	if a.cfg.PodCIDR != "" {
		return a.cfg.PodCIDR, nil
	}

	if a.k8sClient == nil {
		return "", fmt.Errorf("kubernetes client is required to fetch node podCIDR")
	}

	a.logger.Info("querying Kubernetes API for node spec.podCIDR", "node", a.cfg.NodeName)
	node, err := a.k8sClient.CoreV1().Nodes().Get(ctx, a.cfg.NodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get node %q: %w", a.cfg.NodeName, err)
	}

	podCIDR := node.Spec.PodCIDR
	if podCIDR == "" && len(node.Spec.PodCIDRs) > 0 {
		podCIDR = node.Spec.PodCIDRs[0]
	}

	if podCIDR == "" {
		return "", fmt.Errorf("node %q has no podCIDR assigned by controller-manager", a.cfg.NodeName)
	}

	a.cfg.PodCIDR = podCIDR
	return podCIDR, nil
}

// InitDataplane configures kernel sysctls, bridge interface, and iptables MASQUERADE.
func (a *Agent) InitDataplane() error {
	// 1. Enable sysctls
	if err := netutil.EnableIPForward(); err != nil {
		a.logger.Warn("failed to enable net.ipv4.ip_forward", "error", err)
	}
	if err := netutil.EnableBridgeNetfilter(); err != nil {
		a.logger.Warn("failed to enable bridge netfilter", "error", err)
	}

	// 2. Parse podCIDR
	_, podIPNet, err := net.ParseCIDR(a.cfg.PodCIDR)
	if err != nil {
		return fmt.Errorf("invalid podCIDR %q: %w", a.cfg.PodCIDR, err)
	}

	// 3. Initialize IPAM
	ipamInstance, err := ipam.New(a.cfg.CNIDataDir, podIPNet)
	if err != nil {
		return fmt.Errorf("failed to initialize IPAM: %w", err)
	}
	a.ipam = ipamInstance

	// 4. Ensure bridge mycni0 exists with gateway IP
	bridge, err := netutil.EnsureBridge(a.cfg.BridgeName, ipamInstance.GatewayIPNet(), a.cfg.MTU)
	if err != nil {
		return fmt.Errorf("failed to ensure bridge %q: %w", a.cfg.BridgeName, err)
	}
	a.logger.Info("ensured host bridge", "bridge", bridge.Attrs().Name, "gateway", ipamInstance.GetGatewayIP().String())

	// 5. Ensure iptables MASQUERADE NAT rule
	if err := netutil.EnsureMasquerade(a.cfg.PodCIDR, a.cfg.ClusterCIDR); err != nil {
		a.logger.Warn("failed to configure iptables MASQUERADE rule", "error", err)
	} else {
		a.logger.Info("ensured outbound NAT MASQUERADE rule", "podCIDR", a.cfg.PodCIDR, "clusterCIDR", a.cfg.ClusterCIDR)
	}
	_ = netutil.EnsureForwardRules(a.cfg.ClusterCIDR)

	return nil
}

// WatchNodes programs direct routes when peer nodes join the cluster.
func (a *Agent) WatchNodes(ctx context.Context) error {
	if a.k8sClient == nil {
		return nil
	}
	factory := informers.NewSharedInformerFactory(a.k8sClient, a.cfg.SyncPeriod)
	informer := factory.Core().V1().Nodes().Informer()

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if node, ok := obj.(*corev1.Node); ok && node.Name != a.cfg.NodeName {
				if peer, ok := routing.ExtractNodeInfo(node); ok {
					_ = routing.EnsureDirectRoute(1, peer.PodCIDR, peer.NodeIP)
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if node, ok := obj.(*corev1.Node); ok && node.Name != a.cfg.NodeName {
				if peer, ok := routing.ExtractNodeInfo(node); ok {
					_ = routing.DeleteDirectRoute(peer.PodCIDR)
				}
			}
		},
	})
	if err != nil {
		return err
	}
	factory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), informer.HasSynced)
	return nil
}

// ReconcileLeases synchronizes IPAM leases on disk with currently active pods.
func (a *Agent) ReconcileLeases(ctx context.Context) error {
	if a.ipam == nil || a.k8sClient == nil {
		return nil
	}

	pods, err := a.k8sClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + a.cfg.NodeName,
	})
	if err != nil {
		return fmt.Errorf("failed to list pods on node %q: %w", a.cfg.NodeName, err)
	}

	activePodIPs := make(map[string]bool)
	activeContainerIDs := make(map[string]bool)

	for _, pod := range pods.Items {
		// Only consider pods that have not completed/failed
		if pod.Status.Phase == "Failed" || pod.Status.Phase == "Succeeded" {
			continue
		}
		if pod.Status.PodIP != "" {
			activePodIPs[pod.Status.PodIP] = true
		}
		for _, stat := range pod.Status.ContainerStatuses {
			if stat.ContainerID != "" {
				cid := strings.TrimPrefix(stat.ContainerID, "containerd://")
				cid = strings.TrimPrefix(cid, "docker://")
				activeContainerIDs[cid] = true
			}
		}
	}

	reclaimed, err := a.ipam.GC(activePodIPs, activeContainerIDs, 30*time.Second)
	if err != nil {
		return fmt.Errorf("IPAM GC failed: %w", err)
	}

	if len(reclaimed) > 0 {
		a.logger.Info("garbage collected orphaned IPAM leases", "count", len(reclaimed), "reclaimedIPs", reclaimed)
	}

	return nil
}

// Bootstrap runs the initial startup sequence.
func (a *Agent) Bootstrap(ctx context.Context) error {
	a.logger.Info("starting mycni-agent node bootstrapping", "node", a.cfg.NodeName)

	// Step 1: Discover or wait for podCIDR
	for {
		cid, err := a.FetchPodCIDR(ctx)
		if err == nil && cid != "" {
			a.logger.Info("successfully resolved node podCIDR", "podCIDR", cid)
			break
		}
		a.logger.Warn("waiting for node podCIDR allocation...", "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	// Step 2: Install CNI plugin binary
	if err := a.InstallCNIBinary(); err != nil {
		return fmt.Errorf("failed to install CNI binary: %w", err)
	}

	// Step 3: Render CNI configuration
	if err := a.RenderCNIConfig(); err != nil {
		return fmt.Errorf("failed to render CNI config: %w", err)
	}

	// Step 4: Initialize dataplane (bridge, sysctls, iptables)
	if err := a.InitDataplane(); err != nil {
		return fmt.Errorf("failed to initialize dataplane: %w", err)
	}

	// Step 5: Startup IPAM lease reconciliation
	if err := a.ReconcileLeases(ctx); err != nil {
		a.logger.Warn("initial IPAM reconciliation warning", "error", err)
	}

	a.logger.Info("node bootstrapping complete", "node", a.cfg.NodeName, "podCIDR", a.cfg.PodCIDR)
	return nil
}

// Run executes the agent main loop until the context is canceled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.Bootstrap(ctx); err != nil {
		return err
	}
	_ = a.WatchNodes(ctx)

	ticker := time.NewTicker(a.cfg.SyncPeriod)
	defer ticker.Stop()

	a.logger.Info("agent entered periodic sync loop", "interval", a.cfg.SyncPeriod)

	for {
		select {
		case <-ctx.Done():
			a.logger.Info("shutting down mycni-agent (pod connectivity preserved)")
			return nil
		case <-ticker.C:
			// Refresh sysctls and NAT rules
			_ = netutil.EnableIPForward()
			_ = netutil.EnableBridgeNetfilter()
			_ = netutil.EnsureMasquerade(a.cfg.PodCIDR, a.cfg.ClusterCIDR)

			// Reconcile IPAM
			if err := a.ReconcileLeases(ctx); err != nil {
				a.logger.Warn("periodic IPAM reconciliation error", "error", err)
			}
		}
	}
}

