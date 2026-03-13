package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=tidbresourcepoollinks,scope=Namespaced,shortName=tdbrpl
// TiDBResourcePoolLink links one TiDB resource pool to one TiProxyMachineGroup.
type TiDBResourcePoolLink struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TiDBResourcePoolLinkSpec   `json:"spec,omitempty"`
	Status TiDBResourcePoolLinkStatus `json:"status,omitempty"`
}

type TiDBResourcePoolLinkSpec struct {
	MachineGroupRef ObjectReference `json:"machineGroupRef"`
	// ClusterName is the resource pool name in TiProxy.
	ClusterName    string                       `json:"clusterName,omitempty"`
	RouteNamespace string                       `json:"routeNamespace,omitempty"`
	FrontendUser   string                       `json:"frontendUser,omitempty"`
	PDAddresses    []string                     `json:"pdAddresses,omitempty"`
	NSServerAddr   string                       `json:"nsServerAddress,omitempty"`
	TLSSecretRef   *corev1.LocalObjectReference `json:"tlsSecretRef,omitempty"`
	Disabled       bool                         `json:"disabled,omitempty"`
}

type TiDBResourcePoolLinkStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// TiDBResourcePoolLinkList contains a list of TiDBResourcePoolLink.
type TiDBResourcePoolLinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TiDBResourcePoolLink `json:"items"`
}
