package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TiProxyMachineGroup defines a group of TiProxy machines managed by this operator.
type TiProxyMachineGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TiProxyMachineGroupSpec   `json:"spec,omitempty"`
	Status TiProxyMachineGroupStatus `json:"status,omitempty"`
}

type TiProxyMachineGroupSpec struct {
	// Replicas controls desired machine count in the backing ASG.
	Replicas *int32 `json:"replicas,omitempty"`
	// Provider selects cloud implementation, e.g. "aws" or "none".
	Provider string `json:"provider,omitempty"`
	// PortRange controls per-link frontend port assignment.
	PortRange PortRange `json:"portRange"`
	// LinkSelector optionally limits which links are considered.
	LinkSelector *metav1.LabelSelector `json:"linkSelector,omitempty"`
	// Infrastructure contains machine group and networking fields shared by cloud providers.
	Infrastructure MachineInfrastructureSpec `json:"infrastructure,omitempty"`
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
	ObservedGeneration int64                `json:"observedGeneration,omitempty"`
	AllocatedPorts     map[string]int32     `json:"allocatedPorts,omitempty"`
	ResolvedLinks      []ResolvedLinkStatus `json:"resolvedLinks,omitempty"`
	Cloud              CloudStatus          `json:"cloud,omitempty"`
	Conditions         []metav1.Condition   `json:"conditions,omitempty"`
}

// TiProxyMachineGroupList contains a list of TiProxyMachineGroup.
type TiProxyMachineGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TiProxyMachineGroup `json:"items"`
}
