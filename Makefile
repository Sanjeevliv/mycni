GOOS ?= linux
GOARCH ?= arm64
CGO_ENABLED ?= 0

BIN_DIR := bin
CNI_BIN := $(BIN_DIR)/mycni-cni
AGENT_BIN := $(BIN_DIR)/mycni-agent
E2E_BIN := $(BIN_DIR)/mycni-e2e
IMAGE_NAME ?= mycni:latest
CLUSTER_NAME ?= mycni
KIND_NODE_IMAGE ?= kindest/node:v1.36.1

.PHONY: all build build-cni build-agent build-e2e test image kind-up kind-down kind-load deploy redeploy logs e2e test-scale clean install-worker

all: build

build: build-cni build-agent build-e2e

build-cni:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) go build -v -o $(CNI_BIN) ./cmd/mycni-cni

build-agent:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) go build -v -o $(AGENT_BIN) ./cmd/mycni-agent

build-e2e:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) go build -v -o $(E2E_BIN) ./cmd/mycni-e2e

test:
	go test -v -race ./pkg/...

image: build
	docker build --network=host -t $(IMAGE_NAME) .

kind-up:
	@if kind get clusters | grep -q "^$(CLUSTER_NAME)$$"; then \
		echo "Kind cluster '$(CLUSTER_NAME)' already exists."; \
	else \
		kind create cluster --name $(CLUSTER_NAME) --config deploy/kind-config.yaml --image $(KIND_NODE_IMAGE); \
	fi

kind-down:
	kind delete cluster --name $(CLUSTER_NAME)

kind-load:
	kind load docker-image $(IMAGE_NAME) --name $(CLUSTER_NAME)

deploy: image kind-load
	kubectl apply -f deploy/mycni.yaml
	kubectl -n kube-system rollout status ds/mycni-agent --timeout=60s

redeploy: image kind-load
	kubectl -n kube-system rollout restart ds/mycni-agent
	kubectl -n kube-system rollout status ds/mycni-agent --timeout=60s

logs:
	kubectl -n kube-system logs -l k8s-app=mycni --tail=100 -f

e2e:
	bash ./test/e2e/e2e.sh

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
