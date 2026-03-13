package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=tiproxymachinegroups,scope=Namespaced,shortName=tpmg
// TiProxyMachineGroup defines a group of TiProxy machines managed by this operator.
type TiProxyMachineGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TiProxyMachineGroupSpec   `json:"spec,omitempty"`
	Status TiProxyMachineGroupStatus `json:"status,omitempty"`
}

type TiProxyMachineGroupSpec struct {
	// Replicas controls desired machine count in the backing machine group.
	Replicas *int32 `json:"replicas,omitempty"`
	// Scaling controls allowed capacity range for the backing machine group.
	Scaling ScalingSpec `json:"scaling,omitempty"`
	// Provider selects cloud implementation, e.g. "aws" or "none".
	Provider string `json:"provider,omitempty"`
	// ProviderConfigRef optionally points to provider-specific defaults or advanced settings.
	ProviderConfigRef *ObjectReference `json:"providerConfigRef,omitempty"`
	// PortRange controls frontend port allocation range for TiProxyMachinePort resources.
	PortRange PortRange `json:"portRange"`
	// LinkSelector optionally limits which links are considered.
	LinkSelector *metav1.LabelSelector `json:"linkSelector,omitempty"`
	// Exposure controls shared load balancer behavior.
	Exposure ExposureSpec `json:"exposure,omitempty"`
	// Placement expresses the desired region and failure-domain spread intent.
	Placement PlacementSpec `json:"placement,omitempty"`
	// ResourceNames optionally overrides provider resource names generated for this machine group.
	ResourceNames ResourceNamesSpec `json:"resourceNames,omitempty"`
	// Machine selects the machine image and size class.
	Machine MachineSpec `json:"machine,omitempty"`
	// Network selects provider network attachments required to place the machines.
	Network NetworkSpec `json:"network,omitempty"`
	// Tags are copied to provider resources where supported.
	Tags map[string]string `json:"tags,omitempty"`
	// Bootstrap controls how machine-side agent startup context is prepared.
	Bootstrap MachineBootstrapSpec `json:"bootstrap,omitempty"`
	// TiProxy controls runtime configuration for the machine-side agent.
	TiProxy TiProxyRuntimeSpec `json:"tiproxy,omitempty"`
}

func (s TiProxyMachineGroupSpec) DesiredReplicas() int32 {
	if s.Replicas == nil {
		return 1
	}
	if *s.Replicas < 0 {
		return 0
	}
	return *s.Replicas
}

type ResolvedLinkStatus struct {
	UID            string                       `json:"uid"`
	Name           string                       `json:"name"`
	Namespace      string                       `json:"namespace"`
	RouteNamespace string                       `json:"routeNamespace,omitempty"`
	FrontendUser   string                       `json:"frontendUser,omitempty"`
	Port           int32                        `json:"port"`
	ClusterName    string                       `json:"clusterName,omitempty"`
	PDAddresses    []string                     `json:"pdAddresses,omitempty"`
	NSServerAddr   string                       `json:"nsServerAddress,omitempty"`
	TLSSecretRef   *corev1.LocalObjectReference `json:"tlsSecretRef,omitempty"`
	ConfigHash     string                       `json:"configHash,omitempty"`
}

type TiProxyMachineGroupStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// AllocatedPorts stores allocated frontend ports keyed by TiProxyMachinePort UID.
	AllocatedPorts map[string]int32         `json:"allocatedPorts,omitempty"`
	ResolvedLinks  []ResolvedLinkStatus     `json:"resolvedLinks,omitempty"`
	Machines       map[string]MachineStatus `json:"machines,omitempty"`
	Cloud          CloudStatus              `json:"cloud,omitempty"`
	Conditions     []metav1.Condition       `json:"conditions,omitempty"`
}

// MachineStatus captures runtime state reported by one machine-side agent.
// The map key in TiProxyMachineGroup.status.machines is the machine identifier.
type MachineStatus struct {
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	LastHeartbeatTime  metav1.Time `json:"lastHeartbeatTime,omitempty"`
	Phase              string      `json:"phase,omitempty"`
	ConfigHash         string      `json:"configHash,omitempty"`
	TiProxyImage       string      `json:"tiProxyImage,omitempty"`
	Message            string      `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// TiProxyMachineGroupList contains a list of TiProxyMachineGroup.
type TiProxyMachineGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TiProxyMachineGroup `json:"items"`
}
