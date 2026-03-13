package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	MachinePortPhasePending   = "Pending"
	MachinePortPhaseAllocated = "Allocated"
	MachinePortPhaseReady     = "Ready"
	MachinePortPhaseError     = "Error"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=tiproxymachineports,scope=Namespaced,shortName=tpmp
// TiProxyMachinePort allocates one frontend port and manages cloud listener exposure.
type TiProxyMachinePort struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TiProxyMachinePortSpec   `json:"spec,omitempty"`
	Status TiProxyMachinePortStatus `json:"status,omitempty"`
}

type TiProxyMachinePortSpec struct {
	MachineGroupRef ObjectReference `json:"machineGroupRef"`
	// RequestedPort asks allocator to use this exact port.
	// If unset, the allocator picks one from TiProxyMachineGroup.spec.portRange.
	RequestedPort *int32 `json:"requestedPort,omitempty"`
	// Protocol is the frontend listener protocol. Currently only "TCP" is supported.
	Protocol string `json:"protocol,omitempty"`
}

type TiProxyMachinePortStatus struct {
	ObservedGeneration int64                  `json:"observedGeneration,omitempty"`
	AssignedPort       *int32                 `json:"assignedPort,omitempty"`
	Phase              string                 `json:"phase,omitempty"`
	Cloud              MachinePortCloudStatus `json:"cloud,omitempty"`
	Conditions         []metav1.Condition     `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// TiProxyMachinePortList contains a list of TiProxyMachinePort.
type TiProxyMachinePortList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TiProxyMachinePort `json:"items"`
}
