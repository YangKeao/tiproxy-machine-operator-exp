package v1alpha1

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

const (
	ConditionReady        = "Ready"
	ConditionPortAssigned = "PortAssigned"
)

const (
	TiProxyMachinePhasePending  = "Pending"
	TiProxyMachinePhaseStarting = "Starting"
	TiProxyMachinePhaseRunning  = "Running"
	TiProxyMachinePhaseFailed   = "Failed"
)

type ObjectReference struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

func (r ObjectReference) NamespacedName(defaultNamespace string) types.NamespacedName {
	ns := r.Namespace
	if ns == "" {
		ns = defaultNamespace
	}
	return types.NamespacedName{
		Namespace: ns,
		Name:      r.Name,
	}
}

type PortRange struct {
	Start int32 `json:"start"`
	End   int32 `json:"end"`
}

func (r PortRange) Validate() error {
	if r.Start <= 0 {
		return fmt.Errorf("port range start must be > 0")
	}
	if r.End < r.Start {
		return fmt.Errorf("port range end must be >= start")
	}
	return nil
}

type TiProxyRuntimeSpec struct {
	// Deprecated: prefer baseImage + version.
	Image     string `json:"image,omitempty"`
	BaseImage string `json:"baseImage,omitempty"`
	Version   string `json:"version,omitempty"`
	// Config keeps extra TiProxy config fields in a schemaless form.
	Config        *runtime.RawExtension `json:"config,omitempty"`
	APIPort       int32                 `json:"apiPort,omitempty"`
	APIScheme     string                `json:"apiScheme,omitempty"`
	ContainerName string                `json:"containerName,omitempty"`
	ConfigPath    string                `json:"configPath,omitempty"`
	CertDir       string                `json:"certDir,omitempty"`
	ExtraArgs     []string              `json:"extraArgs,omitempty"`
}

type MachineInfrastructureSpec struct {
	Region              string            `json:"region,omitempty"`
	ScalingGroupName    string            `json:"scalingGroupName,omitempty"`
	MachineTemplateName string            `json:"machineTemplateName,omitempty"`
	MachineImage        string            `json:"machineImage,omitempty"`
	MachineType         string            `json:"machineType,omitempty"`
	SubnetIDs           []string          `json:"subnetIDs,omitempty"`
	SecurityGroupIDs    []string          `json:"securityGroupIDs,omitempty"`
	InstanceProfile     string            `json:"instanceProfile,omitempty"`
	SSHKeyName          string            `json:"sshKeyName,omitempty"`
	MinReplicas         *int32            `json:"minReplicas,omitempty"`
	MaxReplicas         *int32            `json:"maxReplicas,omitempty"`
	Tags                map[string]string `json:"tags,omitempty"`
}

type MachineBootstrapSpec struct {
	UserData   string                  `json:"userData,omitempty"`
	Kubeconfig KubeconfigBootstrapSpec `json:"kubeconfig,omitempty"`
}

type KubeconfigBootstrapSpec struct {
	Path string `json:"path,omitempty"`
	// Inline is the full kubeconfig content written directly to Path.
	Inline string `json:"inline,omitempty"`
	// RemoteRef points to an external store entry that contains kubeconfig content.
	// For AWS provider, backend "parameterStore" is treated as SSM Parameter Store.
	RemoteRef *RemoteContentRef `json:"remoteRef,omitempty"`
}

type RemoteContentRef struct {
	Backend    string `json:"backend,omitempty"`
	Identifier string `json:"identifier,omitempty"`
}

type CloudStatus struct {
	MachineGroupName    string `json:"machineGroupName,omitempty"`
	MachineTemplateID   string `json:"machineTemplateID,omitempty"`
	MachineTemplateName string `json:"machineTemplateName,omitempty"`
	MachineTemplateVer  string `json:"machineTemplateVersion,omitempty"`
}
