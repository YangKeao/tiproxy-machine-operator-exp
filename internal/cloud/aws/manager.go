package aws

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tiproxyv1alpha1 "github.com/YangKeao/tiproxy-machine-operator/api/v1alpha1"
	"github.com/YangKeao/tiproxy-machine-operator/internal/cloud"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	autoscalingtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	elbv2 "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/smithy-go"
)

var _ cloud.Manager = (*Manager)(nil)

const (
	asgResourceTypeAutoScalingGroup = "auto-scaling-group"
	asgTagTiProxyImageHash          = "tiproxy.pingcap.com/tiproxy-image-hash"
	tagManagedBy                    = "tiproxy.pingcap.com/managed-by"
	managedByValue                  = "tiproxy-machine-operator"
)

type Manager struct {
	ec2   *ec2.Client
	asg   *autoscaling.Client
	elbv2 *elbv2.Client
}

type placementSelection struct {
	SubnetIDs                    []string
	AvailabilityZones            []string
	VPCID                        string
	AvailabilityZoneDistribution *autoscalingtypes.AvailabilityZoneDistribution
}

func NewManager(ctx context.Context, region string) (*Manager, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &Manager{
		ec2:   ec2.NewFromConfig(cfg),
		asg:   autoscaling.NewFromConfig(cfg),
		elbv2: elbv2.NewFromConfig(cfg),
	}, nil
}

func (m *Manager) EnsureMachineGroup(
	ctx context.Context,
	mg *tiproxyv1alpha1.TiProxyMachineGroup,
	machinePorts []tiproxyv1alpha1.TiProxyMachinePort,
	allocatedPorts map[string]int32,
) (tiproxyv1alpha1.CloudStatus, map[string]tiproxyv1alpha1.MachinePortCloudStatus, error) {
	ltName, asgName := cloud.ResolveNames(mg)
	desiredImageHash := tiproxyImageSpecHash(mg.Spec.TiProxy)
	loadBalancers, err := mg.Spec.Exposure.NormalizeLoadBalancers()
	if err != nil {
		return tiproxyv1alpha1.CloudStatus{}, nil, err
	}
	placement, err := m.resolvePlacement(ctx, mg)
	if err != nil {
		return tiproxyv1alpha1.CloudStatus{}, nil, err
	}

	ltID, ltVersion, err := m.ensureLaunchTemplate(ctx, mg, ltName)
	if err != nil {
		return tiproxyv1alpha1.CloudStatus{}, nil, err
	}
	if err := m.ensureAutoScalingGroup(ctx, mg, asgName, ltName, ltVersion, desiredImageHash, placement); err != nil {
		return tiproxyv1alpha1.CloudStatus{}, nil, err
	}

	status := tiproxyv1alpha1.CloudStatus{
		MachineGroupName:    asgName,
		MachineTemplateID:   ltID,
		MachineTemplateName: ltName,
		MachineTemplateVer:  ltVersion,
	}

	if len(loadBalancers) == 0 {
		if err := m.ensureLoadBalancersDeleted(ctx, mg); err != nil {
			return status, nil, err
		}
		return status, map[string]tiproxyv1alpha1.MachinePortCloudStatus{}, nil
	}

	loadBalancerStatus, machinePortCloud, err := m.ensureLoadBalancerExposure(ctx, mg, asgName, machinePorts, allocatedPorts, placement, loadBalancers)
	if err != nil {
		return status, nil, err
	}
	status.LoadBalancers = loadBalancerStatus
	status.SetPrimaryLoadBalancerCompat()
	if err := m.ensureStaleLoadBalancersDeleted(ctx, mg, loadBalancers); err != nil {
		return status, nil, err
	}
	return status, machinePortCloud, nil
}

func (m *Manager) DeleteMachineGroup(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup) error {
	ltName, asgName := cloud.ResolveNames(mg)

	if err := m.ensureLoadBalancersDeleted(ctx, mg); err != nil {
		return err
	}

	_, err := m.asg.DeleteAutoScalingGroup(ctx, &autoscaling.DeleteAutoScalingGroupInput{
		AutoScalingGroupName: aws.String(asgName),
		ForceDelete:          aws.Bool(true),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete asg %s: %w", asgName, err)
	}

	_, err = m.ec2.DeleteLaunchTemplate(ctx, &ec2.DeleteLaunchTemplateInput{
		LaunchTemplateName: aws.String(ltName),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete launch template %s: %w", ltName, err)
	}
	return nil
}

func (m *Manager) ensureLoadBalancerExposure(
	ctx context.Context,
	mg *tiproxyv1alpha1.TiProxyMachineGroup,
	asgName string,
	machinePorts []tiproxyv1alpha1.TiProxyMachinePort,
	allocated map[string]int32,
	placement placementSelection,
	loadBalancers []tiproxyv1alpha1.LoadBalancerExposureSpec,
) ([]tiproxyv1alpha1.LoadBalancerCloudStatus, map[string]tiproxyv1alpha1.MachinePortCloudStatus, error) {
	if len(placement.SubnetIDs) == 0 {
		return nil, nil, fmt.Errorf("spec.network.subnetIDs is required for loadBalancer exposure")
	}

	type desiredPort struct {
		UID      string
		Port     int32
		Protocol string
	}
	desiredByPort := map[int32]desiredPort{}
	for i := range machinePorts {
		uid := string(machinePorts[i].UID)
		port, ok := allocated[uid]
		if !ok || port <= 0 {
			continue
		}
		protocol := strings.ToUpper(strings.TrimSpace(machinePorts[i].Spec.Protocol))
		if protocol == "" {
			protocol = "TCP"
		}
		if protocol != "TCP" {
			return nil, nil, fmt.Errorf("unsupported protocol %q on TiProxyMachinePort %s/%s", protocol, machinePorts[i].Namespace, machinePorts[i].Name)
		}
		if prev, conflict := desiredByPort[port]; conflict {
			return nil, nil, fmt.Errorf("allocated port %d conflicts between machinePort UID %s and %s", port, prev.UID, uid)
		}
		desiredByPort[port] = desiredPort{
			UID:      uid,
			Port:     port,
			Protocol: protocol,
		}
	}

	managedTGARNs := map[string]struct{}{}
	desiredTGARNs := make([]string, 0, len(desiredByPort))
	cloudStatuses := make(map[string]tiproxyv1alpha1.MachinePortCloudStatus, len(desiredByPort))
	loadBalancerStatuses := make([]tiproxyv1alpha1.LoadBalancerCloudStatus, 0, len(loadBalancers))

	for i := range loadBalancers {
		loadBalancer := loadBalancers[i]
		loadBalancerPlacement, err := m.resolveLoadBalancerPlacement(ctx, mg, loadBalancer, placement)
		if err != nil {
			return nil, nil, err
		}
		lb, err := m.ensureNetworkLoadBalancer(ctx, mg, loadBalancer, loadBalancerPlacement)
		if err != nil {
			return nil, nil, err
		}
		loadBalancerStatuses = append(loadBalancerStatuses, tiproxyv1alpha1.LoadBalancerCloudStatus{
			Name:                loadBalancer.Name,
			Scope:               loadBalancer.Scope,
			LoadBalancerID:      aws.ToString(lb.LoadBalancerArn),
			LoadBalancerAddress: aws.ToString(lb.DNSName),
		})

		targetGroups, err := m.describeTargetGroupsForLB(ctx, aws.ToString(lb.LoadBalancerArn))
		if err != nil {
			return nil, nil, err
		}

		tgByPort := map[int32]elbv2types.TargetGroup{}
		for _, tg := range targetGroups {
			if tg.Port == nil {
				continue
			}
			p := aws.ToInt32(tg.Port)
			tgByPort[p] = tg
			managedTGARNs[aws.ToString(tg.TargetGroupArn)] = struct{}{}
		}

		for port := range desiredByPort {
			if _, ok := tgByPort[port]; ok {
				continue
			}
			created, err := m.createTargetGroup(ctx, mg, loadBalancer, loadBalancerPlacement.VPCID, port)
			if err != nil {
				return nil, nil, err
			}
			tgByPort[port] = created
			managedTGARNs[aws.ToString(created.TargetGroupArn)] = struct{}{}
		}

		for port := range desiredByPort {
			desiredTGARNs = append(desiredTGARNs, aws.ToString(tgByPort[port].TargetGroupArn))
		}

		listeners, err := m.describeListenersByLB(ctx, aws.ToString(lb.LoadBalancerArn))
		if err != nil {
			return nil, nil, err
		}
		listenerByPort := map[int32]elbv2types.Listener{}
		for _, l := range listeners {
			if l.Port == nil {
				continue
			}
			listenerByPort[aws.ToInt32(l.Port)] = l
		}

		for port, desired := range desiredByPort {
			tgArn := aws.ToString(tgByPort[port].TargetGroupArn)
			listenerArn, err := m.ensureListener(ctx, aws.ToString(lb.LoadBalancerArn), port, tgArn, listenerByPort[port])
			if err != nil {
				return nil, nil, err
			}
			status := cloudStatuses[desired.UID]
			status.LoadBalancers = append(status.LoadBalancers, tiproxyv1alpha1.MachinePortLoadBalancerCloudStatus{
				Name:                loadBalancer.Name,
				Scope:               loadBalancer.Scope,
				LoadBalancerID:      aws.ToString(lb.LoadBalancerArn),
				LoadBalancerAddress: aws.ToString(lb.DNSName),
				ListenerID:          listenerArn,
				BackendID:           tgArn,
			})
			cloudStatuses[desired.UID] = status
		}

		desiredPorts := make(map[int32]struct{}, len(desiredByPort))
		for port := range desiredByPort {
			desiredPorts[port] = struct{}{}
		}

		for _, l := range listeners {
			if l.Port == nil || l.ListenerArn == nil {
				continue
			}
			port := aws.ToInt32(l.Port)
			if _, wanted := desiredPorts[port]; wanted {
				continue
			}
			if _, err := m.elbv2.DeleteListener(ctx, &elbv2.DeleteListenerInput{
				ListenerArn: l.ListenerArn,
			}); err != nil && !isNotFound(err) {
				return nil, nil, fmt.Errorf("delete listener %s: %w", aws.ToString(l.ListenerArn), err)
			}
		}

		for _, tg := range targetGroups {
			if tg.Port == nil || tg.TargetGroupArn == nil {
				continue
			}
			port := aws.ToInt32(tg.Port)
			if _, wanted := desiredPorts[port]; wanted {
				continue
			}
			if _, err := m.elbv2.DeleteTargetGroup(ctx, &elbv2.DeleteTargetGroupInput{
				TargetGroupArn: tg.TargetGroupArn,
			}); err != nil && !isNotFound(err) {
				return nil, nil, fmt.Errorf("delete target group %s: %w", aws.ToString(tg.TargetGroupArn), err)
			}
		}
	}

	dedupDesiredTGARNs := dedupeStrings(desiredTGARNs)
	slices.Sort(dedupDesiredTGARNs)
	if err := m.reconcileASGTargetGroups(ctx, asgName, dedupDesiredTGARNs, managedTGARNs); err != nil {
		return nil, nil, err
	}
	slices.SortFunc(loadBalancerStatuses, func(a, b tiproxyv1alpha1.LoadBalancerCloudStatus) int {
		return strings.Compare(a.Name, b.Name)
	})
	for uid, status := range cloudStatuses {
		slices.SortFunc(status.LoadBalancers, func(a, b tiproxyv1alpha1.MachinePortLoadBalancerCloudStatus) int {
			return strings.Compare(a.Name, b.Name)
		})
		status.SetPrimaryLoadBalancerCompat()
		cloudStatuses[uid] = status
	}
	return loadBalancerStatuses, cloudStatuses, nil
}

func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (m *Manager) ensureNetworkLoadBalancer(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup, loadBalancer tiproxyv1alpha1.LoadBalancerExposureSpec, placement placementSelection) (elbv2types.LoadBalancer, error) {
	name := m.loadBalancerName(mg, loadBalancer)
	existing, err := m.describeLoadBalancerByName(ctx, name)
	if err != nil {
		return elbv2types.LoadBalancer{}, err
	}
	scheme := elbv2types.LoadBalancerSchemeEnumInternal
	if strings.EqualFold(loadBalancer.EffectiveScope(), "external") {
		scheme = elbv2types.LoadBalancerSchemeEnumInternetFacing
	}

	if existing != nil {
		if existing.Type != elbv2types.LoadBalancerTypeEnumNetwork {
			return elbv2types.LoadBalancer{}, fmt.Errorf("load balancer %s exists but is not network type", name)
		}
		if existing.Scheme != scheme {
			return elbv2types.LoadBalancer{}, fmt.Errorf("load balancer %s scheme mismatch: have %s want %s", name, existing.Scheme, scheme)
		}
		return *existing, nil
	}

	out, err := m.elbv2.CreateLoadBalancer(ctx, &elbv2.CreateLoadBalancerInput{
		Name:    aws.String(name),
		Type:    elbv2types.LoadBalancerTypeEnumNetwork,
		Scheme:  scheme,
		Subnets: slices.Clone(placement.SubnetIDs),
		Tags:    m.buildELBTags(mg, loadBalancer),
	})
	if err != nil {
		return elbv2types.LoadBalancer{}, fmt.Errorf("create network load balancer %s: %w", name, err)
	}
	if len(out.LoadBalancers) == 0 {
		return elbv2types.LoadBalancer{}, fmt.Errorf("create network load balancer %s: empty response", name)
	}
	return out.LoadBalancers[0], nil
}

func (m *Manager) ensureLoadBalancersDeleted(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup) error {
	loadBalancers, err := mg.Spec.Exposure.NormalizeLoadBalancers()
	if err != nil {
		return err
	}
	for _, loadBalancer := range loadBalancers {
		if err := m.ensureLoadBalancerDeletedByName(ctx, mg, loadBalancer); err != nil {
			return err
		}
	}
	for _, loadBalancer := range mg.Status.Cloud.LoadBalancers {
		if strings.TrimSpace(loadBalancer.LoadBalancerID) == "" {
			continue
		}
		if err := m.ensureLoadBalancerDeletedByARN(ctx, mg, loadBalancer.LoadBalancerID); err != nil {
			return err
		}
	}
	if len(mg.Status.Cloud.LoadBalancers) == 0 && strings.TrimSpace(mg.Status.Cloud.FrontendID) != "" {
		if err := m.ensureLoadBalancerDeletedByARN(ctx, mg, mg.Status.Cloud.FrontendID); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) ensureStaleLoadBalancersDeleted(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup, desired []tiproxyv1alpha1.LoadBalancerExposureSpec) error {
	desiredByName := make(map[string]struct{}, len(desired))
	for i := range desired {
		desiredByName[desired[i].Name] = struct{}{}
	}
	for _, loadBalancer := range mg.Status.Cloud.LoadBalancers {
		if _, ok := desiredByName[loadBalancer.Name]; ok {
			continue
		}
		if strings.TrimSpace(loadBalancer.LoadBalancerID) == "" {
			continue
		}
		if err := m.ensureLoadBalancerDeletedByARN(ctx, mg, loadBalancer.LoadBalancerID); err != nil {
			return err
		}
	}
	if len(mg.Status.Cloud.LoadBalancers) == 0 && strings.TrimSpace(mg.Status.Cloud.FrontendID) != "" {
		desiredPrimaryID := ""
		if len(desired) > 0 {
			desiredPrimaryID = m.loadBalancerName(mg, desired[0])
		}
		if !strings.Contains(mg.Status.Cloud.FrontendID, "/"+desiredPrimaryID+"/") {
			if err := m.ensureLoadBalancerDeletedByARN(ctx, mg, mg.Status.Cloud.FrontendID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) ensureLoadBalancerDeletedByName(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup, loadBalancer tiproxyv1alpha1.LoadBalancerExposureSpec) error {
	lb, err := m.describeLoadBalancerByName(ctx, m.loadBalancerName(mg, loadBalancer))
	if err != nil {
		return err
	}
	if lb == nil || lb.LoadBalancerArn == nil {
		return nil
	}
	return m.ensureLoadBalancerDeletedByARN(ctx, mg, aws.ToString(lb.LoadBalancerArn))
}

func (m *Manager) ensureLoadBalancerDeletedByARN(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup, lbArn string) error {
	if strings.TrimSpace(lbArn) == "" {
		return nil
	}
	listeners, err := m.describeListenersByLB(ctx, lbArn)
	if err != nil {
		return err
	}
	for _, listener := range listeners {
		if listener.ListenerArn == nil {
			continue
		}
		if _, err := m.elbv2.DeleteListener(ctx, &elbv2.DeleteListenerInput{
			ListenerArn: listener.ListenerArn,
		}); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete listener %s: %w", aws.ToString(listener.ListenerArn), err)
		}
	}

	targetGroups, err := m.describeTargetGroupsForLB(ctx, lbArn)
	if err != nil {
		return err
	}
	tgARNs := make([]string, 0, len(targetGroups))
	for _, tg := range targetGroups {
		if tg.TargetGroupArn != nil {
			tgARNs = append(tgARNs, aws.ToString(tg.TargetGroupArn))
		}
	}
	if len(tgARNs) > 0 {
		_, asgName := cloud.ResolveNames(mg)
		_, err := m.asg.DetachLoadBalancerTargetGroups(ctx, &autoscaling.DetachLoadBalancerTargetGroupsInput{
			AutoScalingGroupName: aws.String(asgName),
			TargetGroupARNs:      tgARNs,
		})
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("detach target groups from asg %s: %w", asgName, err)
		}
	}

	for _, tg := range targetGroups {
		if tg.TargetGroupArn == nil {
			continue
		}
		if _, err := m.elbv2.DeleteTargetGroup(ctx, &elbv2.DeleteTargetGroupInput{
			TargetGroupArn: tg.TargetGroupArn,
		}); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete target group %s: %w", aws.ToString(tg.TargetGroupArn), err)
		}
	}

	_, err = m.elbv2.DeleteLoadBalancer(ctx, &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String(lbArn),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete load balancer %s: %w", lbArn, err)
	}
	return nil
}

func (m *Manager) describeLoadBalancerByName(ctx context.Context, name string) (*elbv2types.LoadBalancer, error) {
	out, err := m.elbv2.DescribeLoadBalancers(ctx, &elbv2.DescribeLoadBalancersInput{
		Names: []string{name},
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("describe load balancer %s: %w", name, err)
	}
	if len(out.LoadBalancers) == 0 {
		return nil, nil
	}
	return &out.LoadBalancers[0], nil
}

func (m *Manager) describeListenersByLB(ctx context.Context, lbArn string) ([]elbv2types.Listener, error) {
	if lbArn == "" {
		return nil, nil
	}
	out, err := m.elbv2.DescribeListeners(ctx, &elbv2.DescribeListenersInput{
		LoadBalancerArn: aws.String(lbArn),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("describe listeners for load balancer %s: %w", lbArn, err)
	}
	return out.Listeners, nil
}

func (m *Manager) describeTargetGroupsForLB(ctx context.Context, lbArn string) ([]elbv2types.TargetGroup, error) {
	if lbArn == "" {
		return nil, nil
	}
	out, err := m.elbv2.DescribeTargetGroups(ctx, &elbv2.DescribeTargetGroupsInput{
		LoadBalancerArn: aws.String(lbArn),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("describe target groups for load balancer %s: %w", lbArn, err)
	}
	return out.TargetGroups, nil
}

func (m *Manager) createTargetGroup(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup, loadBalancer tiproxyv1alpha1.LoadBalancerExposureSpec, vpcID string, port int32) (elbv2types.TargetGroup, error) {
	out, err := m.elbv2.CreateTargetGroup(ctx, &elbv2.CreateTargetGroupInput{
		Name:                       aws.String(m.targetGroupName(mg, loadBalancer, port)),
		Port:                       aws.Int32(port),
		Protocol:                   elbv2types.ProtocolEnumTcp,
		VpcId:                      aws.String(vpcID),
		TargetType:                 elbv2types.TargetTypeEnumInstance,
		HealthCheckEnabled:         aws.Bool(true),
		HealthCheckProtocol:        elbv2types.ProtocolEnumTcp,
		HealthCheckPort:            aws.String("traffic-port"),
		HealthyThresholdCount:      aws.Int32(2),
		UnhealthyThresholdCount:    aws.Int32(2),
		HealthCheckTimeoutSeconds:  aws.Int32(6),
		HealthCheckIntervalSeconds: aws.Int32(10),
		Tags:                       m.buildELBTags(mg, loadBalancer),
	})
	if err != nil {
		return elbv2types.TargetGroup{}, fmt.Errorf("create target group for port %d: %w", port, err)
	}
	if len(out.TargetGroups) == 0 {
		return elbv2types.TargetGroup{}, fmt.Errorf("create target group for port %d: empty response", port)
	}
	return out.TargetGroups[0], nil
}

func (m *Manager) ensureListener(ctx context.Context, lbArn string, port int32, targetGroupArn string, existing elbv2types.Listener) (string, error) {
	actions := []elbv2types.Action{
		{
			Type:           elbv2types.ActionTypeEnumForward,
			TargetGroupArn: aws.String(targetGroupArn),
		},
	}
	if existing.ListenerArn == nil {
		out, err := m.elbv2.CreateListener(ctx, &elbv2.CreateListenerInput{
			LoadBalancerArn: aws.String(lbArn),
			Port:            aws.Int32(port),
			Protocol:        elbv2types.ProtocolEnumTcp,
			DefaultActions:  actions,
		})
		if err != nil {
			return "", fmt.Errorf("create listener for port %d: %w", port, err)
		}
		if len(out.Listeners) == 0 {
			return "", fmt.Errorf("create listener for port %d: empty response", port)
		}
		return aws.ToString(out.Listeners[0].ListenerArn), nil
	}

	needModify := existing.Protocol != elbv2types.ProtocolEnumTcp || !listenerForwardsTo(existing, targetGroupArn)
	if needModify {
		_, err := m.elbv2.ModifyListener(ctx, &elbv2.ModifyListenerInput{
			ListenerArn:    existing.ListenerArn,
			Port:           aws.Int32(port),
			Protocol:       elbv2types.ProtocolEnumTcp,
			DefaultActions: actions,
		})
		if err != nil {
			return "", fmt.Errorf("modify listener %s for port %d: %w", aws.ToString(existing.ListenerArn), port, err)
		}
	}
	return aws.ToString(existing.ListenerArn), nil
}

func listenerForwardsTo(listener elbv2types.Listener, tgArn string) bool {
	for _, action := range listener.DefaultActions {
		if action.Type != elbv2types.ActionTypeEnumForward {
			continue
		}
		if aws.ToString(action.TargetGroupArn) == tgArn {
			return true
		}
		if action.ForwardConfig != nil {
			for _, tuple := range action.ForwardConfig.TargetGroups {
				if aws.ToString(tuple.TargetGroupArn) == tgArn {
					return true
				}
			}
		}
	}
	return false
}

func (m *Manager) reconcileASGTargetGroups(ctx context.Context, asgName string, desired []string, managed map[string]struct{}) error {
	out, err := m.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		return fmt.Errorf("describe asg %s: %w", asgName, err)
	}
	if len(out.AutoScalingGroups) == 0 {
		return fmt.Errorf("asg %s not found while reconciling target groups", asgName)
	}
	asg := out.AutoScalingGroups[0]

	current := make(map[string]struct{}, len(asg.TargetGroupARNs))
	for _, arn := range asg.TargetGroupARNs {
		current[arn] = struct{}{}
	}
	desiredSet := make(map[string]struct{}, len(desired))
	for _, arn := range desired {
		desiredSet[arn] = struct{}{}
	}

	toAttach := make([]string, 0)
	for _, arn := range desired {
		if _, ok := current[arn]; ok {
			continue
		}
		toAttach = append(toAttach, arn)
	}
	if len(toAttach) > 0 {
		_, err := m.asg.AttachLoadBalancerTargetGroups(ctx, &autoscaling.AttachLoadBalancerTargetGroupsInput{
			AutoScalingGroupName: aws.String(asgName),
			TargetGroupARNs:      toAttach,
		})
		if err != nil {
			return fmt.Errorf("attach target groups to asg %s: %w", asgName, err)
		}
	}

	toDetach := make([]string, 0)
	for arn := range current {
		if _, ours := managed[arn]; !ours {
			continue
		}
		if _, wanted := desiredSet[arn]; wanted {
			continue
		}
		toDetach = append(toDetach, arn)
	}
	if len(toDetach) > 0 {
		_, err := m.asg.DetachLoadBalancerTargetGroups(ctx, &autoscaling.DetachLoadBalancerTargetGroupsInput{
			AutoScalingGroupName: aws.String(asgName),
			TargetGroupARNs:      toDetach,
		})
		if err != nil {
			return fmt.Errorf("detach target groups from asg %s: %w", asgName, err)
		}
	}
	return nil
}

func (m *Manager) resolvePlacement(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup) (placementSelection, error) {
	subnetIDs := mg.Spec.Network.SubnetIDs
	if len(subnetIDs) == 0 {
		return placementSelection{}, fmt.Errorf("spec.network.subnetIDs is empty")
	}
	out, err := m.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: slices.Clone(subnetIDs),
	})
	if err != nil {
		return placementSelection{}, fmt.Errorf("describe subnets: %w", err)
	}
	selection, err := selectPlacementSubnets(subnetIDs, out.Subnets, mg.Spec.Placement.FailureDomains)
	if err != nil {
		return placementSelection{}, err
	}
	distribution, err := availabilityZoneDistributionForSpreadPolicy(mg.Spec.Placement.SpreadPolicy)
	if err != nil {
		return placementSelection{}, err
	}
	selection.AvailabilityZoneDistribution = distribution
	return selection, nil
}

func (m *Manager) resolveLoadBalancerPlacement(
	ctx context.Context,
	mg *tiproxyv1alpha1.TiProxyMachineGroup,
	loadBalancer tiproxyv1alpha1.LoadBalancerExposureSpec,
	fallback placementSelection,
) (placementSelection, error) {
	subnetIDs := loadBalancer.SubnetIDs
	if len(subnetIDs) == 0 {
		return fallback, nil
	}

	out, err := m.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: slices.Clone(subnetIDs),
	})
	if err != nil {
		return placementSelection{}, fmt.Errorf("describe load balancer subnets: %w", err)
	}
	selection, err := selectPlacementSubnets(subnetIDs, out.Subnets, mg.Spec.Placement.FailureDomains)
	if err != nil {
		return placementSelection{}, err
	}
	if fallback.VPCID != "" && selection.VPCID != fallback.VPCID {
		return placementSelection{}, fmt.Errorf("loadBalancer %q subnets must belong to the same vpc %q as machine placement, got %q", loadBalancer.Name, fallback.VPCID, selection.VPCID)
	}
	selection.AvailabilityZoneDistribution = fallback.AvailabilityZoneDistribution
	return selection, nil
}

func selectPlacementSubnets(requestedSubnetIDs []string, subnets []ec2types.Subnet, failureDomains []string) (placementSelection, error) {
	if len(requestedSubnetIDs) == 0 {
		return placementSelection{}, fmt.Errorf("subnetIDs is empty")
	}
	if len(subnets) == 0 {
		return placementSelection{}, fmt.Errorf("describe subnets returned no result")
	}

	byID := make(map[string]ec2types.Subnet, len(subnets))
	for _, subnet := range subnets {
		subnetID := strings.TrimSpace(aws.ToString(subnet.SubnetId))
		if subnetID == "" {
			continue
		}
		byID[subnetID] = subnet
	}

	allowedZones := make(map[string]struct{}, len(failureDomains))
	for _, zone := range failureDomains {
		zone = strings.TrimSpace(zone)
		if zone == "" {
			continue
		}
		allowedZones[zone] = struct{}{}
	}

	selected := placementSelection{}
	selectedZones := make(map[string]struct{})
	missingZones := make(map[string]struct{}, len(allowedZones))
	for zone := range allowedZones {
		missingZones[zone] = struct{}{}
	}

	for _, subnetID := range requestedSubnetIDs {
		subnet, ok := byID[subnetID]
		if !ok {
			return placementSelection{}, fmt.Errorf("subnet %s not found", subnetID)
		}
		vpcID := strings.TrimSpace(aws.ToString(subnet.VpcId))
		if vpcID == "" {
			return placementSelection{}, fmt.Errorf("subnet %s has empty vpc id", subnetID)
		}
		if selected.VPCID == "" {
			selected.VPCID = vpcID
		} else if selected.VPCID != vpcID {
			return placementSelection{}, fmt.Errorf("subnets belong to different vpcs: %s and %s", selected.VPCID, vpcID)
		}

		zone := strings.TrimSpace(aws.ToString(subnet.AvailabilityZone))
		if len(allowedZones) > 0 {
			if _, ok := allowedZones[zone]; !ok {
				continue
			}
			delete(missingZones, zone)
		}

		selected.SubnetIDs = append(selected.SubnetIDs, subnetID)
		if zone != "" {
			if _, seen := selectedZones[zone]; !seen {
				selectedZones[zone] = struct{}{}
				selected.AvailabilityZones = append(selected.AvailabilityZones, zone)
			}
		}
	}

	if len(selected.SubnetIDs) == 0 {
		if len(allowedZones) > 0 {
			return placementSelection{}, fmt.Errorf("none of spec.network.subnetIDs are in requested failure domains %v", sortedKeys(allowedZones))
		}
		return placementSelection{}, fmt.Errorf("no usable subnets selected from spec.network.subnetIDs")
	}
	if len(missingZones) > 0 {
		return placementSelection{}, fmt.Errorf("requested failure domains %v do not have matching subnets in spec.network.subnetIDs", sortedKeys(missingZones))
	}
	return selected, nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

func availabilityZoneDistributionForSpreadPolicy(policy string) (*autoscalingtypes.AvailabilityZoneDistribution, error) {
	switch normalizeSpreadPolicy(policy) {
	case "":
		return nil, nil
	case "balanced", "balanced-best-effort":
		return &autoscalingtypes.AvailabilityZoneDistribution{
			CapacityDistributionStrategy: autoscalingtypes.CapacityDistributionStrategyBalancedBestEffort,
		}, nil
	case "balanced-only":
		return &autoscalingtypes.AvailabilityZoneDistribution{
			CapacityDistributionStrategy: autoscalingtypes.CapacityDistributionStrategyBalancedOnly,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported spec.placement.spreadPolicy %q", policy)
	}
}

func normalizeSpreadPolicy(policy string) string {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(policy), "_", "-")) {
	case "", "none":
		return ""
	case "balanced", "balanced-best-effort", "balancedbesteffort":
		return "balanced-best-effort"
	case "balanced-only", "balancedonly", "strict-balanced":
		return "balanced-only"
	default:
		return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(policy), "_", "-"))
	}
}

func (m *Manager) ensureLaunchTemplate(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup, ltName string) (string, string, error) {
	if mg.Spec.Machine.Image == "" {
		return "", "", fmt.Errorf("spec.machine.image is required")
	}
	if mg.Spec.Machine.Class == "" {
		return "", "", fmt.Errorf("spec.machine.class is required")
	}

	data := m.buildLaunchTemplateData(mg)
	versionDesc := aws.String(m.versionDescription(mg))

	describeOut, err := m.ec2.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{
		LaunchTemplateNames: []string{ltName},
	})
	if err != nil && !isNotFound(err) {
		return "", "", fmt.Errorf("describe launch template %s: %w", ltName, err)
	}

	if err != nil || len(describeOut.LaunchTemplates) == 0 {
		createOut, createErr := m.ec2.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
			LaunchTemplateName: aws.String(ltName),
			LaunchTemplateData: &data,
			VersionDescription: versionDesc,
		})
		if createErr != nil {
			return "", "", fmt.Errorf("create launch template %s: %w", ltName, createErr)
		}
		return aws.ToString(createOut.LaunchTemplate.LaunchTemplateId), strconv.FormatInt(aws.ToInt64(createOut.LaunchTemplate.LatestVersionNumber), 10), nil
	}

	lt := describeOut.LaunchTemplates[0]
	createVerOut, err := m.ec2.CreateLaunchTemplateVersion(ctx, &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId:   lt.LaunchTemplateId,
		LaunchTemplateData: &data,
		SourceVersion:      aws.String("$Latest"),
		VersionDescription: versionDesc,
	})
	if err != nil {
		return "", "", fmt.Errorf("create launch template version %s: %w", ltName, err)
	}

	latestVersion := strconv.FormatInt(aws.ToInt64(createVerOut.LaunchTemplateVersion.VersionNumber), 10)
	_, _ = m.ec2.ModifyLaunchTemplate(ctx, &ec2.ModifyLaunchTemplateInput{
		LaunchTemplateId: lt.LaunchTemplateId,
		DefaultVersion:   aws.String(latestVersion),
	})
	return aws.ToString(lt.LaunchTemplateId), latestVersion, nil
}

func (m *Manager) ensureAutoScalingGroup(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup, asgName, ltName, ltVersion, desiredImageHash string, placement placementSelection) error {
	if len(placement.SubnetIDs) == 0 {
		return fmt.Errorf("spec.network.subnetIDs is required")
	}
	if ltVersion == "" {
		ltVersion = "$Latest"
	}

	desired := mg.Spec.DesiredReplicas()
	minSize := desired
	maxSize := desired
	if mg.Spec.Scaling.MinReplicas != nil {
		minSize = *mg.Spec.Scaling.MinReplicas
	}
	if mg.Spec.Scaling.MaxReplicas != nil {
		maxSize = *mg.Spec.Scaling.MaxReplicas
	}
	if minSize > desired {
		desired = minSize
	}
	if maxSize < desired {
		maxSize = desired
	}

	describeOut, err := m.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		return fmt.Errorf("describe asg %s: %w", asgName, err)
	}

	if len(describeOut.AutoScalingGroups) == 0 {
		_, err = m.asg.CreateAutoScalingGroup(ctx, &autoscaling.CreateAutoScalingGroupInput{
			AutoScalingGroupName:         aws.String(asgName),
			MinSize:                      aws.Int32(minSize),
			MaxSize:                      aws.Int32(maxSize),
			DesiredCapacity:              aws.Int32(desired),
			VPCZoneIdentifier:            aws.String(strings.Join(placement.SubnetIDs, ",")),
			AvailabilityZones:            slices.Clone(placement.AvailabilityZones),
			AvailabilityZoneDistribution: placement.AvailabilityZoneDistribution,
			LaunchTemplate: &autoscalingtypes.LaunchTemplateSpecification{
				LaunchTemplateName: aws.String(ltName),
				Version:            aws.String(ltVersion),
			},
			Tags: m.buildASGTags(mg),
		})
		if err != nil {
			return fmt.Errorf("create asg %s: %w", asgName, err)
		}
		if desiredImageHash != "" {
			if err := m.setASGTag(ctx, asgName, asgTagTiProxyImageHash, desiredImageHash); err != nil {
				return err
			}
		}
		return nil
	}
	existingASG := describeOut.AutoScalingGroups[0]

	_, err = m.asg.UpdateAutoScalingGroup(ctx, &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName:         aws.String(asgName),
		MinSize:                      aws.Int32(minSize),
		MaxSize:                      aws.Int32(maxSize),
		DesiredCapacity:              aws.Int32(desired),
		VPCZoneIdentifier:            aws.String(strings.Join(placement.SubnetIDs, ",")),
		AvailabilityZones:            slices.Clone(placement.AvailabilityZones),
		AvailabilityZoneDistribution: placement.AvailabilityZoneDistribution,
		LaunchTemplate: &autoscalingtypes.LaunchTemplateSpecification{
			LaunchTemplateName: aws.String(ltName),
			Version:            aws.String(ltVersion),
		},
	})
	if err != nil {
		return fmt.Errorf("update asg %s: %w", asgName, err)
	}

	if err := m.ensureImageRollingRefresh(ctx, asgName, existingASG, desiredImageHash); err != nil {
		return err
	}
	return nil
}

func (m *Manager) ensureImageRollingRefresh(ctx context.Context, asgName string, asg autoscalingtypes.AutoScalingGroup, desiredImageHash string) error {
	if desiredImageHash == "" {
		return nil
	}

	existingImageHash := asgTagValue(asg.Tags, asgTagTiProxyImageHash)
	// Bootstrap tracking for pre-existing ASGs: set baseline without forcing an unexpected refresh.
	if existingImageHash == "" {
		return m.setASGTag(ctx, asgName, asgTagTiProxyImageHash, desiredImageHash)
	}
	if existingImageHash == desiredImageHash {
		return nil
	}
	if len(asg.Instances) == 0 {
		return m.setASGTag(ctx, asgName, asgTagTiProxyImageHash, desiredImageHash)
	}

	inProgress, err := m.hasActiveInstanceRefresh(ctx, asgName)
	if err != nil {
		return err
	}
	if inProgress {
		return nil
	}

	_, err = m.asg.StartInstanceRefresh(ctx, &autoscaling.StartInstanceRefreshInput{
		AutoScalingGroupName: aws.String(asgName),
		Strategy:             autoscalingtypes.RefreshStrategyRolling,
	})
	if err != nil {
		if isInstanceRefreshInProgress(err) {
			return nil
		}
		return fmt.Errorf("start instance refresh for asg %s: %w", asgName, err)
	}

	return m.setASGTag(ctx, asgName, asgTagTiProxyImageHash, desiredImageHash)
}

func (m *Manager) hasActiveInstanceRefresh(ctx context.Context, asgName string) (bool, error) {
	out, err := m.asg.DescribeInstanceRefreshes(ctx, &autoscaling.DescribeInstanceRefreshesInput{
		AutoScalingGroupName: aws.String(asgName),
		MaxRecords:           aws.Int32(5),
	})
	if err != nil {
		return false, fmt.Errorf("describe instance refreshes for asg %s: %w", asgName, err)
	}
	for _, refresh := range out.InstanceRefreshes {
		if refresh.Status == autoscalingtypes.InstanceRefreshStatusPending ||
			refresh.Status == autoscalingtypes.InstanceRefreshStatusInProgress ||
			refresh.Status == autoscalingtypes.InstanceRefreshStatusCancelling {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) setASGTag(ctx context.Context, asgName, key, value string) error {
	_, err := m.asg.CreateOrUpdateTags(ctx, &autoscaling.CreateOrUpdateTagsInput{
		Tags: []autoscalingtypes.Tag{
			{
				ResourceId:        aws.String(asgName),
				ResourceType:      aws.String(asgResourceTypeAutoScalingGroup),
				Key:               aws.String(key),
				Value:             aws.String(value),
				PropagateAtLaunch: aws.Bool(false),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("set asg tag %s=%s for %s: %w", key, value, asgName, err)
	}
	return nil
}

func asgTagValue(tags []autoscalingtypes.TagDescription, key string) string {
	for _, tag := range tags {
		if aws.ToString(tag.Key) == key {
			return aws.ToString(tag.Value)
		}
	}
	return ""
}

func (m *Manager) buildLaunchTemplateData(mg *tiproxyv1alpha1.TiProxyMachineGroup) ec2types.RequestLaunchTemplateData {
	machine := mg.Spec.Machine
	network := mg.Spec.Network

	data := ec2types.RequestLaunchTemplateData{
		ImageId:          aws.String(machine.Image),
		InstanceType:     ec2types.InstanceType(machine.Class),
		SecurityGroupIds: slices.Clone(network.SecurityGroupIDs),
		UserData:         aws.String(base64.StdEncoding.EncodeToString([]byte(m.userData(mg)))),
		MetadataOptions: &ec2types.LaunchTemplateInstanceMetadataOptionsRequest{
			HttpTokens: ec2types.LaunchTemplateHttpTokensStateRequired,
		},
		TagSpecifications: []ec2types.LaunchTemplateTagSpecificationRequest{
			{
				ResourceType: ec2types.ResourceTypeInstance,
				Tags:         m.buildEC2Tags(mg),
			},
			{
				ResourceType: ec2types.ResourceTypeVolume,
				Tags:         m.buildEC2Tags(mg),
			},
		},
	}

	if machine.IdentityRef != "" {
		profile := &ec2types.LaunchTemplateIamInstanceProfileSpecificationRequest{}
		if strings.HasPrefix(machine.IdentityRef, "arn:") {
			profile.Arn = aws.String(machine.IdentityRef)
		} else {
			profile.Name = aws.String(machine.IdentityRef)
		}
		data.IamInstanceProfile = profile
	}
	if machine.SSHKeyName != "" {
		data.KeyName = aws.String(machine.SSHKeyName)
	}

	return data
}

func (m *Manager) buildEC2Tags(mg *tiproxyv1alpha1.TiProxyMachineGroup) []ec2types.Tag {
	tags := map[string]string{
		"Name":                          fmt.Sprintf("%s-%s", mg.Namespace, mg.Name),
		"tiproxy.pingcap.com/group":     mg.Name,
		"tiproxy.pingcap.com/namespace": mg.Namespace,
		tagManagedBy:                    managedByValue,
	}
	for k, v := range mg.Spec.Tags {
		tags[k] = v
	}

	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	out := make([]ec2types.Tag, 0, len(keys))
	for _, key := range keys {
		val := tags[key]
		out = append(out, ec2types.Tag{
			Key:   aws.String(key),
			Value: aws.String(val),
		})
	}
	return out
}

func (m *Manager) buildASGTags(mg *tiproxyv1alpha1.TiProxyMachineGroup) []autoscalingtypes.Tag {
	tags := map[string]string{
		"Name":                          fmt.Sprintf("%s-%s", mg.Namespace, mg.Name),
		"tiproxy.pingcap.com/group":     mg.Name,
		"tiproxy.pingcap.com/namespace": mg.Namespace,
		tagManagedBy:                    managedByValue,
	}
	for k, v := range mg.Spec.Tags {
		tags[k] = v
	}

	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	out := make([]autoscalingtypes.Tag, 0, len(keys))
	for _, key := range keys {
		val := tags[key]
		out = append(out, autoscalingtypes.Tag{
			Key:               aws.String(key),
			Value:             aws.String(val),
			PropagateAtLaunch: aws.Bool(true),
		})
	}
	return out
}

func (m *Manager) buildELBTags(mg *tiproxyv1alpha1.TiProxyMachineGroup, loadBalancer tiproxyv1alpha1.LoadBalancerExposureSpec) []elbv2types.Tag {
	tags := map[string]string{
		"Name":                              fmt.Sprintf("%s-%s-%s", mg.Namespace, mg.Name, loadBalancer.Name),
		"tiproxy.pingcap.com/group":         mg.Name,
		"tiproxy.pingcap.com/load-balancer": loadBalancer.Name,
		"tiproxy.pingcap.com/namespace":     mg.Namespace,
		tagManagedBy:                        managedByValue,
	}
	for k, v := range mg.Spec.Tags {
		tags[k] = v
	}

	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	out := make([]elbv2types.Tag, 0, len(keys))
	for _, key := range keys {
		out = append(out, elbv2types.Tag{
			Key:   aws.String(key),
			Value: aws.String(tags[key]),
		})
	}
	return out
}

func (m *Manager) loadBalancerName(mg *tiproxyv1alpha1.TiProxyMachineGroup, loadBalancer tiproxyv1alpha1.LoadBalancerExposureSpec) string {
	name := strings.TrimSpace(loadBalancer.LoadBalancerName)
	if name == "" {
		name = fmt.Sprintf("tpm-%s-%s-%s", mg.Namespace, mg.Name, loadBalancer.Name)
	}
	return awsResourceName(name, 32)
}

func (m *Manager) targetGroupName(mg *tiproxyv1alpha1.TiProxyMachineGroup, loadBalancer tiproxyv1alpha1.LoadBalancerExposureSpec, port int32) string {
	return awsResourceName(fmt.Sprintf("tpm-%s-%s-%s-p%d", mg.Namespace, mg.Name, loadBalancer.Name, port), 32)
}

func awsResourceName(raw string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "tpm"
	}
	first := name[0]
	if first < 'a' || first > 'z' {
		name = "tpm-" + name
	}
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	if len(name) <= maxLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	prefixLen := maxLen - len(suffix)
	if prefixLen < 3 {
		return name[:maxLen]
	}
	prefix := strings.Trim(name[:prefixLen], "-")
	if prefix == "" {
		prefix = "tpm"
	}
	return prefix + suffix
}

func (m *Manager) versionDescription(mg *tiproxyv1alpha1.TiProxyMachineGroup) string {
	serialized, _ := json.Marshal(mg.Spec)
	sum := sha256.Sum256(serialized)
	return "tiproxy-machine-operator-" + hex.EncodeToString(sum[:8])
}

func tiproxyImageSpecHash(spec tiproxyv1alpha1.TiProxyRuntimeSpec) string {
	image := strings.TrimSpace(spec.Image)
	baseImage := strings.TrimSpace(spec.BaseImage)
	version := strings.TrimSpace(spec.Version)
	if image == "" && baseImage == "" && version == "" {
		return ""
	}
	serialized := image + "\n" + baseImage + "\n" + version
	sum := sha256.Sum256([]byte(serialized))
	return hex.EncodeToString(sum[:8])
}

func (m *Manager) userData(mg *tiproxyv1alpha1.TiProxyMachineGroup) string {
	if mg.Spec.Bootstrap.UserData != "" {
		return mg.Spec.Bootstrap.UserData
	}

	kubeconfig := mg.Spec.Bootstrap.Kubeconfig
	kubeconfigPath := strings.TrimSpace(kubeconfig.Path)
	if kubeconfigPath == "" {
		kubeconfigPath = "/etc/tiproxy-machine-agent/kubeconfig"
	}
	region := strings.TrimSpace(mg.Spec.Placement.Region)
	inlineKubeconfig := strings.TrimSpace(kubeconfig.Inline)

	userData := fmt.Sprintf(`#!/bin/bash
set -euxo pipefail
mkdir -p /etc/tiproxy-machine-agent
cat >/etc/tiproxy-machine-agent/env <<'EOF'
TIPROXY_MACHINE_NAMESPACE=%s
TIPROXY_MACHINE_GROUP=%s
TIPROXY_KUBECONFIG=%s
EOF
`, mg.Namespace, mg.Name, kubeconfigPath)

	if inlineKubeconfig != "" {
		encoded := base64.StdEncoding.EncodeToString([]byte(inlineKubeconfig))
		userData += fmt.Sprintf(`
KCFG_PATH=%q
mkdir -p "$(dirname "${KCFG_PATH}")"
echo %q | base64 -d >"${KCFG_PATH}"
chmod 0600 "${KCFG_PATH}"
`, kubeconfigPath, encoded)
	}

	if kubeconfig.RemoteRef != nil && strings.TrimSpace(kubeconfig.RemoteRef.Identifier) != "" {
		backend := strings.ToLower(strings.TrimSpace(kubeconfig.RemoteRef.Backend))
		if backend == "" {
			backend = "parameterstore"
		}
		switch backend {
		case "parameterstore", "parameter-store", "ssm", "ssm-parameter":
			userData += fmt.Sprintf(`
if ! command -v aws >/dev/null 2>&1; then
  echo "aws cli is required to fetch kubeconfig from parameter store" >&2
  exit 1
fi

KCFG_PATH=%q
REMOTE_IDENTIFIER=%q
AWS_REGION_OVERRIDE=%q
AWS_REGION="${AWS_REGION_OVERRIDE:-${AWS_REGION:-}}"
if [ -z "${AWS_REGION}" ]; then
  # Fallback only when region is not explicitly configured.
  AWS_REGION="$(curl -fsS http://169.254.169.254/latest/meta-data/placement/region || true)"
fi

mkdir -p "$(dirname "${KCFG_PATH}")"
if [ -n "${AWS_REGION}" ]; then
  aws ssm get-parameter --name "${REMOTE_IDENTIFIER}" --with-decryption --region "${AWS_REGION}" --query 'Parameter.Value' --output text >"${KCFG_PATH}"
else
  aws ssm get-parameter --name "${REMOTE_IDENTIFIER}" --with-decryption --query 'Parameter.Value' --output text >"${KCFG_PATH}"
fi
chmod 0600 "${KCFG_PATH}"
`, kubeconfigPath, kubeconfig.RemoteRef.Identifier, region)
		default:
			userData += fmt.Sprintf(`
echo "unsupported kubeconfig remote backend: %s" >&2
exit 1
`, backend)
		}
	}

	userData += `
systemctl daemon-reload || true
systemctl enable --now tiproxy-machine-agent || true
`
	return userData
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") {
		return true
	}

	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := strings.ToLower(apiErr.ErrorCode())
	if strings.Contains(code, "notfound") {
		return true
	}
	if code == "validationerror" {
		msg := strings.ToLower(apiErr.ErrorMessage())
		return strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist")
	}
	return false
}

func isInstanceRefreshInProgress(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return strings.EqualFold(apiErr.ErrorCode(), "InstanceRefreshInProgress")
}
