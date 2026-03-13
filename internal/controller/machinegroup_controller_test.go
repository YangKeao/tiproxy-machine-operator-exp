package controller

import (
	"testing"
	"time"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestPruneStaleMachineStatuses(t *testing.T) {
	now := time.Now()
	mg := &tiproxyv1alpha1.TiProxyMachineGroup{
		Status: tiproxyv1alpha1.TiProxyMachineGroupStatus{
			Machines: map[string]tiproxyv1alpha1.MachineStatus{
				"fresh": {
					LastHeartbeatTime: metav1.NewTime(now.Add(-1 * time.Minute)),
				},
				"stale": {
					LastHeartbeatTime: metav1.NewTime(now.Add(-15 * time.Minute)),
				},
				"zero": {},
			},
		},
	}
	r := &MachineGroupReconciler{MachineStatusStaleAfter: 10 * time.Minute}
	removed := r.pruneStaleMachineStatuses(mg, now)
	if removed != 2 {
		t.Fatalf("expected 2 pruned entries, got %d", removed)
	}
	if len(mg.Status.Machines) != 1 {
		t.Fatalf("expected 1 remaining entry, got %d", len(mg.Status.Machines))
	}
	if _, ok := mg.Status.Machines["fresh"]; !ok {
		t.Fatalf("fresh machine should remain")
	}
}

func TestPruneStaleMachineStatusesDisabled(t *testing.T) {
	now := time.Now()
	mg := &tiproxyv1alpha1.TiProxyMachineGroup{
		Status: tiproxyv1alpha1.TiProxyMachineGroupStatus{
			Machines: map[string]tiproxyv1alpha1.MachineStatus{
				"stale": {
					LastHeartbeatTime: metav1.NewTime(now.Add(-24 * time.Hour)),
				},
			},
		},
	}
	r := &MachineGroupReconciler{MachineStatusStaleAfter: 0}
	removed := r.pruneStaleMachineStatuses(mg, now)
	if removed != 0 {
		t.Fatalf("expected 0 pruned entries when disabled, got %d", removed)
	}
	if len(mg.Status.Machines) != 1 {
		t.Fatalf("expected status to remain unchanged, got %d entries", len(mg.Status.Machines))
	}
}

func TestAssignPortsToLinksPrefersSameNameMachinePort(t *testing.T) {
	links := []tiproxyv1alpha1.TiDBResourcePoolLink{
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "link-a",
				UID:       types.UID("link-a"),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "link-b",
				UID:       types.UID("link-b"),
			},
		},
	}
	machinePorts := []tiproxyv1alpha1.TiProxyMachinePort{
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "link-a",
				UID:       types.UID("mp-a"),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "port-b",
				UID:       types.UID("mp-b"),
			},
		},
	}
	allocated := map[string]int32{
		"mp-a": 6001,
		"mp-b": 6002,
	}

	got, errs := assignPortsToLinks(links, machinePorts, allocated)
	if len(errs) != 0 {
		t.Fatalf("expected no link errors, got %v", errs)
	}
	if got["link-a"] != 6001 {
		t.Fatalf("expected link-a match same-name port 6001, got %d", got["link-a"])
	}
	if got["link-b"] != 6002 {
		t.Fatalf("expected link-b get remaining 6002, got %d", got["link-b"])
	}
}

func TestAssignPortsToLinksInsufficientMachinePorts(t *testing.T) {
	links := []tiproxyv1alpha1.TiDBResourcePoolLink{
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "link-a",
				UID:       types.UID("link-a"),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "link-b",
				UID:       types.UID("link-b"),
			},
		},
	}
	machinePorts := []tiproxyv1alpha1.TiProxyMachinePort{
		{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "link-a",
				UID:       types.UID("mp-a"),
			},
		},
	}
	allocated := map[string]int32{
		"mp-a": 6001,
	}

	got, errs := assignPortsToLinks(links, machinePorts, allocated)
	if len(got) != 1 {
		t.Fatalf("expected one assigned link, got %v", got)
	}
	if got["link-a"] != 6001 {
		t.Fatalf("expected link-a get 6001, got %d", got["link-a"])
	}
	if len(errs) != 1 || errs["link-b"] == "" {
		t.Fatalf("expected link-b unresolved error, got %v", errs)
	}
}
