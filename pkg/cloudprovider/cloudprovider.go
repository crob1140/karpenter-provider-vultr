package cloudprovider

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloud "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	vultrv1 "github.com/crob1140/karpenter-provider-vultr/pkg/apis/v1alpha1"
	"github.com/crob1140/karpenter-provider-vultr/pkg/bootstrap"
	"github.com/crob1140/karpenter-provider-vultr/pkg/vultr"
)

var _ karpcloud.CloudProvider = (*CloudProvider)(nil)

type CloudProvider struct {
	kubeClient    client.Client
	vultr         *vultr.Client
	tokenProvider *bootstrap.TokenProvider
	instanceTypes *vultr.InstanceTypeProvider
}

func New(kubeClient client.Client, vultrClient *vultr.Client) *CloudProvider {
	return &CloudProvider{
		kubeClient:    kubeClient,
		vultr:         vultrClient,
		tokenProvider: bootstrap.NewTokenProvider(kubeClient),
		instanceTypes: vultr.NewInstanceTypeProvider(vultrClient),
	}
}

func (c *CloudProvider) Name() string { return "vultr" }
func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&vultrv1.VultrNodeClass{}}
}
func (c *CloudProvider) RepairPolicies() []karpcloud.RepairPolicy     { return nil }
func (c *CloudProvider) DisruptionReasons() []karpv1.DisruptionReason { return nil }

func (c *CloudProvider) nodeClass(ctx context.Context, ref *karpv1.NodeClassReference) (*vultrv1.VultrNodeClass, error) {
	if ref == nil {
		return nil, fmt.Errorf("NodeClassRef is nil")
	}
	if ref.Group != vultrv1.GroupVersion.Group || ref.Kind != "VultrNodeClass" {
		return nil, fmt.Errorf("unsupported NodeClassRef %s/%s", ref.Group, ref.Kind)
	}
	nc := &vultrv1.VultrNodeClass{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: ref.Name}, nc); err != nil {
		return nil, err
	}
	if !nc.DeletionTimestamp.IsZero() {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: vultrv1.GroupVersion.Group, Resource: "vultrnodeclasses"}, nc.Name)
	}
	return nc, nil
}

func (c *CloudProvider) Create(ctx context.Context, nc *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	nodeClass, err := c.nodeClass(ctx, nc.Spec.NodeClassRef)
	if err != nil {
		return nil, fmt.Errorf("resolving NodeClass: %w", err)
	}
	if ready := nodeClass.StatusConditions().Get(status.ConditionReady); ready != nil && ready.IsFalse() {
		return nil, karpcloud.NewNodeClassNotReadyError(fmt.Errorf("NodeClass is not ready: %s", ready.Message))
	}

	requirements := scheduling.NewNodeSelectorRequirementsWithMinValues(nc.Spec.Requirements...)
	plan, planInfo, err := c.selectPlan(ctx, nodeClass.Spec.Region, requirements, nodeClass.Spec.Plan)
	if err != nil {
		return nil, err
	}

	caHash := nodeClass.Spec.CACertHash
	if caHash == "" {
		caHash, err = bootstrap.ClusterCAHash(ctx, c.kubeClient)
		if err != nil {
			return nil, karpcloud.NewCreateError(err, "ClusterCAUnavailable", "Unable to determine the Kubernetes cluster CA hash")
		}
	}

	token, err := c.tokenProvider.Token(ctx)
	if err != nil {
		return nil, karpcloud.NewCreateError(err, "BootstrapTokenUnavailable", "Unable to obtain a kubeadm bootstrap token")
	}

	userData, err := bootstrap.Render(bootstrap.BootstrapConfig{
		KubernetesVersion: nodeClass.Spec.KubernetesVersion,
		ClusterEndpoint:   nodeClass.Spec.ClusterEndpoint,
		CACertHash:        caHash,
		NodeName:          nc.Name,
		ExtraUserData:     nodeClass.Spec.ExtraUserData,
	}, token)
	if err != nil {
		return nil, karpcloud.NewCreateError(err, "BootstrapConfigurationInvalid", "Unable to generate Vultr node bootstrap configuration")
	}

	instance, err := c.vultr.CreateInstance(ctx, vultr.CreateInstanceRequest{
		Region:     nodeClass.Spec.Region,
		Plan:       plan,
		OSID:       nodeClass.Spec.OSID,
		SnapshotID: nodeClass.Spec.SnapshotID,
		Hostname:   nc.Name,
		Label:      nc.Name,
		SSHKeyIDs:  nodeClass.Spec.SSHKeyIDs,
		VPCIDs:     nodeClass.Spec.VPCIDs,
		UserData:   userData,
		EnableIPv6: nodeClass.Spec.EnableIPv6 != nil && *nodeClass.Spec.EnableIPv6,
		Tags: []string{
			fmt.Sprintf("karpenter-nodeclaim=%s", nc.Name),
			fmt.Sprintf("karpenter-nodepool=%s", nc.Labels[karpv1.NodePoolLabelKey]),
			fmt.Sprintf("karpenter-nodeclass=%s", nodeClass.Name),
		},
	})
	if err != nil {
		return nil, classifyCreateError(err)
	}
	return instanceToNodeClaim(instance, nc, nodeClass, &planInfo), nil
}

func (c *CloudProvider) selectPlan(ctx context.Context, region string, requirements scheduling.Requirements, configuredPlan string) (string, vultr.Plan, error) {
	plans, err := c.instanceTypes.Plans(ctx)
	if err != nil {
		return "", vultr.Plan{}, karpcloud.NewCreateError(err, "PlanLookupFailed", "Unable to resolve the Vultr plan catalogue")
	}
	available, err := c.instanceTypes.ValidateRegion(ctx, region)
	if err != nil {
		if apiErr, ok := err.(*vultr.APIError); ok && apiErr.NotFound() {
			return "", vultr.Plan{}, karpcloud.NewCreateError(err, "InvalidRegion", "The configured Vultr region does not exist or is not available to this account")
		}
		return "", vultr.Plan{}, karpcloud.NewCreateError(err, "CapacityLookupFailed", "Unable to determine Vultr regional capacity")
	}

	instanceRequirement := requirements.Get(corev1.LabelInstanceTypeStable)
	matches := func(planID string) bool {
		return scheduling.NewRequirements(instanceRequirement).IsCompatible(
			scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, planID)),
			scheduling.AllowUndefinedWellKnownLabels,
		)
	}

	if configuredPlan != "" {
		plan, ok := plans[configuredPlan]
		if !ok {
			return "", vultr.Plan{}, karpcloud.NewCreateError(fmt.Errorf("Vultr plan %q does not exist", configuredPlan), "InvalidPlan", "The selected Vultr plan is not available in the Vultr plan catalog")
		}
		if !matches(configuredPlan) {
			return "", vultr.Plan{}, karpcloud.NewCreateError(fmt.Errorf("Vultr plan %q conflicts with the NodeClaim instance-type requirements", configuredPlan), "InvalidInstanceConfiguration", "The selected Vultr plan does not satisfy the NodeClaim requirements")
		}
		if _, ok := available[configuredPlan]; !ok {
			return "", vultr.Plan{}, karpcloud.NewInsufficientCapacityError(fmt.Errorf("Vultr plan %q is currently unavailable in region %q", configuredPlan, region))
		}
		return configuredPlan, plan, nil
	}

	var selected vultr.Plan
	found := false
	for id, plan := range plans {
		if _, ok := available[id]; !ok || !matches(id) || plan.VCPUCount <= 0 || plan.RAM <= 0 || plan.MonthlyCost <= 0 || math.IsNaN(plan.MonthlyCost) || math.IsInf(plan.MonthlyCost, 0) {
			continue
		}
		if !found || plan.MonthlyCost < selected.MonthlyCost || (plan.MonthlyCost == selected.MonthlyCost && plan.ID < selected.ID) {
			selected = plan
			found = true
		}
	}
	if !found {
		return "", vultr.Plan{}, karpcloud.NewInsufficientCapacityError(fmt.Errorf("no Vultr plan in region %q satisfies the NodeClaim instance-type requirements", region))
	}
	return selected.ID, selected, nil
}

func classifyCreateError(err error) error {
	if apiErr, ok := err.(*vultr.APIError); ok {
		if apiErr.StatusCode == 409 {
			return karpcloud.NewInsufficientCapacityError(err)
		}
		if apiErr.StatusCode == 400 || apiErr.StatusCode == 422 {
			return karpcloud.NewCreateError(err, "InvalidInstanceConfiguration", "Vultr rejected the requested instance configuration")
		}
	}
	return karpcloud.NewCreateError(err, "InstanceCreateFailed", "Vultr instance creation failed")
}

func (c *CloudProvider) Delete(ctx context.Context, nc *karpv1.NodeClaim) error {
	id, err := parseProviderID(nc.Status.ProviderID)
	if err != nil {
		return err
	}
	err = c.vultr.DeleteInstance(ctx, id)
	if err != nil {
		if apiErr, ok := err.(*vultr.APIError); ok && apiErr.NotFound() {
			return karpcloud.NewNodeClaimNotFoundError(err)
		}
		return err
	}
	return nil
}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	id, err := parseProviderID(providerID)
	if err != nil {
		return nil, err
	}
	instance, err := c.vultr.GetInstance(ctx, id)
	if err != nil {
		if apiErr, ok := err.(*vultr.APIError); ok && apiErr.NotFound() {
			return nil, karpcloud.NewNodeClaimNotFoundError(err)
		}
		return nil, err
	}
	return c.hydrateInstanceNodeClaim(ctx, instance, nil), nil
}

func (c *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	instances, err := c.vultr.ListInstances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*karpv1.NodeClaim, 0, len(instances))
	for i := range instances {
		if !managed(&instances[i]) {
			continue
		}
		out = append(out, c.hydrateInstanceNodeClaim(ctx, &instances[i], nil))
	}
	return out, nil
}

func (c *CloudProvider) hydrateInstanceNodeClaim(ctx context.Context, i *vultr.Instance, original *karpv1.NodeClaim) *karpv1.NodeClaim {
	plan, ok, err := c.instanceTypes.GetPlan(ctx, i.Plan)
	if err != nil || !ok {
		return instanceToNodeClaim(i, original, nil, nil)
	}
	return instanceToNodeClaim(i, original, nil, &plan)
}

func (c *CloudProvider) GetInstanceTypes(ctx context.Context, np *karpv1.NodePool) ([]*karpcloud.InstanceType, error) {
	ref := np.Spec.Template.Spec.NodeClassRef
	nc, err := c.nodeClass(ctx, ref)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return c.instanceTypes.ListForPlan(ctx, nc.Spec.Region, nc.Spec.Plan)
}

func (c *CloudProvider) IsDrifted(ctx context.Context, nc *karpv1.NodeClaim) (karpcloud.DriftReason, error) {
	nodeClass, err := c.nodeClass(ctx, nc.Spec.NodeClassRef)
	if err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if nc.Annotations[vultrv1.NodeClassHashAnnotation] != nodeClass.Hash() {
		return karpcloud.DriftReason("VultrNodeClassDrifted"), nil
	}
	return "", nil
}

func parseProviderID(providerID string) (string, error) {
	const prefix = "vultr://"
	if !strings.HasPrefix(providerID, prefix) || strings.TrimPrefix(providerID, prefix) == "" {
		return "", fmt.Errorf("invalid Vultr provider ID %q", providerID)
	}
	return strings.TrimPrefix(providerID, prefix), nil
}

func managed(i *vultr.Instance) bool {
	for _, t := range i.Tags {
		if strings.HasPrefix(t, "karpenter-nodeclaim=") {
			return true
		}
	}
	return false
}

func instanceToNodeClaim(i *vultr.Instance, original *karpv1.NodeClaim, nc *vultrv1.VultrNodeClass, plan *vultr.Plan) *karpv1.NodeClaim {
	labels := map[string]string{
		corev1.LabelInstanceTypeStable: i.Plan,
		corev1.LabelTopologyRegion:     i.Region,
		corev1.LabelArchStable:         "amd64",
		corev1.LabelOSStable:           "linux",
		karpv1.CapacityTypeLabelKey:    karpv1.CapacityTypeOnDemand,
	}
	if original != nil {
		for key, value := range original.Labels {
			labels[key] = value
		}
	}
	result := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Status: karpv1.NodeClaimStatus{ProviderID: "vultr://" + i.ID}}
	if i.OSID > 0 {
		result.Status.ImageID = fmt.Sprintf("%d", i.OSID)
	}
	if nc != nil {
		result.Annotations = map[string]string{vultrv1.NodeClassHashAnnotation: nc.Hash()}
	}
	if plan != nil {
		it := &karpcloud.InstanceType{
			Name:     plan.ID,
			Capacity: planCapacityForNodeClaim(*plan),
			Overhead: &karpcloud.InstanceTypeOverhead{EvictionThreshold: corev1.ResourceList{corev1.ResourceMemory: *resourceQuantityMemory(plan.RAM)}},
		}
		result.Status.Capacity = it.Capacity
		result.Status.Allocatable = it.Allocatable()
	}
	return result
}

func planCapacityForNodeClaim(plan vultr.Plan) corev1.ResourceList {
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(plan.VCPUCount), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(int64(plan.RAM)*1024*1024, resource.BinarySI),
		corev1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
	}
	if plan.Disk > 0 {
		capacity[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(int64(plan.Disk)*1000*1000*1000, resource.DecimalSI)
	}
	return capacity
}

func resourceQuantityMemory(ramMB int) *resource.Quantity {
	return resource.NewQuantity(int64(float64(ramMB)*1024*1024*0.075), resource.BinarySI)
}
