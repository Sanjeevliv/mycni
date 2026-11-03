package cni

import (
	"testing"
)

func TestParseConfigValid(t *testing.T) {
	input := []byte(`{
		"cniVersion": "1.0.0",
		"name": "mycni",
		"type": "mycni-cni",
		"bridge": "mycni0",
		"mtu": 1500,
		"podCIDR": "10.244.1.0/24",
		"dataDir": "/var/lib/cni/mycni"
	}`)

	conf, err := ParseConfig(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if conf.CNIVersion != "1.0.0" {
		t.Errorf("expected cniVersion 1.0.0, got %s", conf.CNIVersion)
	}
	if conf.Name != "mycni" {
		t.Errorf("expected name mycni, got %s", conf.Name)
	}
	if conf.Bridge != "mycni0" {
		t.Errorf("expected bridge mycni0, got %s", conf.Bridge)
	}
	if conf.MTU != 1500 {
		t.Errorf("expected mtu 1500, got %d", conf.MTU)
	}
	if conf.PodCIDR != "10.244.1.0/24" {
		t.Errorf("expected podCIDR 10.244.1.0/24, got %s", conf.PodCIDR)
	}
	if conf.PodIPNet == nil || conf.PodIPNet.String() != "10.244.1.0/24" {
		t.Errorf("expected parsed podIPNet 10.244.1.0/24, got %v", conf.PodIPNet)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	input := []byte(`{
		"cniVersion": "1.0.0",
		"name": "mycni",
		"type": "mycni-cni",
		"podCIDR": "10.244.2.0/24"
	}`)

	conf, err := ParseConfig(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if conf.Bridge != "mycni0" {
		t.Errorf("expected default bridge mycni0, got %s", conf.Bridge)
	}
	if conf.MTU != 1500 {
		t.Errorf("expected default mtu 1500, got %d", conf.MTU)
	}
	if conf.DataDir != "/var/lib/cni/mycni" {
		t.Errorf("expected default dataDir /var/lib/cni/mycni, got %s", conf.DataDir)
	}
}

func TestParseConfigMissingPodCIDR(t *testing.T) {
	input := []byte(`{
		"cniVersion": "1.0.0",
		"name": "mycni",
		"type": "mycni-cni"
	}`)

	_, err := ParseConfig(input)
	if err == nil {
		t.Fatal("expected error for missing podCIDR, got nil")
	}
}

func TestParseConfigInvalidCIDR(t *testing.T) {
	input := []byte(`{
		"cniVersion": "1.0.0",
		"name": "mycni",
		"type": "mycni-cni",
		"podCIDR": "not-a-cidr"
	}`)

	_, err := ParseConfig(input)
	if err == nil {
		t.Fatal("expected error for invalid podCIDR, got nil")
	}
}
