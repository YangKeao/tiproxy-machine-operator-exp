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
	"github.com/aws/smithy-go"
)

var _ cloud.Manager = (*Manager)(nil)

const (
	asgResourceTypeAutoScalingGroup = "auto-scaling-group"
	asgTagTiProxyImageHash          = "tiproxy.pingcap.com/tiproxy-image-hash"
)

type Manager struct {
	ec2 *ec2.Client
	asg *autoscaling.Client
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
		ec2: ec2.NewFromConfig(cfg),
		asg: autoscaling.NewFromConfig(cfg),
	}, nil
}

func (m *Manager) EnsureMachineGroup(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup) (tiproxyv1alpha1.CloudStatus, error) {
	ltName, asgName := cloud.ResolveNames(mg)
	desiredImageHash := tiproxyImageSpecHash(mg.Spec.TiProxy)

	ltID, ltVersion, err := m.ensureLaunchTemplate(ctx, mg, ltName)
	if err != nil {
		return tiproxyv1alpha1.CloudStatus{}, err
	}
	if err := m.ensureAutoScalingGroup(ctx, mg, asgName, ltName, ltVersion, desiredImageHash); err != nil {
		return tiproxyv1alpha1.CloudStatus{}, err
	}

	return tiproxyv1alpha1.CloudStatus{
		MachineGroupName:    asgName,
		MachineTemplateID:   ltID,
		MachineTemplateName: ltName,
		MachineTemplateVer:  ltVersion,
	}, nil
}

func (m *Manager) DeleteMachineGroup(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup) error {
	ltName, asgName := cloud.ResolveNames(mg)

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

func (m *Manager) ensureLaunchTemplate(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup, ltName string) (string, string, error) {
	if mg.Spec.Infrastructure.MachineImage == "" {
		return "", "", fmt.Errorf("spec.infrastructure.machineImage is required")
	}
	if mg.Spec.Infrastructure.MachineType == "" {
		return "", "", fmt.Errorf("spec.infrastructure.machineType is required")
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

func (m *Manager) ensureAutoScalingGroup(ctx context.Context, mg *tiproxyv1alpha1.TiProxyMachineGroup, asgName, ltName, ltVersion, desiredImageHash string) error {
	if len(mg.Spec.Infrastructure.SubnetIDs) == 0 {
		return fmt.Errorf("spec.infrastructure.subnetIDs is required")
	}
	if ltVersion == "" {
		ltVersion = "$Latest"
	}

	desired := mg.Spec.DesiredReplicas()
	minSize := desired
	maxSize := desired
	if mg.Spec.Infrastructure.MinReplicas != nil {
		minSize = *mg.Spec.Infrastructure.MinReplicas
	}
	if mg.Spec.Infrastructure.MaxReplicas != nil {
		maxSize = *mg.Spec.Infrastructure.MaxReplicas
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
			AutoScalingGroupName: aws.String(asgName),
			MinSize:              aws.Int32(minSize),
			MaxSize:              aws.Int32(maxSize),
			DesiredCapacity:      aws.Int32(desired),
			VPCZoneIdentifier:    aws.String(strings.Join(mg.Spec.Infrastructure.SubnetIDs, ",")),
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
		AutoScalingGroupName: aws.String(asgName),
		MinSize:              aws.Int32(minSize),
		MaxSize:              aws.Int32(maxSize),
		DesiredCapacity:      aws.Int32(desired),
		VPCZoneIdentifier:    aws.String(strings.Join(mg.Spec.Infrastructure.SubnetIDs, ",")),
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
	infra := mg.Spec.Infrastructure

	data := ec2types.RequestLaunchTemplateData{
		ImageId:          aws.String(infra.MachineImage),
		InstanceType:     ec2types.InstanceType(infra.MachineType),
		SecurityGroupIds: slices.Clone(infra.SecurityGroupIDs),
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

	if infra.InstanceProfile != "" {
		profile := &ec2types.LaunchTemplateIamInstanceProfileSpecificationRequest{}
		if strings.HasPrefix(infra.InstanceProfile, "arn:") {
			profile.Arn = aws.String(infra.InstanceProfile)
		} else {
			profile.Name = aws.String(infra.InstanceProfile)
		}
		data.IamInstanceProfile = profile
	}
	if infra.SSHKeyName != "" {
		data.KeyName = aws.String(infra.SSHKeyName)
	}

	return data
}

func (m *Manager) buildEC2Tags(mg *tiproxyv1alpha1.TiProxyMachineGroup) []ec2types.Tag {
	tags := map[string]string{
		"Name":                          fmt.Sprintf("%s-%s", mg.Namespace, mg.Name),
		"tiproxy.pingcap.com/group":     mg.Name,
		"tiproxy.pingcap.com/namespace": mg.Namespace,
	}
	for k, v := range mg.Spec.Infrastructure.Tags {
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
	}
	for k, v := range mg.Spec.Infrastructure.Tags {
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
	region := strings.TrimSpace(mg.Spec.Infrastructure.Region)
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
