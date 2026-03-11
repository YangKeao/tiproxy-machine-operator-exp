package cloud

import (
	"context"
	"fmt"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
)

type Manager interface {
	EnsureMachineGroup(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup) (tiproxyv1alpha1.CloudStatus, error)
	DeleteMachineGroup(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup) error
}

func ResolveNames(mg *tiproxyv1alpha1.TiProxyMachineGroup) (launchTemplateName, asgName string) {
	launchTemplateName = mg.Spec.Infrastructure.MachineTemplateName
	if launchTemplateName == "" {
		launchTemplateName = fmt.Sprintf("%s-%s-lt", mg.Namespace, mg.Name)
	}
	asgName = mg.Spec.Infrastructure.ScalingGroupName
	if asgName == "" {
		asgName = fmt.Sprintf("%s-%s-asg", mg.Namespace, mg.Name)
	}
	return launchTemplateName, asgName
}
