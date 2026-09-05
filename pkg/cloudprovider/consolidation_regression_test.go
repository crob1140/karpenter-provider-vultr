package cloudprovider

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloud "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// Regression coverage for the pricing/availability contract consumed by Karpenter's
// consolidation logic: an unavailable offering must not be considered a viable or
// cheaper replacement, even when its advertised price is lower.
func TestConsolidationReplacementCandidatesExcludeUnavailableVultrOfferings(t *testing.T) {
	cheapUnavailable := testInstanceType("cheap", 0.01, false)
	expensiveAvailable := testInstanceType("expensive", 0.02, true)
	instanceTypes := karpcloud.InstanceTypes{cheapUnavailable, expensiveAvailable}

	requirements := scheduling.NewRequirements(
		scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "syd"),
	)

	compatible := instanceTypes.Compatible(requirements)
	if len(compatible) != 1 || compatible[0].Name != "expensive" {
		t.Fatalf("expected only the available replacement candidate, got %v", instanceTypeNames(compatible))
	}

	ordered := karpcloud.InstanceTypes{cheapUnavailable, expensiveAvailable}
	ordered.OrderByPrice(requirements)
	if ordered[0].Name != "expensive" {
		t.Fatalf("expected consolidation pricing order to ignore unavailable cheap offering, got %s first", ordered[0].Name)
	}
}
