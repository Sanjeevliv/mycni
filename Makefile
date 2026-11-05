GOOS ?= linux
GOARCH ?= arm64
CGO_ENABLED ?= 0

BIN_DIR := bin
CNI_BIN := $(BIN_DIR)/mycni-cni
AGENT_BIN := $(BIN_DIR)/mycni-agent
IMAGE_NAME ?= mycni:latest
CLUSTER_NAME ?= mycni

.PHONY: all build build-cni build-agent test image kind-load deploy redeploy logs test-scale clean install-worker

all: build

build: build-cni build-agent

build-cni:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) go build -v -o $(CNI_BIN) ./cmd/mycni-cni

build-agent:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) go build -v -o $(AGENT_BIN) ./cmd/mycni-agent

test:
	go test -v -race ./pkg/...

image: build
	docker build --network=host -t $(IMAGE_NAME) .

kind-load:
	kind load docker-image $(IMAGE_NAME) --name $(CLUSTER_NAME)

deploy:
	kubectl apply -f deploy/mycni.yaml

redeploy: image kind-load
	kubectl -n kube-system rollout restart ds/mycni-agent
	kubectl -n kube-system rollout status ds/mycni-agent --timeout=60s

logs:
	kubectl -n kube-system logs -l k8s-app=mycni --tail=100 -f

test-scale:
	kubectl apply -f test/manifests/m2-scale-test.yaml
	kubectl rollout status deployment/m2-scale-test --timeout=120s
	@echo "Checking assigned pod IPs:"
	kubectl get pods -l app=m2-scale-test -o wide
	@echo "Unique IP count:"
	@kubectl get pods -l app=m2-scale-test -o jsonpath='{.items[*].status.podIP}' | tr ' ' '\n' | sort -u | wc -l

clean:
	rm -rf $(BIN_DIR)

# M1 manual installation helper for mycni-worker (kept for reference)
install-worker: build-cni
	docker cp $(CNI_BIN) mycni-worker:/opt/cni/bin/mycni-cni
	docker exec mycni-worker chmod +x /opt/cni/bin/mycni-cni
	docker exec mycni-worker mkdir -p /etc/cni/net.d /var/lib/cni/mycni
	docker exec mycni-worker sh -c 'printf "{\n  \"cniVersion\": \"1.0.0\",\n  \"name\": \"mycni\",\n  \"type\": \"mycni-cni\",\n  \"bridge\": \"mycni0\",\n  \"mtu\": 1500,\n  \"podCIDR\": \"10.244.1.0/24\",\n  \"dataDir\": \"/var/lib/cni/mycni\"\n}\n" > /etc/cni/net.d/10-mycni.conf'
	docker exec mycni-worker iptables -t nat -C POSTROUTING -s 10.244.1.0/24 ! -d 10.244.0.0/16 -j MASQUERADE 2>/dev/null || \
		docker exec mycni-worker iptables -t nat -A POSTROUTING -s 10.244.1.0/24 ! -d 10.244.0.0/16 -j MASQUERADE
