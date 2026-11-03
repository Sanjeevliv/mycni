GOOS ?= linux
GOARCH ?= arm64
CGO_ENABLED ?= 0

BIN_DIR := bin
CNI_BIN := $(BIN_DIR)/mycni-cni

.PHONY: all build build-cni test clean install-worker setup-worker-nat

all: build

build: build-cni

build-cni:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) go build -v -o $(CNI_BIN) ./cmd/mycni-cni

test:
	go test -v -race ./pkg/...

clean:
	rm -rf $(BIN_DIR)

# M1 installation helper for mycni-worker
install-worker: build-cni
	docker cp $(CNI_BIN) mycni-worker:/opt/cni/bin/mycni-cni
	docker exec mycni-worker chmod +x /opt/cni/bin/mycni-cni
	docker exec mycni-worker mkdir -p /etc/cni/net.d /var/lib/cni/mycni
	docker exec mycni-worker sh -c 'printf "{\n  \"cniVersion\": \"1.0.0\",\n  \"name\": \"mycni\",\n  \"type\": \"mycni-cni\",\n  \"bridge\": \"mycni0\",\n  \"mtu\": 1500,\n  \"podCIDR\": \"10.244.1.0/24\",\n  \"dataDir\": \"/var/lib/cni/mycni\"\n}\n" > /etc/cni/net.d/10-mycni.conf'
	docker exec mycni-worker iptables -t nat -C POSTROUTING -s 10.244.1.0/24 ! -d 10.244.0.0/16 -j MASQUERADE 2>/dev/null || \
		docker exec mycni-worker iptables -t nat -A POSTROUTING -s 10.244.1.0/24 ! -d 10.244.0.0/16 -j MASQUERADE
