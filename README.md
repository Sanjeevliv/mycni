# MyCNI: Kubernetes Container Network Interface from Scratch

**MyCNI** is a Kubernetes CNI plugin written from scratch in Go. It provisions pod network namespaces with veth interfaces, attaches them to a host Linux bridge (`mycni0`), allocates IP addresses per node using file-locked IPAM (`flock`), and enables pod-to-pod and pod-to-internet connectivity with outbound NAT.

---

## Current Status: Milestone 1 (M1) Complete ✅

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
  - Verified on `mycni-worker`: node transitions to `Ready`, pods receive IPs, inter-pod ping passes, outbound internet connectivity passes, and pod deletion cleanly reclaims leases and veth devices.

---

## Project Structure

```text
mycni/
├── cmd/
│   └── mycni-cni/          # CNI plugin binary (skel.PluginMainFuncs)
├── pkg/
│   ├── cni/                # CNI config parsing and validation
│   ├── ipam/               # Subnet IPAM allocator with flock persistence
│   └── netutil/            # Bridge, veth, sysctl, and netlink operations
├── deploy/
│   ├── kind-config.yaml    # 3-node kind cluster configuration
│   └── 10-mycni.conf       # Sample CNI configuration file
├── test/
│   └── manifests/          # Test workloads and pod definitions
├── MyCNI_plan.md           # Master roadmap and architectural specification
├── Makefile                # Build, test, and worker installation targets
└── README.md
```

---

## Quickstart

### 1. Build the CNI Binary
```bash
make build
```
Cross-compiles `bin/mycni-cni` with `CGO_ENABLED=0 GOOS=linux GOARCH=arm64`.

### 2. Run Unit Tests
```bash
make test
```
Runs unit tests with race detection (`-race`) covering config parsing and IPAM allocation, concurrency, reuse, and exhaustion.

### 3. Deploy to Kind Worker Node (M1)
```bash
make install-worker
```
Copies `mycni-cni` to `/opt/cni/bin/mycni-cni`, writes `/etc/cni/net.d/10-mycni.conf`, and configures outbound iptables MASQUERADE.

### 4. Verify Pod Connectivity
```bash
kubectl apply -f test/manifests/m1-test-pods.yaml
kubectl get pods -o wide
kubectl exec pod-a -- ping -c 3 <pod-b-ip>
kubectl exec pod-a -- ping -c 3 8.8.8.8
```
