package config

import (
	"strings"
	"testing"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
)

func TestBuildTiProxyConfig(t *testing.T) {
	cfg, err := BuildTiProxyConfig(RenderOptions{
		ResolvedLinks: []tiproxyv1alpha1.ResolvedLinkStatus{
			{
				Name:         "link-a",
				Namespace:    "ns1",
				Port:         6001,
				PDAddresses:  []string{"10.0.0.1:2379", "10.0.0.2:2379"},
				NSServerAddr: "10.0.10.1:53,10.0.10.2",
			},
			{
				Name:        "link-b",
				Namespace:   "ns1",
				ClusterName: "cluster-b",
				Port:        6000,
				PDAddresses: []string{"10.0.1.1:2379"},
			},
		},
		APIPort:        3080,
		PortRangeStart: 6000,
		PortRangeEnd:   6099,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := string(cfg)
	if !strings.Contains(s, `addr = "0.0.0.0:6000"`) {
		t.Fatalf("unexpected proxy addr: %s", s)
	}
	if !strings.Contains(s, `port-range = [6000, 6099]`) {
		t.Fatalf("unexpected port-range config: %s", s)
	}
	if !strings.Contains(s, `routing-rule = "port"`) {
		t.Fatalf("unexpected routing-rule config: %s", s)
	}
	if !strings.Contains(s, `name = "ns1.link-a"`) {
		t.Fatalf("missing default cluster name: %s", s)
	}
	if !strings.Contains(s, `name = "cluster-b"`) {
		t.Fatalf("missing explicit cluster name: %s", s)
	}
	if !strings.Contains(s, `pd-addrs = "10.0.0.1:2379,10.0.0.2:2379"`) {
		t.Fatalf("missing pd-addrs aggregation: %s", s)
	}
	if !strings.Contains(s, `ns-servers = "10.0.10.1:53,10.0.10.2"`) {
		t.Fatalf("missing ns-servers: %s", s)
	}
	if !strings.Contains(s, `addr = "0.0.0.0:3080"`) {
		t.Fatalf("unexpected api addr config: %s", s)
	}
}

func TestBuildTiProxyConfigDefaultListenPort(t *testing.T) {
	cfg, err := BuildTiProxyConfig(RenderOptions{
		DefaultListenPort: 7000,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(cfg), `addr = "0.0.0.0:7000"`) {
		t.Fatalf("default listen port not rendered: %s", string(cfg))
	}
	if !strings.Contains(string(cfg), `backend-clusters = []`) {
		t.Fatalf("empty backend-clusters not rendered: %s", string(cfg))
	}
}

func TestBuildTiProxyConfigDuplicateCluster(t *testing.T) {
	_, err := BuildTiProxyConfig(RenderOptions{
		ResolvedLinks: []tiproxyv1alpha1.ResolvedLinkStatus{
			{
				Name:        "l1",
				Namespace:   "ns",
				ClusterName: "same",
				Port:        6000,
				PDAddresses: []string{"10.0.0.1:2379"},
			},
			{
				Name:        "l2",
				Namespace:   "ns",
				ClusterName: "same",
				Port:        6001,
				PDAddresses: []string{"10.0.0.2:2379"},
			},
		},
		PortRangeStart: 6000,
		PortRangeEnd:   6001,
	})
	if err == nil {
		t.Fatal("expected duplicate cluster error, got nil")
	}
}
