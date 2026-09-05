package v1alpha1

import (
	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (in *VultrNodeClass) GetConditions() []status.Condition {
	out := make([]status.Condition, len(in.Status.Conditions))
	for i := range in.Status.Conditions {
		out[i] = status.Condition(in.Status.Conditions[i])
	}
	return out
}
func (in *VultrNodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = make([]metav1.Condition, len(conditions))
	for i := range conditions {
		in.Status.Conditions[i] = metav1.Condition(conditions[i])
	}
}
func (in *VultrNodeClass) StatusConditions() status.ConditionSet {
	return status.NewReadyConditions().For(in)
}
