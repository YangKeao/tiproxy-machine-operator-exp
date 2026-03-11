package aws

import (
	"testing"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
)

func TestTiProxyImageSpecHash(t *testing.T) {
	h1 := tiproxyImageSpecHash(tiproxyv1alpha1.TiProxyRuntimeSpec{
		BaseImage: "repo/tiproxy",
		Version:   "v1",
	})
	h2 := tiproxyImageSpecHash(tiproxyv1alpha1.TiProxyRuntimeSpec{
		BaseImage: "repo/tiproxy",
		Version:   "v2",
	})
	if h1 == "" || h2 == "" {
		t.Fatalf("expected non-empty hashes, got h1=%q h2=%q", h1, h2)
	}
	if h1 == h2 {
		t.Fatalf("expected different hashes when image spec changes, got %q", h1)
	}

	empty := tiproxyImageSpecHash(tiproxyv1alpha1.TiProxyRuntimeSpec{})
	if empty != "" {
		t.Fatalf("expected empty hash for empty spec, got %q", empty)
	}
}

func TestASGTagValue(t *testing.T) {
	tags := []autoscalingtypes.TagDescription{
		{Key: strPtr("k1"), Value: strPtr("v1")},
		{Key: strPtr(asgTagTiProxyImageHash), Value: strPtr("abcd")},
	}
	if got := asgTagValue(tags, asgTagTiProxyImageHash); got != "abcd" {
		t.Fatalf("expected abcd, got %q", got)
	}
	if got := asgTagValue(tags, "missing"); got != "" {
		t.Fatalf("expected empty string for missing tag, got %q", got)
	}
}

func strPtr(s string) *string {
	return &s
}
