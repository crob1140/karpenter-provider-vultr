package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
)

var (
	GroupVersion       = schema.GroupVersion{Group: "karpenter.vultr.com", Version: "v1alpha1"}
	SchemeGroupVersion = GroupVersion
)

func AddToScheme(scheme *runtime.Scheme) error { return SchemeBuilder.AddToScheme(scheme) }

func init() { _ = AddToScheme(k8sscheme.Scheme) }

var SchemeBuilder = runtime.NewSchemeBuilder(
	func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(GroupVersion, &VultrNodeClass{}, &VultrNodeClassList{})
		metav1.AddToGroupVersion(scheme, GroupVersion)
		return nil
	},
)
