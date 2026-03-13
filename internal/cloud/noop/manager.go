package noop

import (
	"context"
	"fmt"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
	"github.com/YangKeao/tiproxy-machine-operator/internal/cloud"
)

var _ cloud.Manager = (*Manager)(nil)

type Manager struct{}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) EnsureMachineGroup(
	_ context.Context,
	mg *tiproxyv1alpha1.TiProxyMachineGroup,
	machinePorts []tiproxyv1alpha1.TiProxyMachinePort,
	allocatedPorts map[string]int32,
) (tiproxyv1alpha1.CloudStatus, map[string]tiproxyv1alpha1.MachinePortCloudStatus, error) {
	lt, asg := cloud.ResolveNames(mg)
	loadBalancers, err := mg.Spec.Exposure.NormalizeLoadBalancers()
	if err != nil {
		return tiproxyv1alpha1.CloudStatus{}, nil, err
	}

	status := tiproxyv1alpha1.CloudStatus{
		MachineGroupName:    asg,
		MachineTemplateName: lt,
	}
	cloudByUID := map[string]tiproxyv1alpha1.MachinePortCloudStatus{}
	if len(loadBalancers) == 0 {
		return status, cloudByUID, nil
	}

	for _, loadBalancer := range loadBalancers {
		status.LoadBalancers = append(status.LoadBalancers, tiproxyv1alpha1.LoadBalancerCloudStatus{
			Name:                loadBalancer.Name,
			Scope:               loadBalancer.Scope,
			LoadBalancerID:      fmt.Sprintf("noop://%s/%s", mg.Name, loadBalancer.Name),
			LoadBalancerAddress: fmt.Sprintf("%s.noop.local", loadBalancer.Name),
		})
	}
	status.SetPrimaryLoadBalancerCompat()

	for i := range machinePorts {
		uid := string(machinePorts[i].UID)
		port, ok := allocatedPorts[uid]
		if !ok || port <= 0 {
			continue
		}
		entry := tiproxyv1alpha1.MachinePortCloudStatus{}
		for _, loadBalancer := range status.LoadBalancers {
			entry.LoadBalancers = append(entry.LoadBalancers, tiproxyv1alpha1.MachinePortLoadBalancerCloudStatus{
				Name:                loadBalancer.Name,
				Scope:               loadBalancer.Scope,
				LoadBalancerID:      loadBalancer.LoadBalancerID,
				LoadBalancerAddress: loadBalancer.LoadBalancerAddress,
				ListenerID:          fmt.Sprintf("noop-listener://%s/%d", loadBalancer.Name, port),
				BackendID:           fmt.Sprintf("noop-backend://%s/%d", loadBalancer.Name, port),
			})
		}
		entry.SetPrimaryLoadBalancerCompat()
		cloudByUID[uid] = entry
	}
	return status, cloudByUID, nil
}

func (m *Manager) DeleteMachineGroup(_ context.Context, _ *tiproxyv1alpha1.TiProxyMachineGroup) error {
	return nil
}
