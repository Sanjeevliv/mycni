FROM golang:1.26-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
    iptables \
    iproute2 \
    kmod \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY bin/mycni-cni /bin/mycni-cni
COPY bin/mycni-agent /bin/mycni-agent
COPY bin/mycni-e2e /bin/mycni-e2e
COPY bin/mycni-e2e /usr/local/bin/ping
COPY bin/mycni-e2e /usr/local/bin/nslookup

ENTRYPOINT ["/bin/mycni-agent"]
