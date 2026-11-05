FROM golang:1.26-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
    iptables \
    iproute2 \
    kmod \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY bin/mycni-cni /bin/mycni-cni
COPY bin/mycni-agent /bin/mycni-agent

ENTRYPOINT ["/bin/mycni-agent"]
