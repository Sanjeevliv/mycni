# MyCNI: Kubernetes Container Network Interface from Scratch

**MyCNI** is a Kubernetes CNI plugin and node daemon written from scratch in Go. It provisions pod network namespaces with veth interfaces, attaches them to a host Linux bridge (`mycni0`), automatically discovers per-node subnets (`spec.podCIDR`) via a Kubernetes `DaemonSet` agent, allocates IP addresses using file-locked IPAM (`flock`) with startup garbage collection, and enables pod-to-pod and pod-to-internet connectivity with outbound NAT.

---

## Current Status: Milestone 2 (M2) Complete ✅

- [x] **M0: Environment & Manual Networking Lab**
  - Multi-node `kind` cluster with default CNI disabled (`10.244.0.0/16` podSubnet).
  - Manual Linux network namespaces, veth pairs, and bridge setup verified.
  - Repository structure, `go.mod`, `.gitignore`, and `Makefile` configured.
- [x] **M1: Single-Node CNI Plugin**
  - CNI v1.0.0 compliant binary (`cmd/mycni-cni`) implementing `ADD`, `DEL`, and `CHECK`.
  - JSON CNI config parsing (`pkg/cni`) supporting configurable bridge, MTU, dataDir, and podCIDR.
  - Concurrency-safe local IPAM (`pkg/ipam`) with exclusive file locking (`syscall.Flock`).
  - Automated veth creation, namespace setup, default route assignment, and bridge attachment (`pkg/netutil`).
  - Structured operation logging to `/var/log/mycni-cni.log`.
  - Egress MASQUERADE NAT configured for external reachability.
  - Walkthrough and interview guide: [`M1.md`](file:///Users/sanjeev/Desktop/mycni/M1.md).
- [x] **M2: DaemonSet Agent + Automated Install + IPAM Hardening**
  - Long-running DaemonSet agent (`cmd/mycni-agent`, `pkg/agent`) deployed to all cluster nodes.
  - Dynamic discovery of node `spec.podCIDR` via Kubernetes Downward API and `client-go`.
  - Atomic installation of `mycni-cni` binary and configuration into host directories (`/opt/cni/bin`, `/etc/cni/net.d`).
  - Automated dataplane bootstrap: `mycni0` bridge creation, gateway IP assignment, sysctls (`net.ipv4.ip_forward=1`, `net.bridge.bridge-nf-call-iptables=1` with `modprobe br_netfilter`), and idempotent iptables MASQUERADE.
  - IPAM hardening: rich lease metadata tracking, backward compatibility, and startup/periodic garbage collection to reclaim leaked IPs from crashed sandboxes.
  - High concurrency stress test passed: 30-replica Deployment verified with 30 unique IPs (0 collisions) and 100% clean lease reclamation on scale-down.
  - Zero-downtime rolling restarts verified: active pods and CoreDNS continue running without packet drops.
  - Walkthrough and interview guide: [`M2.md`](file:///Users/sanjeev/Desktop/mycni/M2.md).
- [x] **M3: Cross-Node Connectivity — Direct Routing**
  - Dynamic route controller in `mycni-agent` watching Kubernetes `Node` events.
  - Direct L3 host routes (`proto 77`) via Netlink across all cluster nodes.
  - E2E cross-node connectivity test suite verified via `make e2e`.

---

## Project Structure

```text
mycni/
├── cmd/
│   ├── mycni-cni/          # CNI plugin binary (skel.PluginMainFuncs)
│   └── mycni-agent/        # Node DaemonSet agent entrypoint
├── pkg/
│   ├── agent/              # Node bootstrapping, CNI asset installer, and reconciler
│   ├── cni/                # CNI config parsing, CNI_ARGS parser, and validation
│   ├── ipam/               # Subnet IPAM allocator, lease metadata, flock, and GC
│   └── netutil/            # Bridge, veth, iptables NAT, sysctl, and netlink operations
├── deploy/
│   ├── mycni.yaml          # DaemonSet, ServiceAccount, ClusterRole, and ClusterRoleBinding
│   ├── kind-config.yaml    # 3-node kind cluster configuration
│   └── 10-mycni.conf       # Sample CNI configuration file
├── test/
│   └── manifests/
│       ├── m1-test-pods.yaml   # M1 single-node test pods
│       └── m2-scale-test.yaml  # M2 30-replica concurrency scale test
├── Dockerfile              # Container image packaging mycni-agent + mycni-cni
├── Makefile                # Automated build, test, image, kind-load, and deploy targets
├── M1.md                   # Milestone 1 architectural guide & interview prep
├── M2.md                   # Milestone 2 architectural guide & interview prep
├── MyCNI_plan.md           # Master roadmap and architectural specification
└── README.md
```

---

## Quickstart

### 1. Build Binaries and Run Tests
```bash
# Build linux binaries for mycni-cni and mycni-agent
make build

# Run unit test suite with race detector (-race)
make test
```

### 2. Build Container Image and Load into Kind
```bash
# Build container image with iptables, iproute2, and kmod
make image

# Load image into multi-node kind cluster
make kind-load
```

### 3. Deploy MyCNI DaemonSet across All Nodes
```bash
# Apply RBAC and DaemonSet manifests
make deploy
```
Watch all nodes automatically transition to `Ready` and `CoreDNS` pods become `1/1 Running`:
```bash
kubectl get nodes
kubectl get pods -n kube-system
```

### 4. Run Concurrency Scale Test (30 Replicas)
```bash
make test-scale
```
Scales 30 pods across the cluster, confirms zero IP collisions, and checks IP allocations.

### 5. Inspect Agent Logs & Rollout Restarts
```bash
# View live logs from DaemonSet agents
make logs

# Test zero-downtime rolling restart
make redeploy
```
