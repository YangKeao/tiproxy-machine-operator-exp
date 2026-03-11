package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TiProxyMachine represents one agent+TiProxy process on one machine.
type TiProxyMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TiProxyMachineSpec   `json:"spec,omitempty"`
	Status TiProxyMachineStatus `json:"status,omitempty"`
}

type TiProxyMachineSpec struct {
	MachineGroupRef    ObjectReference `json:"machineGroupRef"`
	ProviderInstanceID string          `json:"providerInstanceID,omitempty"`
	NodeName           string          `json:"nodeName,omitempty"`
	Address            string          `json:"address,omitempty"`
}

type TiProxyMachineStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	LastHeartbeatTime  metav1.Time        `json:"lastHeartbeatTime,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	ConfigHash         string             `json:"configHash,omitempty"`
	TiProxyImage       string             `json:"tiProxyImage,omitempty"`
	Message            string             `json:"message,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// TiProxyMachineList contains a list of TiProxyMachine.
type TiProxyMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TiProxyMachine `json:"items"`
}
