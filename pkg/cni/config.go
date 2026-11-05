package cni

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/containernetworking/cni/pkg/types"
)

// PodMeta holds Kubernetes pod metadata parsed from CNI_ARGS.
type PodMeta struct {
	PodName      string
	PodNamespace string
	PodUID       string
}

// ParseCNIArgs parses the semi-colon delimited CNI_ARGS string provided by the CRI runtime.
func ParseCNIArgs(args string) *PodMeta {
	meta := &PodMeta{}
	if args == "" {
		return meta
	}
	pairs := strings.Split(args, ";")
	for _, pair := range pairs {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			k := strings.TrimSpace(kv[0])
			v := strings.TrimSpace(kv[1])
			switch k {
			case "K8S_POD_NAME":
				meta.PodName = v
			case "K8S_POD_NAMESPACE":
				meta.PodNamespace = v
			case "K8S_POD_UID":
				meta.PodUID = v
			}
		}
	}
	return meta
}

// PluginConf represents the CNI network configuration for mycni.
type PluginConf struct {
	types.NetConf
	Bridge  string `json:"bridge"`
	MTU     int    `json:"mtu"`
	PodCIDR string `json:"podCIDR"`
	DataDir string `json:"dataDir"`

	PodIPNet *net.IPNet `json:"-"`
}

// ParseConfig parses the CNI configuration JSON from stdin bytes.
func ParseConfig(stdin []byte) (*PluginConf, error) {
	conf := &PluginConf{
		Bridge:  "mycni0",
		MTU:     1500,
		DataDir: "/var/lib/cni/mycni",
	}

	if err := json.Unmarshal(stdin, conf); err != nil {
		return nil, fmt.Errorf("failed to parse CNI config: %w", err)
	}

	if conf.Bridge == "" {
		conf.Bridge = "mycni0"
	}
	if conf.MTU <= 0 {
		conf.MTU = 1500
	}
	if conf.DataDir == "" {
		conf.DataDir = "/var/lib/cni/mycni"
	}
	if conf.PodCIDR == "" {
		return nil, fmt.Errorf("missing podCIDR in CNI config")
	}

	_, ipnet, err := net.ParseCIDR(conf.PodCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid podCIDR %q: %w", conf.PodCIDR, err)
	}
	conf.PodIPNet = ipnet

	return conf, nil
}
