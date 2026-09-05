package vultr

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

func BuildInstanceTypes(ctx context.Context, client *Client, region string) ([]*cloudprovider.InstanceType, error) {
	plans, err := client.ListPlans(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]*cloudprovider.InstanceType, 0, len(plans))
	for _, plan := range plans {
		if plan.ID == "" || plan.VCPUCount <= 0 || plan.RAM <= 0 {
			continue
		}
		capacity := corev1.ResourceList{corev1.ResourceCPU: *resource.NewQuantity(int64(plan.VCPUCount), resource.DecimalSI), corev1.ResourceMemory: *resource.NewQuantity(int64(plan.RAM)*1024*1024, resource.BinarySI)}
		requirements := scheduling.NewRequirements(
			scheduling.NewRequirement(
				corev1.LabelInstanceTypeStable,
				corev1.NodeSelectorOpIn,
				plan.ID,
			),
			scheduling.NewRequirement(
				corev1.LabelArchStable,
				corev1.NodeSelectorOpIn,
				"amd64",
			),
			scheduling.NewRequirement(
				corev1.LabelOSStable,
				corev1.NodeSelectorOpIn,
				"linux",
			),
		)
		result = append(result, &cloudprovider.InstanceType{Name: plan.ID, Requirements: requirements, Capacity: capacity, Overhead: &cloudprovider.InstanceTypeOverhead{}, Offerings: cloudprovider.Offerings{{Requirements: scheduling.NewRequirements(scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand), scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region)), Price: plan.MonthlyCost / 730.0, Available: true}}})
	}
	return result, nil
}
