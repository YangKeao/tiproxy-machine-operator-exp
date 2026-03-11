package portallocator

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestReconcilePorts(t *testing.T) {
	links := []LinkPortRequest{
		{UID: types.UID("a"), Name: "ns/a"},
		{UID: types.UID("b"), Name: "ns/b"},
		{UID: types.UID("c"), Name: "ns/c"},
	}
	existing := map[string]int32{
		"a": 4001,
		"b": 4999,
	}

	got, err := ReconcilePorts(existing, links, 4000, 4002)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["a"] != 4001 {
		t.Fatalf("expected a=4001, got %d", got["a"])
	}
	if got["b"] != 4000 {
		t.Fatalf("expected b=4000, got %d", got["b"])
	}
	if got["c"] != 4002 {
		t.Fatalf("expected c=4002, got %d", got["c"])
	}
}

func TestReconcilePortsInsufficient(t *testing.T) {
	links := []LinkPortRequest{
		{UID: types.UID("a"), Name: "ns/a"},
		{UID: types.UID("b"), Name: "ns/b"},
	}
	_, err := ReconcilePorts(nil, links, 6000, 6000)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestReconcilePortsRequested(t *testing.T) {
	reqPort := int32(4002)
	links := []LinkPortRequest{
		{UID: types.UID("a"), Name: "ns/a"},
		{UID: types.UID("b"), Name: "ns/b", RequestedPort: &reqPort},
	}

	got, err := ReconcilePorts(map[string]int32{"a": 4001}, links, 4000, 4002)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["b"] != 4002 {
		t.Fatalf("expected b=4002, got %d", got["b"])
	}
	if got["a"] != 4001 {
		t.Fatalf("expected a=4001, got %d", got["a"])
	}
}

func TestReconcilePortsRequestedConflict(t *testing.T) {
	port := int32(6001)
	links := []LinkPortRequest{
		{UID: types.UID("a"), Name: "default/link-a", RequestedPort: &port},
		{UID: types.UID("b"), Name: "default/link-b", RequestedPort: &port},
	}

	_, err := ReconcilePorts(nil, links, 6000, 6099)
	if err == nil {
		t.Fatalf("expected conflict error, got nil")
	}
}

func TestReconcilePortsRequestedOutOfRange(t *testing.T) {
	port := int32(7000)
	links := []LinkPortRequest{
		{UID: types.UID("a"), Name: "default/link-a", RequestedPort: &port},
	}

	_, err := ReconcilePorts(nil, links, 6000, 6099)
	if err == nil {
		t.Fatalf("expected out-of-range error, got nil")
	}
}
