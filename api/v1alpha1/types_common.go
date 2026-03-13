package v1alpha1

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

const (
	ConditionReady        = "Ready"
	ConditionPortAssigned = "PortAssigned"
	ConditionExposed      = "Exposed"
)

const (
	MachinePhasePending  = "Pending"
	MachinePhaseStarting = "Starting"
	MachinePhaseRunning  = "Running"
	MachinePhaseFailed   = "Failed"
)

// Deprecated: use MachinePhase* constants.
const (
	TiProxyMachinePhasePending  = MachinePhasePending
	TiProxyMachinePhaseStarting = MachinePhaseStarting
	TiProxyMachinePhaseRunning  = MachinePhaseRunning
	TiProxyMachinePhaseFailed   = MachinePhaseFailed
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

type ScalingSpec struct {
	MinReplicas *int32 `json:"minReplicas,omitempty"`
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`
}

type PlacementSpec struct {
	// Region selects the provider region that hosts this machine group.
	Region string `json:"region,omitempty"`
	// FailureDomains expresses preferred fault domains, such as AZs.
	FailureDomains []string `json:"failureDomains,omitempty"`
	// SpreadPolicy expresses desired spread behavior across failure domains.
	// Supported values are implementation-defined. "Balanced" is recommended.
	SpreadPolicy string `json:"spreadPolicy,omitempty"`
}

type MachineSpec struct {
	Image string `json:"image,omitempty"`
	Class string `json:"class,omitempty"`
	// IdentityRef points to a provider-specific machine identity attachment.
	// On AWS this is currently interpreted as an IAM instance profile name or ARN.
	IdentityRef string `json:"identityRef,omitempty"`
	// SSHKeyName requests provider-managed SSH key injection when supported.
	SSHKeyName string `json:"sshKeyName,omitempty"`
}

type NetworkSpec struct {
	SubnetIDs []string `json:"subnetIDs,omitempty"`
	// SecurityGroupIDs configures network policy attachments where the provider supports them.
	SecurityGroupIDs []string `json:"securityGroupIDs,omitempty"`
}

type ResourceNamesSpec struct {
	Group           string `json:"group,omitempty"`
	MachineTemplate string `json:"machineTemplate,omitempty"`
}

type ExposureSpec struct {
	// LoadBalancers controls shared load balancer exposure endpoints.
	// Nil means one default internal load balancer named "internal".
	// An explicit empty list disables load balancer exposure.
	LoadBalancers []LoadBalancerExposureSpec `json:"loadBalancers,omitempty"`
}

type LoadBalancerExposureSpec struct {
	// Name uniquely identifies this load balancer inside one TiProxyMachineGroup.
	Name string `json:"name,omitempty"`
	// Scope controls whether the load balancer is internal or internet-facing.
	// Supported values: "internal", "external". Default: "internal".
	Scope string `json:"scope,omitempty"`
	// LoadBalancerName optionally overrides the cloud resource name for this load balancer.
	LoadBalancerName string `json:"loadBalancerName,omitempty"`
	// SubnetIDs optionally overrides which subnets host this load balancer.
	// If unset, the machine placement subnet set is reused.
	SubnetIDs []string `json:"subnetIDs,omitempty"`
}

func (f LoadBalancerExposureSpec) EffectiveScope() string {
	scope := strings.TrimSpace(f.Scope)
	if scope == "" {
		return "internal"
	}
	return scope
}

func (s ExposureSpec) NormalizeLoadBalancers() ([]LoadBalancerExposureSpec, error) {
	if s.LoadBalancers == nil {
		return []LoadBalancerExposureSpec{{
			Name:  "internal",
			Scope: "internal",
		}}, nil
	}

	out := make([]LoadBalancerExposureSpec, len(s.LoadBalancers))
	usedExplicit := map[string]struct{}{}
	usedNames := map[string]int{}
	for i := range s.LoadBalancers {
		out[i] = s.LoadBalancers[i]
		out[i].Scope = out[i].EffectiveScope()

		explicitName := strings.TrimSpace(out[i].Name)
		if explicitName != "" {
			if _, exists := usedExplicit[explicitName]; exists {
				return nil, fmt.Errorf("duplicate exposure.loadBalancers name %q", explicitName)
			}
			usedExplicit[explicitName] = struct{}{}
			usedNames[explicitName]++
			continue
		}

		base := strings.TrimSpace(out[i].Scope)
		if base == "" {
			base = "load-balancer"
		}
		if usedNames[base] == 0 {
			out[i].Name = base
			usedNames[base] = 1
			continue
		}
		usedNames[base]++
		out[i].Name = fmt.Sprintf("%s-%d", base, usedNames[base])
	}

	for i := range out {
		switch strings.ToLower(strings.TrimSpace(out[i].Scope)) {
		case "internal", "external":
		default:
			return nil, fmt.Errorf("unsupported exposure.loadBalancers[%d].scope %q", i, out[i].Scope)
		}
	}
	return out, nil
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
	// Deprecated: use LoadBalancers.
	FrontendID string `json:"frontendID,omitempty"`
	// Deprecated: use LoadBalancers.
	FrontendAddress string                    `json:"frontendAddress,omitempty"`
	LoadBalancers   []LoadBalancerCloudStatus `json:"loadBalancers,omitempty"`
}

type MachinePortCloudStatus struct {
	// Deprecated: use LoadBalancers.
	FrontendID string `json:"frontendID,omitempty"`
	// Deprecated: use LoadBalancers.
	FrontendAddress string `json:"frontendAddress,omitempty"`
	// Deprecated: use LoadBalancers.
	ListenerID string `json:"listenerID,omitempty"`
	// Deprecated: use LoadBalancers.
	BackendID     string                               `json:"backendID,omitempty"`
	LoadBalancers []MachinePortLoadBalancerCloudStatus `json:"loadBalancers,omitempty"`
}

type LoadBalancerCloudStatus struct {
	Name                string `json:"name,omitempty"`
	Scope               string `json:"scope,omitempty"`
	LoadBalancerID      string `json:"loadBalancerID,omitempty"`
	LoadBalancerAddress string `json:"loadBalancerAddress,omitempty"`
}

type MachinePortLoadBalancerCloudStatus struct {
	Name                string `json:"name,omitempty"`
	Scope               string `json:"scope,omitempty"`
	LoadBalancerID      string `json:"loadBalancerID,omitempty"`
	LoadBalancerAddress string `json:"loadBalancerAddress,omitempty"`
	ListenerID          string `json:"listenerID,omitempty"`
	BackendID           string `json:"backendID,omitempty"`
}

func (s *CloudStatus) SetPrimaryLoadBalancerCompat() {
	s.FrontendID = ""
	s.FrontendAddress = ""
	if len(s.LoadBalancers) == 0 {
		return
	}
	s.FrontendID = s.LoadBalancers[0].LoadBalancerID
	s.FrontendAddress = s.LoadBalancers[0].LoadBalancerAddress
}

func (s *MachinePortCloudStatus) SetPrimaryLoadBalancerCompat() {
	s.FrontendID = ""
	s.FrontendAddress = ""
	s.ListenerID = ""
	s.BackendID = ""
	if len(s.LoadBalancers) == 0 {
		return
	}
	s.FrontendID = s.LoadBalancers[0].LoadBalancerID
	s.FrontendAddress = s.LoadBalancers[0].LoadBalancerAddress
	s.ListenerID = s.LoadBalancers[0].ListenerID
	s.BackendID = s.LoadBalancers[0].BackendID
}
