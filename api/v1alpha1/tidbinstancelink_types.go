package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TiDBInstanceLink links one TiDB cluster to one TiProxyMachineGroup.
type TiDBInstanceLink struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TiDBInstanceLinkSpec   `json:"spec,omitempty"`
	Status TiDBInstanceLinkStatus `json:"status,omitempty"`
}

type TiDBInstanceLinkSpec struct {
	MachineGroupRef ObjectReference `json:"machineGroupRef"`
	// This means the name of the resource pool.
	ClusterName string `json:"clusterName,omitempty"`
	// Port pins this link to a specific frontend port.
	// If unset, the controller allocates one from TiProxyMachineGroup.spec.portRange.
	Port           *int32                       `json:"port,omitempty"`
	RouteNamespace string                       `json:"routeNamespace,omitempty"`
	FrontendUser   string                       `json:"frontendUser,omitempty"`
	PDAddresses    []string                     `json:"pdAddresses,omitempty"`
	NSServerAddr   string                       `json:"nsServerAddress,omitempty"`
	TLSSecretRef   *corev1.LocalObjectReference `json:"tlsSecretRef,omitempty"`
	Disabled       bool                         `json:"disabled,omitempty"`
}

type TiDBInstanceLinkStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	AssignedPort       *int32             `json:"assignedPort,omitempty"`
	Phase              string             `json:"phase,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// TiDBInstanceLinkList contains a list of TiDBInstanceLink.
type TiDBInstanceLinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TiDBInstanceLink `json:"items"`
}
