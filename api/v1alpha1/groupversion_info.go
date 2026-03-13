// Package v1alpha1 contains API Schema definitions for the tiproxy.pingcap.com v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=tiproxy.pingcap.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Group   = "tiproxy.pingcap.com"
	Version = "v1alpha1"
)

var (
	GroupVersion  = schema.GroupVersion{Group: Group, Version: Version}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&TiProxyMachineGroup{},
		&TiProxyMachineGroupList{},
		&TiDBResourcePoolLink{},
		&TiDBResourcePoolLinkList{},
		&TiProxyMachinePort{},
		&TiProxyMachinePortList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
