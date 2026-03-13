package agent

import (
	"context"
	"testing"
	"time"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestUpsertMachineStatusOnMachineGroup(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := tiproxyv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	mg := &tiproxyv1alpha1.TiProxyMachineGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "mg",
			Namespace:  "default",
			Generation: 3,
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&tiproxyv1alpha1.TiProxyMachineGroup{}).
		WithObjects(mg).
		Build()

	a := &Agent{
		Client:       cl,
		Namespace:    "default",
		MachineGroup: "mg",
		MachineID:    "i-test",
	}

	if err := a.upsertMachineStatus(context.Background(), tiproxyv1alpha1.MachinePhaseRunning, "img:v1", "cfg1", "ok"); err != nil {
		t.Fatalf("upsert machine status: %v", err)
	}

	got := &tiproxyv1alpha1.TiProxyMachineGroup{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mg"}, got); err != nil {
		t.Fatalf("get machine group: %v", err)
	}
	status, ok := got.Status.Machines["i-test"]
	if !ok {
		t.Fatalf("machine status not found")
	}
	if status.Phase != tiproxyv1alpha1.MachinePhaseRunning {
		t.Fatalf("unexpected phase: %s", status.Phase)
	}
	if status.ConfigHash != "cfg1" {
		t.Fatalf("unexpected config hash: %s", status.ConfigHash)
	}
	if status.TiProxyImage != "img:v1" {
		t.Fatalf("unexpected image: %s", status.TiProxyImage)
	}
	if status.ObservedGeneration != 3 {
		t.Fatalf("unexpected observed generation: %d", status.ObservedGeneration)
	}
	if status.LastHeartbeatTime.IsZero() {
		t.Fatalf("last heartbeat should be set")
	}
}

func TestRemoveMachineStatusOnShutdown(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := tiproxyv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	mg := &tiproxyv1alpha1.TiProxyMachineGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mg",
			Namespace: "default",
		},
		Status: tiproxyv1alpha1.TiProxyMachineGroupStatus{
			Machines: map[string]tiproxyv1alpha1.MachineStatus{
				"i-test": {
					Phase:             tiproxyv1alpha1.MachinePhaseRunning,
					LastHeartbeatTime: metav1.NewTime(time.Now()),
				},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&tiproxyv1alpha1.TiProxyMachineGroup{}).
		WithObjects(mg).
		Build()

	a := &Agent{
		Client:       cl,
		Namespace:    "default",
		MachineGroup: "mg",
		MachineID:    "i-test",
	}

	if err := a.removeMachineStatus(context.Background()); err != nil {
		t.Fatalf("remove machine status: %v", err)
	}

	got := &tiproxyv1alpha1.TiProxyMachineGroup{}
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mg"}, got); err != nil {
		t.Fatalf("get machine group: %v", err)
	}
	if len(got.Status.Machines) != 0 {
		t.Fatalf("machine status should be removed, got=%v", got.Status.Machines)
	}
}
