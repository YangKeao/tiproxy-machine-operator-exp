package v1alpha1

import "testing"

func TestExposureSpecNormalizeLoadBalancersDefault(t *testing.T) {
	loadBalancers, err := (ExposureSpec{}).NormalizeLoadBalancers()
	if err != nil {
		t.Fatalf("NormalizeLoadBalancers(): %v", err)
	}
	if len(loadBalancers) != 1 {
		t.Fatalf("expected 1 default load balancer, got %d", len(loadBalancers))
	}
	if loadBalancers[0].Name != "internal" {
		t.Fatalf("expected default load balancer name internal, got %q", loadBalancers[0].Name)
	}
	if loadBalancers[0].Scope != "internal" {
		t.Fatalf("expected default load balancer scope internal, got %q", loadBalancers[0].Scope)
	}
}

func TestExposureSpecNormalizeLoadBalancersExplicit(t *testing.T) {
	loadBalancers, err := (ExposureSpec{
		LoadBalancers: []LoadBalancerExposureSpec{
			{Name: "internal", Scope: "internal"},
			{Name: "public", Scope: "external", SubnetIDs: []string{"subnet-a", "subnet-b"}},
		},
	}).NormalizeLoadBalancers()
	if err != nil {
		t.Fatalf("NormalizeLoadBalancers(): %v", err)
	}
	if len(loadBalancers) != 2 {
		t.Fatalf("expected 2 load balancers, got %d", len(loadBalancers))
	}
	if loadBalancers[1].Name != "public" || loadBalancers[1].Scope != "external" {
		t.Fatalf("unexpected second load balancer: %#v", loadBalancers[1])
	}
	if len(loadBalancers[1].SubnetIDs) != 2 {
		t.Fatalf("expected subnet override to be preserved, got %#v", loadBalancers[1].SubnetIDs)
	}
}

func TestExposureSpecNormalizeLoadBalancersExplicitEmptyDisablesExposure(t *testing.T) {
	loadBalancers, err := (ExposureSpec{
		LoadBalancers: []LoadBalancerExposureSpec{},
	}).NormalizeLoadBalancers()
	if err != nil {
		t.Fatalf("NormalizeLoadBalancers(): %v", err)
	}
	if len(loadBalancers) != 0 {
		t.Fatalf("expected explicit empty load balancers to disable exposure, got %d entries", len(loadBalancers))
	}
}

func TestExposureSpecNormalizeLoadBalancersRejectsDuplicateNames(t *testing.T) {
	_, err := (ExposureSpec{
		LoadBalancers: []LoadBalancerExposureSpec{
			{Name: "public", Scope: "external"},
			{Name: "public", Scope: "internal"},
		},
	}).NormalizeLoadBalancers()
	if err == nil {
		t.Fatalf("expected duplicate load balancer names to be rejected")
	}
}

func TestExposureSpecNormalizeLoadBalancersRejectsInvalidScope(t *testing.T) {
	_, err := (ExposureSpec{
		LoadBalancers: []LoadBalancerExposureSpec{
			{Name: "public", Scope: "dmz"},
		},
	}).NormalizeLoadBalancers()
	if err == nil {
		t.Fatalf("expected invalid load balancer scope to be rejected")
	}
}
