#!/usr/bin/env bash
set -euo pipefail

echo "==> Verifying cluster nodes are Ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=60s

echo "==> Checking direct routes (proto 77) on each node..."
for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
    echo "Node $node:"
    docker exec "$node" ip route show proto 77
done

echo "==> Testing cross-node pod ping..."
kubectl run probe-1 --image=busybox:1.36 --restart=Never -- sleep 3600
kubectl run probe-2 --image=busybox:1.36 --restart=Never -- sleep 3600
kubectl wait --for=condition=Ready pod/probe-1 pod/probe-2 --timeout=60s

IP2=$(kubectl get pod probe-2 -o jsonpath='{.status.podIP}')
echo "Pinging probe-2 ($IP2) from probe-1..."
kubectl exec probe-1 -- ping -c 3 "$IP2"

kubectl delete pod probe-1 probe-2 --ignore-not-found
echo "M3 direct routing verification PASSED!"
