package aws

import (
	"testing"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
	"github.com/aws/aws-sdk-go-v2/aws"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
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

func TestSelectPlacementSubnetsFiltersFailureDomains(t *testing.T) {
	selection, err := selectPlacementSubnets(
		[]string{"subnet-a", "subnet-b", "subnet-c"},
		[]ec2types.Subnet{
			{SubnetId: aws.String("subnet-a"), VpcId: aws.String("vpc-1"), AvailabilityZone: aws.String("us-east-1a")},
			{SubnetId: aws.String("subnet-b"), VpcId: aws.String("vpc-1"), AvailabilityZone: aws.String("us-east-1b")},
			{SubnetId: aws.String("subnet-c"), VpcId: aws.String("vpc-1"), AvailabilityZone: aws.String("us-east-1c")},
		},
		[]string{"us-east-1c", "us-east-1a"},
	)
	if err != nil {
		t.Fatalf("select placement subnets: %v", err)
	}
	if got, want := selection.VPCID, "vpc-1"; got != want {
		t.Fatalf("unexpected vpc id: got %q want %q", got, want)
	}
	if len(selection.SubnetIDs) != 2 || selection.SubnetIDs[0] != "subnet-a" || selection.SubnetIDs[1] != "subnet-c" {
		t.Fatalf("unexpected selected subnets: %#v", selection.SubnetIDs)
	}
	if len(selection.AvailabilityZones) != 2 || selection.AvailabilityZones[0] != "us-east-1a" || selection.AvailabilityZones[1] != "us-east-1c" {
		t.Fatalf("unexpected availability zones: %#v", selection.AvailabilityZones)
	}
}

func TestSelectPlacementSubnetsRequiresAllFailureDomains(t *testing.T) {
	_, err := selectPlacementSubnets(
		[]string{"subnet-a"},
		[]ec2types.Subnet{
			{SubnetId: aws.String("subnet-a"), VpcId: aws.String("vpc-1"), AvailabilityZone: aws.String("us-east-1a")},
		},
		[]string{"us-east-1a", "us-east-1b"},
	)
	if err == nil {
		t.Fatalf("expected error for missing failure domain")
	}
}

func TestAvailabilityZoneDistributionForSpreadPolicy(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		want   autoscalingtypes.CapacityDistributionStrategy
		ok     bool
	}{
		{name: "unset", policy: "", ok: false},
		{name: "balanced", policy: "Balanced", want: autoscalingtypes.CapacityDistributionStrategyBalancedBestEffort, ok: true},
		{name: "best effort", policy: "balanced-best-effort", want: autoscalingtypes.CapacityDistributionStrategyBalancedBestEffort, ok: true},
		{name: "balanced only", policy: "balanced-only", want: autoscalingtypes.CapacityDistributionStrategyBalancedOnly, ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := availabilityZoneDistributionForSpreadPolicy(tc.policy)
			if err != nil {
				t.Fatalf("availabilityZoneDistributionForSpreadPolicy(%q): %v", tc.policy, err)
			}
			if !tc.ok {
				if got != nil {
					t.Fatalf("expected nil distribution, got %#v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected distribution for %q", tc.policy)
			}
			if got.CapacityDistributionStrategy != tc.want {
				t.Fatalf("unexpected strategy for %q: got %q want %q", tc.policy, got.CapacityDistributionStrategy, tc.want)
			}
		})
	}
}

func TestAvailabilityZoneDistributionForSpreadPolicyRejectsUnknownValue(t *testing.T) {
	if _, err := availabilityZoneDistributionForSpreadPolicy("random"); err == nil {
		t.Fatalf("expected error for unsupported spread policy")
	}
}

func strPtr(s string) *string {
	return &s
}
