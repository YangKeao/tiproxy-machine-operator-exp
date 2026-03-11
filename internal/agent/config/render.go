package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
)

type RenderOptions struct {
	ResolvedLinks     []tiproxyv1alpha1.ResolvedLinkStatus
	APIPort           int32
	ListenHost        string
	PortRangeStart    int32
	PortRangeEnd      int32
	DefaultListenPort int32
}

func BuildTiProxyConfig(opts RenderOptions) ([]byte, error) {
	apiPort := opts.APIPort
	if apiPort == 0 {
		apiPort = 3080
	}
	if apiPort <= 0 {
		return nil, fmt.Errorf("invalid api port %d", apiPort)
	}

	listenHost := strings.TrimSpace(opts.ListenHost)
	if listenHost == "" {
		listenHost = "0.0.0.0"
	}

	clusters, err := buildBackendClusters(opts.ResolvedLinks)
	if err != nil {
		return nil, err
	}

	rangeStart := opts.PortRangeStart
	rangeEnd := opts.PortRangeEnd
	if rangeStart <= 0 || rangeEnd < rangeStart {
		rangeStart, rangeEnd = inferPortRange(opts.ResolvedLinks)
	}
	if rangeStart <= 0 || rangeEnd < rangeStart {
		if opts.DefaultListenPort <= 0 {
			opts.DefaultListenPort = 6000
		}
		rangeStart = opts.DefaultListenPort
		rangeEnd = opts.DefaultListenPort
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[proxy]\n")
	fmt.Fprintf(&b, "addr = %q\n", fmt.Sprintf("%s:%d", listenHost, rangeStart))
	fmt.Fprintf(&b, "port-range = [%d, %d]\n", rangeStart, rangeEnd)
	if len(clusters) == 0 {
		// Explicitly clear old backend-clusters in TiProxy's partial TOML update semantics.
		fmt.Fprintf(&b, "backend-clusters = []\n")
	} else {
		for _, cluster := range clusters {
			fmt.Fprintf(&b, "\n[[proxy.backend-clusters]]\n")
			fmt.Fprintf(&b, "name = %q\n", cluster.Name)
			fmt.Fprintf(&b, "pd-addrs = %q\n", cluster.PDAddrs)
			if cluster.NSServers != "" {
				fmt.Fprintf(&b, "ns-servers = %q\n", cluster.NSServers)
			}
		}
	}
	fmt.Fprintf(&b, "\n[balance]\n")
	fmt.Fprintf(&b, "routing-rule = %q\n", "port")
	fmt.Fprintf(&b, "\n[api]\n")
	fmt.Fprintf(&b, "addr = %q\n", fmt.Sprintf("0.0.0.0:%d", apiPort))
	return []byte(b.String()), nil
}

func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type backendCluster struct {
	Name      string
	PDAddrs   string
	NSServers string
}

func buildBackendClusters(links []tiproxyv1alpha1.ResolvedLinkStatus) ([]backendCluster, error) {
	clusters := make([]backendCluster, 0, len(links))
	names := make(map[string]struct{}, len(links))
	for _, link := range links {
		if link.Port <= 0 {
			return nil, fmt.Errorf("link %s/%s has invalid assigned port %d", link.Namespace, link.Name, link.Port)
		}
		clusterName := strings.TrimSpace(link.ClusterName)
		if clusterName == "" {
			clusterName = defaultClusterName(link)
		}
		if _, ok := names[clusterName]; ok {
			return nil, fmt.Errorf("duplicate cluster name %q in resolved links", clusterName)
		}
		names[clusterName] = struct{}{}

		pdAddrs := make([]string, 0, len(link.PDAddresses))
		for _, addr := range link.PDAddresses {
			addr = strings.TrimSpace(addr)
			if addr != "" {
				pdAddrs = append(pdAddrs, addr)
			}
		}
		if len(pdAddrs) == 0 {
			return nil, fmt.Errorf("cluster %q has empty pd addresses", clusterName)
		}

		clusters = append(clusters, backendCluster{
			Name:      clusterName,
			PDAddrs:   strings.Join(pdAddrs, ","),
			NSServers: strings.TrimSpace(link.NSServerAddr),
		})
	}
	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].Name < clusters[j].Name
	})
	return clusters, nil
}

func defaultClusterName(link tiproxyv1alpha1.ResolvedLinkStatus) string {
	if link.Namespace == "" {
		return link.Name
	}
	return fmt.Sprintf("%s.%s", link.Namespace, link.Name)
}

func inferPortRange(links []tiproxyv1alpha1.ResolvedLinkStatus) (int32, int32) {
	var start int32
	var end int32
	for _, link := range links {
		if link.Port <= 0 {
			continue
		}
		if start == 0 || link.Port < start {
			start = link.Port
		}
		if end == 0 || link.Port > end {
			end = link.Port
		}
	}
	return start, end
}

func ClusterName(link tiproxyv1alpha1.ResolvedLinkStatus) string {
	if strings.TrimSpace(link.ClusterName) != "" {
		return strings.TrimSpace(link.ClusterName)
	}
	return defaultClusterName(link)
}
