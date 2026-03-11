package noop

import (
	"context"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
	"github.com/YangKeao/tiproxy-machine-operator/internal/cloud"
)

var _ cloud.Manager = (*Manager)(nil)

type Manager struct{}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) EnsureMachineGroup(_ context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup) (tiproxyv1alpha1.CloudStatus, error) {
	lt, asg := cloud.ResolveNames(mg)
	return tiproxyv1alpha1.CloudStatus{
		MachineGroupName:    asg,
		MachineTemplateName: lt,
	}, nil
}

func (m *Manager) DeleteMachineGroup(_ context.Context, _ *tiproxyv1alpha1.TiProxyMachineGroup) error {
	return nil
}
