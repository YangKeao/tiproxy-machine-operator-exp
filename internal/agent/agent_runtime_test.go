package agent

import (
	"testing"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
)

func TestResolveTiProxyImage(t *testing.T) {
	cases := []struct {
		name     string
		spec     tiproxyv1alpha1.TiProxyRuntimeSpec
		fallback string
		expect   string
	}{
		{
			name: "explicit image",
			spec: tiproxyv1alpha1.TiProxyRuntimeSpec{
				Image: "repo/tiproxy:v9.0.0",
			},
			fallback: "pingcap/tiproxy:latest",
			expect:   "repo/tiproxy:v9.0.0",
		},
		{
			name: "base image plus version",
			spec: tiproxyv1alpha1.TiProxyRuntimeSpec{
				BaseImage: "repo/tiproxy",
				Version:   "v9.0.1",
			},
			fallback: "pingcap/tiproxy:latest",
			expect:   "repo/tiproxy:v9.0.1",
		},
		{
			name: "version with fallback base",
			spec: tiproxyv1alpha1.TiProxyRuntimeSpec{
				Version: "v9.0.2",
			},
			fallback: "pingcap/tiproxy:latest",
			expect:   "pingcap/tiproxy:v9.0.2",
		},
		{
			name: "only base image",
			spec: tiproxyv1alpha1.TiProxyRuntimeSpec{
				BaseImage: "repo/tiproxy",
			},
			fallback: "pingcap/tiproxy:latest",
			expect:   "repo/tiproxy",
		},
		{
			name:     "fallback",
			spec:     tiproxyv1alpha1.TiProxyRuntimeSpec{},
			fallback: "pingcap/tiproxy:latest",
			expect:   "pingcap/tiproxy:latest",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			actual := resolveTiProxyImage(c.spec, c.fallback)
			if actual != c.expect {
				t.Fatalf("expected %q, got %q", c.expect, actual)
			}
		})
	}
}
