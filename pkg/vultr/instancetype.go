package vultr

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const (
	availabilityCacheTTL = 60 * time.Second
	planCacheTTL         = 5 * time.Minute
	memoryOverheadRatio  = 0.075
	defaultMaxPods       = 110
	vultrZoneLabel       = corev1.LabelTopologyZone
)

type availabilityCacheEntry struct {
	plans       map[string]struct{}
	refreshedAt time.Time
}

type planCacheEntry struct {
	plans       map[string]Plan
	refreshedAt time.Time
}

type InstanceTypeProvider struct {
	client *Client
	mu     sync.Mutex
	cache  map[string]availabilityCacheEntry
	plans  *planCacheEntry
}

func NewInstanceTypeProvider(client *Client) *InstanceTypeProvider {
	return &InstanceTypeProvider{client: client, cache: map[string]availabilityCacheEntry{}}
}

// Plans returns the Vultr plan catalogue. The catalogue is cached because it changes
// much less frequently than Karpenter asks for instance types. A stale successful
// catalogue is retained if a refresh fails, so a transient API failure doesn't make
// every NodePool appear empty.
func (p *InstanceTypeProvider) Plans(ctx context.Context) (map[string]Plan, error) {
	now := time.Now()
	p.mu.Lock()
	cached := p.plans
	if cached != nil && now.Sub(cached.refreshedAt) < planCacheTTL {
		result := clonePlans(cached.plans)
		p.mu.Unlock()
		return result, nil
	}
	p.mu.Unlock()

	plans, err := p.client.ListPlans(ctx)
	if err != nil {
		if cached != nil && staleCacheUsable(err) {
			return clonePlans(cached.plans), nil
		}
		return nil, err
	}
	byID := make(map[string]Plan, len(plans))
	for _, plan := range plans {
		if plan.ID != "" {
			byID[plan.ID] = plan
		}
	}

	p.mu.Lock()
	p.plans = &planCacheEntry{plans: byID, refreshedAt: now}
	result := clonePlans(byID)
	p.mu.Unlock()
	return result, nil
}

func (p *InstanceTypeProvider) GetPlan(ctx context.Context, id string) (Plan, bool, error) {
	plans, err := p.Plans(ctx)
	if err != nil {
		return Plan{}, false, err
	}
	plan, ok := plans[id]
	return plan, ok, nil
}

func (p *InstanceTypeProvider) PlanAvailable(ctx context.Context, region, plan string) (bool, error) {
	available, err := p.availablePlans(ctx, region)
	if err != nil {
		return false, err
	}
	_, ok := available[plan]
	return ok, nil
}

func (p *InstanceTypeProvider) ValidateRegion(ctx context.Context, region string) (map[string]struct{}, error) {
	return p.availablePlans(ctx, region)
}

// List returns every valid Vultr plan from the catalogue, including plans that are
// currently unavailable in the selected region. Karpenter deliberately needs those
// unavailable instance types to remain present so an availability change can be
// observed without changing the instance-type universe.
func (p *InstanceTypeProvider) List(ctx context.Context, region string) ([]*cloudprovider.InstanceType, error) {
	return p.list(ctx, region, "")
}

func (p *InstanceTypeProvider) ListForPlan(ctx context.Context, region, configuredPlan string) ([]*cloudprovider.InstanceType, error) {
	return p.list(ctx, region, configuredPlan)
}

func (p *InstanceTypeProvider) list(ctx context.Context, region, configuredPlan string) ([]*cloudprovider.InstanceType, error) {
	plans, err := p.Plans(ctx)
	if err != nil {
		return nil, err
	}
	available, err := p.availablePlans(ctx, region)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(plans))
	for id := range plans {
		if configuredPlan == "" || id == configuredPlan {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	result := make([]*cloudprovider.InstanceType, 0, len(ids))
	for _, id := range ids {
		plan := plans[id]
		if plan.ID == "" || plan.VCPUCount <= 0 || plan.RAM <= 0 || plan.MonthlyCost <= 0 || math.IsNaN(plan.MonthlyCost) || math.IsInf(plan.MonthlyCost, 0) {
			continue
		}

		_, isAvailable := available[plan.ID]
		capacity := planCapacity(plan)
		overhead := &cloudprovider.InstanceTypeOverhead{
			KubeReserved:   corev1.ResourceList{},
			SystemReserved: corev1.ResourceList{},
			EvictionThreshold: corev1.ResourceList{
				corev1.ResourceMemory: *resource.NewQuantity(int64(float64(plan.RAM)*1024*1024*memoryOverheadRatio), resource.BinarySI),
			},
		}

		// Vultr currently exposes a regional placement model rather than a separate
		// availability-zone model. Karpenter requires every offering to carry a zone
		// label, so use the Vultr region as the provider's single synthetic zone.
		requirements := scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, plan.ID),
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
			scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
			scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region),
			scheduling.NewRequirement(vultrZoneLabel, corev1.NodeSelectorOpIn, region),
		)

		offeringRequirements := scheduling.NewRequirements(
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
			scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region),
			scheduling.NewRequirement(vultrZoneLabel, corev1.NodeSelectorOpIn, region),
		)

		result = append(result, &cloudprovider.InstanceType{
			Name:         plan.ID,
			Requirements: requirements,
			Capacity:     capacity,
			Overhead:     overhead,
			Offerings: cloudprovider.Offerings{{
				Requirements: offeringRequirements,
				Price:        plan.MonthlyCost / 730.0,
				Available:    isAvailable,
			}},
		})
	}
	return result, nil
}

func planCapacity(plan Plan) corev1.ResourceList {
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(plan.VCPUCount), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(int64(plan.RAM)*1024*1024, resource.BinarySI),
		corev1.ResourcePods:   *resource.NewQuantity(defaultMaxPods, resource.DecimalSI),
	}
	if plan.Disk > 0 {
		capacity[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(int64(plan.Disk)*1000*1000*1000, resource.DecimalSI)
	}
	return capacity
}

func (p *InstanceTypeProvider) availablePlans(ctx context.Context, region string) (map[string]struct{}, error) {
	now := time.Now()
	p.mu.Lock()
	cached, ok := p.cache[region]
	if ok && now.Sub(cached.refreshedAt) < availabilityCacheTTL {
		result := cloneSet(cached.plans)
		p.mu.Unlock()
		return result, nil
	}
	p.mu.Unlock()

	availability, err := p.client.GetRegionAvailability(ctx, region)
	if err != nil {
		if ok && staleCacheUsable(err) {
			return cloneSet(cached.plans), nil
		}
		return nil, err
	}
	plans := make(map[string]struct{}, len(availability.AvailablePlans))
	for _, id := range availability.AvailablePlans {
		if id != "" {
			plans[id] = struct{}{}
		}
	}

	p.mu.Lock()
	p.cache[region] = availabilityCacheEntry{plans: plans, refreshedAt: now}
	result := cloneSet(plans)
	p.mu.Unlock()
	return result, nil
}

func staleCacheUsable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	return true
}

func cloneSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

func clonePlans(in map[string]Plan) map[string]Plan {
	out := make(map[string]Plan, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func BuildInstanceTypes(ctx context.Context, client *Client, region string) ([]*cloudprovider.InstanceType, error) {
	return NewInstanceTypeProvider(client).List(ctx, region)
}
