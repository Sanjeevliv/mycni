#!/usr/bin/env bash
set -euo pipefail

echo "================================================================="
echo " MyCNI Milestone 3 — End-to-End Connectivity & Routing Test Suite"
echo "================================================================="

# 1. Verify all nodes are Ready
echo ""
echo "[1/7] Verifying all cluster nodes are Ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=60s
kubectl get nodes -o wide

# 2. Verify mycni-agent and CoreDNS are Running
echo ""
echo "[2/7] Verifying mycni-agent DaemonSet and CoreDNS..."
kubectl -n kube-system rollout status ds/mycni-agent --timeout=60s
kubectl -n kube-system wait --for=condition=Ready pod -l k8s-app=kube-dns --timeout=60s
kubectl -n kube-system get pods -o wide

# 3. Verify proto 77 direct routes on all nodes
echo ""
echo "[3/7] Verifying kernel routes (proto 77) on each node..."
NODES=$(kubectl get nodes -o jsonpath='{.items[*].metadata.name}')
for node in $NODES; do
    echo "  -> Node: $node"
    ROUTES=$(docker exec "$node" ip route show | grep "proto 77" || true)
    echo "$ROUTES" | sed 's/^/     /'
    COUNT=$(echo "$ROUTES" | grep -c "proto 77" || true)
    if [ "$COUNT" -lt 2 ]; then
        echo "ERROR: Expected at least 2 peer routes (proto 77) on $node, found $COUNT"
        exit 1
    fi
done

# 4. Deploy E2E test DaemonSet and ClusterIP Service
echo ""
echo "[4/7] Deploying E2E probe DaemonSet and ClusterIP Service..."
kubectl apply -f test/manifests/m3-e2e.yaml
kubectl rollout status ds/mycni-e2e --timeout=60s
kubectl wait --for=condition=Ready pod -l app=mycni-e2e --timeout=60s

PODS=$(kubectl get pods -l app=mycni-e2e -o jsonpath='{.items[*].metadata.name}')
echo "Active E2E pods:"
kubectl get pods -l app=mycni-e2e -o wide

# 5. Full N x N Pod-to-Pod Ping & HTTP Matrix
echo ""
echo "[5/7] Testing full N x N Pod-to-Pod Ping & HTTP Matrix across all nodes..."
PASS_COUNT=0
for src_pod in $PODS; do
    SRC_NODE=$(kubectl get pod "$src_pod" -o jsonpath='{.spec.nodeName}')
    SRC_IP=$(kubectl get pod "$src_pod" -o jsonpath='{.status.podIP}')
    for dst_pod in $PODS; do
        DST_NODE=$(kubectl get pod "$dst_pod" -o jsonpath='{.spec.nodeName}')
        DST_IP=$(kubectl get pod "$dst_pod" -o jsonpath='{.status.podIP}')

        echo -n "  -> $src_pod ($SRC_NODE: $SRC_IP) ==> $dst_pod ($DST_NODE: $DST_IP): "
        kubectl exec "$src_pod" -- ping -c 1 -W 3 "$DST_IP" >/dev/null
        RESP=$(kubectl exec "$src_pod" -- curl -sSf --connect-timeout 3 "http://${DST_IP}:8080/")
        if ! echo "$RESP" | grep -q "$dst_pod"; then
            echo "FAILED (unexpected HTTP response: $RESP)"
            exit 1
        fi
        echo "PING OK | HTTP OK"
        PASS_COUNT=$((PASS_COUNT + 1))
    done
done
echo "  Verified $PASS_COUNT / $PASS_COUNT pod-to-pod pairs!"

# 6. Verify ClusterIP Service VIP and CoreDNS Resolution from every pod
echo ""
echo "[6/7] Verifying ClusterIP Service VIP and CoreDNS resolution from every pod..."
SVC_IP=$(kubectl get svc mycni-e2e-svc -o jsonpath='{.spec.clusterIP}')
echo "  ClusterIP for mycni-e2e-svc: $SVC_IP"

for src_pod in $PODS; do
    SRC_NODE=$(kubectl get pod "$src_pod" -o jsonpath='{.spec.nodeName}')
    echo -n "  -> From $src_pod ($SRC_NODE): "

    # ClusterIP VIP HTTP request
    kubectl exec "$src_pod" -- curl -sSf --connect-timeout 3 "http://${SVC_IP}:80/healthz" >/dev/null

    # CoreDNS lookup for kubernetes.default.svc.cluster.local
    kubectl exec "$src_pod" -- nslookup kubernetes.default.svc.cluster.local >/dev/null

    # CoreDNS lookup + HTTP request for mycni-e2e-svc.default.svc.cluster.local
    kubectl exec "$src_pod" -- nslookup mycni-e2e-svc.default.svc.cluster.local >/dev/null
    kubectl exec "$src_pod" -- curl -sSf --connect-timeout 3 "http://mycni-e2e-svc.default.svc.cluster.local/healthz" >/dev/null

    echo "ClusterIP OK | CoreDNS OK"
done

# 7. Verify dynamic route cleanup on node deletion and restoration
echo ""
echo "[7/7] Verifying automatic route cleanup on node deletion..."
WORKER2_CIDR=$(kubectl get node mycni-worker2 -o jsonpath='{.spec.podCIDR}')
echo "  Stopping kubelet and deleting node mycni-worker2 (podCIDR: $WORKER2_CIDR)..."
docker exec mycni-worker2 systemctl stop kubelet
kubectl delete node mycni-worker2 >/dev/null

sleep 3
REMAINING_ROUTE_W1=$(docker exec mycni-worker ip route show | grep "proto 77" | grep "$WORKER2_CIDR" || true)
REMAINING_ROUTE_CP=$(docker exec mycni-control-plane ip route show | grep "proto 77" | grep "$WORKER2_CIDR" || true)
if [ -n "$REMAINING_ROUTE_W1" ] || [ -n "$REMAINING_ROUTE_CP" ]; then
    echo "ERROR: Route for deleted node ($WORKER2_CIDR) was not cleaned up!"
    exit 1
fi
echo "  Confirmed: route $WORKER2_CIDR automatically removed from mycni-worker and mycni-control-plane!"

echo "  Restoring mycni-worker2 (podCIDR: $WORKER2_CIDR) and starting kubelet..."
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Node
metadata:
  name: mycni-worker2
spec:
  podCIDR: ${WORKER2_CIDR}
  podCIDRs:
  - ${WORKER2_CIDR}
EOF
docker exec mycni-worker2 systemctl start kubelet
kubectl wait --for=condition=Ready node/mycni-worker2 --timeout=60s

# Wait for route to be re-programmed by informer
for i in {1..15}; do
    RESTORED_ROUTE=$(docker exec mycni-worker ip route show | grep "proto 77" | grep -c "$WORKER2_CIDR" || true)
    if [ "$RESTORED_ROUTE" -ge 1 ]; then
        break
    fi
    sleep 1
done
echo "  Confirmed: peer route $WORKER2_CIDR restored after mycni-worker2 rejoined!"

# Cleanup E2E workload
kubectl delete -f test/manifests/m3-e2e.yaml --ignore-not-found >/dev/null

echo ""
echo "================================================================="
echo " All M3 End-to-End Connectivity Checks PASSED!"
echo "================================================================="
