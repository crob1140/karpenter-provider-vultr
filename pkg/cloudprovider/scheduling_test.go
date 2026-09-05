package cloudprovider

import (
	"context"
	"testing"

	"github.com/awslabs/operatorpkg/status"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloud "sigs.k8s.io/karpenter/pkg/cloudprovider"
	provisioningscheduling "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	vultrv1 "github.com/crob1140/karpenter-provider-vultr/pkg/apis/v1alpha1"
)

func TestSchedulerFiltersUnavailableVultrOffering(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{})
	client := fake.NewClientBuilder().Build()
	cp := fakeCloudProvider()
	cluster := state.NewCluster(clock.RealClock{}, client, cp)

	np := testNodePool("vultr", []string{"cheap", "available"})
	instanceTypes := map[string][]*karpcloud.InstanceType{
		np.Name: {
			testInstanceType("cheap", 0.01, false),
			testInstanceType("available", 0.02, true),
		},
	}

	topology, err := provisioningscheduling.NewTopology(ctx, client, cluster, nil, []*karpv1.NodePool{np}, instanceTypes, nil)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := provisioningscheduling.NewScheduler(ctx, client, []*karpv1.NodePool{np}, cluster, nil, topology, instanceTypes, nil, nil, clock.RealClock{}, nil)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "example/app",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU: *resource.NewMilliQuantity(500, resource.DecimalSI),
				}},
			}},
		},
	}

	result, err := scheduler.Solve(ctx, []*corev1.Pod{pod})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.NewNodeClaims) != 1 {
		t.Fatalf("expected one new NodeClaim, got %d (errors=%v)", len(result.NewNodeClaims), result.PodErrors)
	}
	options := result.NewNodeClaims[0].InstanceTypeOptions
	if len(options) != 1 || options[0].Name != "available" {
		t.Fatalf("expected scheduler to retain only the available plan, got %#v", instanceTypeNames(options))
	}
}

func TestSchedulerAllowsMultiPlanNodePoolAndPrefersAvailableCheapestPlan(t *testing.T) {
	ctx := options.ToContext(context.Background(), &options.Options{})
	client := fake.NewClientBuilder().Build()
	cp := fakeCloudProvider()
	cluster := state.NewCluster(clock.RealClock{}, client, cp)

	np := testNodePool("vultr", []string{"expensive", "cheap"})
	instanceTypes := map[string][]*karpcloud.InstanceType{
		np.Name: {
			testInstanceType("expensive", 0.05, true),
			testInstanceType("cheap", 0.01, true),
		},
	}

	topology, err := provisioningscheduling.NewTopology(ctx, client, cluster, nil, []*karpv1.NodePool{np}, instanceTypes, nil)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := provisioningscheduling.NewScheduler(ctx, client, []*karpv1.NodePool{np}, cluster, nil, topology, instanceTypes, nil, nil, clock.RealClock{}, nil)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app", Image: "example/app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: *resource.NewMilliQuantity(500, resource.DecimalSI),
			}},
		}}},
	}
	result, err := scheduler.Solve(ctx, []*corev1.Pod{pod})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.NewNodeClaims) != 1 {
		t.Fatalf("expected one new NodeClaim, got %d (errors=%v)", len(result.NewNodeClaims), result.PodErrors)
	}
	options := result.NewNodeClaims[0].InstanceTypeOptions
	if len(options) != 2 {
		t.Fatalf("expected both compatible plans to remain scheduler options, got %#v", instanceTypeNames(options))
	}
	instanceTypeReq := result.NewNodeClaims[0].Requirements.Get(corev1.LabelInstanceTypeStable)
	if !instanceTypeReq.Has("cheap") || !instanceTypeReq.Has("expensive") {
		t.Fatalf("expected NodeClaim instance-type requirement to preserve both plans, got %v", instanceTypeReq)
	}
}

func testNodePool(name string, plans []string) *karpv1.NodePool {
	return &karpv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{
			Spec: karpv1.NodeClaimTemplateSpec{
				NodeClassRef: &karpv1.NodeClassReference{Group: vultrv1.GroupVersion.Group, Kind: "VultrNodeClass", Name: "default"},
				Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
					{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: plans},
					{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
					{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}},
				},
			},
		}},
	}
}

func testInstanceType(name string, price float64, available bool) *karpcloud.InstanceType {
	return &karpcloud.InstanceType{
		Name: name,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, name),
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
		),
		Capacity: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(500, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(4096*1024*1024, resource.BinarySI),
			corev1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
		},
		Overhead: &karpcloud.InstanceTypeOverhead{},
		Offerings: karpcloud.Offerings{{
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "syd"),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "syd"),
			),
			Price: price, Available: available,
		}},
	}
}

func instanceTypeNames(types []*karpcloud.InstanceType) []string {
	result := make([]string, 0, len(types))
	for _, it := range types {
		result = append(result, it.Name)
	}
	return result
}

func fakeCloudProvider() karpcloud.CloudProvider {
	return &testCloudProvider{}
}

type testCloudProvider struct{}

func (t *testCloudProvider) Create(context.Context, *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	return nil, nil
}
func (t *testCloudProvider) Delete(context.Context, *karpv1.NodeClaim) error        { return nil }
func (t *testCloudProvider) Get(context.Context, string) (*karpv1.NodeClaim, error) { return nil, nil }
func (t *testCloudProvider) List(context.Context) ([]*karpv1.NodeClaim, error)      { return nil, nil }
func (t *testCloudProvider) GetInstanceTypes(context.Context, *karpv1.NodePool) ([]*karpcloud.InstanceType, error) {
	return nil, nil
}
func (t *testCloudProvider) IsDrifted(context.Context, *karpv1.NodeClaim) (karpcloud.DriftReason, error) {
	return "", nil
}
func (t *testCloudProvider) RepairPolicies() []karpcloud.RepairPolicy { return nil }
func (t *testCloudProvider) Name() string                             { return "test" }
func (t *testCloudProvider) GetSupportedNodeClasses() []status.Object { return nil }
