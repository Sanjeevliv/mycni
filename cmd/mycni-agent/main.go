package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	"github.com/sanjeevliv/mycni/pkg/agent"
)

func main() {
	var (
		nodeName       string
		podCIDR        string
		clusterCIDR    string
		bridgeName     string
		mtu            int
		routingBackend string
		cniBinSrc      string
		cniBinDir      string
		cniConfDir     string
		cniDataDir     string
		kubeconfig     string
		syncPeriod     time.Duration
	)

	defaultKubeconfig := os.Getenv("KUBECONFIG")
	if defaultKubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			defaultKubeconfig = filepath.Join(home, ".kube", "config")
		}
	}

	flag.StringVar(&nodeName, "node-name", getEnvOrDefault("NODE_NAME", ""), "Kubernetes node name")
	flag.StringVar(&podCIDR, "pod-cidr", getEnvOrDefault("POD_CIDR", ""), "Node Pod CIDR (auto-detected if omitted)")
	flag.StringVar(&clusterCIDR, "cluster-cidr", getEnvOrDefault("CLUSTER_CIDR", "10.244.0.0/16"), "Cluster-wide Pod CIDR for NAT masquerade")
	flag.StringVar(&bridgeName, "bridge", "mycni0", "Bridge interface name")
	flag.IntVar(&mtu, "mtu", 1500, "MTU for bridge and pod veth interfaces")
	flag.StringVar(&routingBackend, "backend", getEnvOrDefault("ROUTING_BACKEND", "direct"), "Cross-node routing backend (direct, vxlan)")
	flag.StringVar(&cniBinSrc, "cni-bin-src", getEnvOrDefault("CNI_BIN_SRC", "/bin/mycni-cni"), "Source path of mycni-cni binary to install")
	flag.StringVar(&cniBinDir, "cni-bin-dir", getEnvOrDefault("CNI_BIN_DIR", "/host/opt/cni/bin"), "Target host directory for CNI binaries")
	flag.StringVar(&cniConfDir, "cni-conf-dir", getEnvOrDefault("CNI_CONF_DIR", "/host/etc/cni/net.d"), "Target host directory for CNI configs")
	flag.StringVar(&cniDataDir, "cni-data-dir", getEnvOrDefault("CNI_DATA_DIR", "/var/lib/cni/mycni"), "Target host directory for IPAM lease data")
	flag.StringVar(&kubeconfig, "kubeconfig", defaultKubeconfig, "Path to kubeconfig file (fallback if not in-cluster)")
	flag.DurationVar(&syncPeriod, "sync-period", 30*time.Second, "Periodic reconciliation interval")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	if nodeName == "" {
		logger.Error("node name must be specified via --node-name or NODE_NAME environment variable")
		os.Exit(1)
	}

	// 1. Build Kubernetes client
	k8sClient, err := buildK8sClient(kubeconfig)
	if err != nil {
		logger.Error("failed to create kubernetes client", "error", err)
		os.Exit(1)
	}

	// 2. Initialize Agent
	cfg := agent.Config{
		NodeName:       nodeName,
		PodCIDR:        podCIDR,
		ClusterCIDR:    clusterCIDR,
		BridgeName:     bridgeName,
		MTU:            mtu,
		RoutingBackend: routingBackend,
		CNIBinSrc:      cniBinSrc,
		CNIBinDir:      cniBinDir,
		CNIConfDir:     cniConfDir,
		CNIDataDir:     cniDataDir,
		SyncPeriod:     syncPeriod,
	}

	nodeAgent, err := agent.New(cfg, k8sClient, logger)
	if err != nil {
		logger.Error("failed to initialize agent", "error", err)
		os.Exit(1)
	}

	// 3. Setup context with signal cancellation
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Info("starting mycni-agent daemon", "node", nodeName, "backend", routingBackend, "version", "v0.1.0")
	if err := nodeAgent.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("agent failed", "error", err)
		os.Exit(1)
	}
}

func buildK8sClient(kubeconfig string) (kubernetes.Interface, error) {
	// First try in-cluster config
	config, err := rest.InClusterConfig()
	if err == nil {
		return kubernetes.NewForConfig(config)
	}

	// Fallback to kubeconfig file
	if kubeconfig != "" {
		if _, statErr := os.Stat(kubeconfig); statErr == nil {
			config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
			if err == nil {
				return kubernetes.NewForConfig(config)
			}
		}
	}

	return nil, fmt.Errorf("unable to load in-cluster config or kubeconfig (%v): %w", kubeconfig, err)
}

func getEnvOrDefault(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

