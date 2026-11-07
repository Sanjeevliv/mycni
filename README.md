# MyCNI: Kubernetes Container Network Interface from Scratch

**MyCNI** is a Kubernetes CNI plugin and node routing daemon written from scratch in Go. It provisions pod network namespaces with veth interfaces, attaches them to a host Linux bridge (`mycni0`) with hairpin mode enabled, dynamically discovers per-node subnets (`spec.podCIDR`) via a Kubernetes `DaemonSet` agent, allocates IP addresses using file-locked IPAM (`flock`) with garbage collection, programs cross-node host routes (`proto 77`) via a `client-go` `SharedInformer` watching `v1.Node` objects, and enables full cross-node pod-to-pod, `ClusterIP` Service, CoreDNS, and internet connectivity.

---

## One-Command Quickstart

Bring up a 3-node `kind` cluster, build and load the `mycni` image, deploy the `mycni-agent` `DaemonSet`, and run the full cross-node E2E connectivity suite in a single command:

```bash
make kind-up deploy e2e
```

---

## Current Status: Milestone 3 (M3) Complete — `v0.1.0` ✅

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
- [x] **M2: DaemonSet Agent + Automated Install + IPAM Hardening**
  - Long-running DaemonSet agent (`cmd/mycni-agent`, `pkg/agent`) deployed to all cluster nodes.
  - Dynamic discovery of node `spec.podCIDR` via Kubernetes Downward API and `client-go`.
  - Atomic installation of `mycni-cni` binary and configuration into host directories (`/opt/cni/bin`, `/etc/cni/net.d`).
  - Automated dataplane bootstrap: `mycni0` bridge creation, gateway IP assignment, sysctls (`net.ipv4.ip_forward=1`, `net.bridge.bridge-nf-call-iptables=1` with `modprobe br_netfilter`), and idempotent iptables MASQUERADE.
  - IPAM hardening: rich lease metadata tracking, backward compatibility, and startup/periodic garbage collection to reclaim leaked IPs from crashed sandboxes.
  - High concurrency stress test passed: 30-replica Deployment verified with 30 unique IPs (0 collisions) and 100% clean lease reclamation on scale-down.
- [x] **M3: Cross-Node Connectivity — Direct Routing (`v0.1.0`)**
  - Pluggable routing backend interface (`pkg/routing`) with `NodeInfo`, `Backend`, `ExtractNodeInfo`, and deterministic `DiffRoutes`.
  - Direct routing backend (`pkg/routing/direct`) programming kernel host routes (`ip route replace <peerPodCIDR> via <peerNodeIP> dev eth0 proto 77`) via Netlink.
  - Real-time Kubernetes `SharedInformer` watching `v1.Node` additions, updates, and deletions in `mycni-agent`, paired with a 30-second periodic route reconciler.
  - Bridge port hairpin mode (`LinkSetHairpin`) and cluster CIDR `FORWARD` rules for seamless `ClusterIP` Service VIP hairpinning and cross-node traffic.
  - Automated end-to-end verification suite (`test/e2e/e2e.sh`, `cmd/mycni-e2e`) verifying the full $N \times N$ pod-to-pod ping/HTTP matrix, `ClusterIP` Service VIPs, CoreDNS resolution, and dynamic route cleanup on node deletion.
  - Architectural design doc: [`docs/design.md`](file:///Users/sanjeev/Desktop/mycni/docs/design.md).

---

## Project Structure

```text
mycni/
├── cmd/
│   ├── mycni-cni/          # CNI plugin binary (skel.PluginMainFuncs)
│   ├── mycni-agent/        # Node DaemonSet agent & route controller entrypoint
│   └── mycni-e2e/          # Self-contained HTTP/ICMP/DNS probe binary for E2E tests
├── pkg/
│   ├── agent/              # Node bootstrapping, CNI installer, Node informer, & reconcilers
│   ├── cni/                # CNI config parsing, CNI_ARGS parser, and validation
│   ├── ipam/               # Subnet IPAM allocator, lease metadata, flock, and GC
│   ├── netutil/            # Bridge (with hairpin), veth, iptables NAT/FORWARD, sysctl
│   └── routing/            # Pluggable routing Backend interface & DiffRoutes
│       └── direct/         # Direct L3 host-routing backend (netlink proto 77)
├── deploy/
│   ├── mycni.yaml          # DaemonSet, ServiceAccount, ClusterRole, and ClusterRoleBinding
│   ├── kind-config.yaml    # 3-node kind cluster configuration
│   └── 10-mycni.conf       # Sample CNI configuration file
├── docs/
│   └── design.md           # Dataplane & cross-node routing architecture specification
├── test/
│   ├── e2e/                # Automated cross-node E2E connectivity test suite
│   └── manifests/
│       ├── m1-test-pods.yaml   # M1 single-node test pods
│       ├── m2-scale-test.yaml  # M2 30-replica concurrency scale test
│       └── m3-e2e.yaml         # M3 E2E probe DaemonSet and ClusterIP Service
├── Dockerfile              # Container image packaging mycni-agent, mycni-cni, & mycni-e2e
├── Makefile                # Automated build, test, kind-up, deploy, and e2e targets
├── MyCNI_plan.md           # Master roadmap and architectural specification
└── README.md
```

---

## Step-by-Step Usage

### 1. Build Binaries and Run Unit Tests
```bash
# Build linux binaries for mycni-cni, mycni-agent, and mycni-e2e
make build

# Run unit test suite with race detector (-race)
make test
```

### 2. Provision Kind Cluster and Deploy MyCNI
```bash
# Create 3-node kind cluster (if not already running)
make kind-up

# Build image, load into kind nodes, and deploy the mycni-agent DaemonSet
make deploy
```

### 3. Run End-to-End Cross-Node Connectivity Suite
```bash
make e2e
```
Verifies:
1. All cluster nodes transition to `Ready`.
2. `mycni-agent` DaemonSet and `CoreDNS` pods are `1/1 Running`.
3. Every node has `proto 77` kernel routes installed for all peer `podCIDR` subnets.
4. Full $N \times N$ pod-to-pod ICMP `ping` and HTTP `curl` matrix across all nodes passes.
5. `ClusterIP` Service VIP reachability and `CoreDNS` resolution (`kubernetes.default.svc.cluster.local`) succeed from every pod.
6. Deleting a worker node automatically cleans up its `podCIDR` route on remaining nodes, and restoring the node re-programs the route.

### 4. Run Concurrency Scale Test (30 Replicas)
```bash
make test-scale
```
Scales 30 pods across the cluster, confirms zero IP collisions, and verifies clean IP reclamation.
