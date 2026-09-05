package vultr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

func TestInstanceTypeProviderMarksRegionalAvailability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plans":
			_ = json.NewEncoder(w).Encode(map[string]any{"plans": []any{
				map[string]any{"id": "p1", "vcpu_count": 1, "ram": 1024, "monthly_cost": 5},
				map[string]any{"id": "p2", "vcpu_count": 2, "ram": 2048, "monthly_cost": 10},
			}, "meta": map[string]any{"links": map[string]string{"next": ""}}})
		case "/v2/regions/syd/availability":
			_, _ = w.Write([]byte(`{"available_plans":["p1"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClientWithBaseURL("test", server.URL+"/v2", server.Client())
	provider := NewInstanceTypeProvider(client)
	types, err := provider.List(context.Background(), "syd")
	if err != nil || len(types) != 2 {
		t.Fatalf("List() = %d types, %v", len(types), err)
	}
	byName := map[string]*cloudprovider.InstanceType{}
	for _, it := range types {
		byName[it.Name] = it
	}
	if !byName["p1"].Offerings[0].Available || byName["p2"].Offerings[0].Available {
		t.Fatalf("unexpected availability: p1=%v p2=%v", byName["p1"].Offerings[0].Available, byName["p2"].Offerings[0].Available)
	}
	for _, it := range types {
		offering := it.Offerings[0]
		if offering.Zone() != "syd" || offering.Requirements.Get(corev1.LabelTopologyRegion).Any() != "syd" {
			t.Fatalf("expected synthetic Vultr zone/region syd for %s, got %s / %s", it.Name, offering.Zone(), offering.Requirements.Get(corev1.LabelTopologyRegion).Any())
		}
	}
}

func TestInstanceTypeProviderCachesPlansAndAvailability(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plans":
			_, _ = w.Write([]byte(`{"plans":[{"id":"p1","vcpu_count":1,"ram":1024,"monthly_cost":5}],"meta":{"links":{"next":""}}}`))
		case "/v2/regions/syd/availability":
			_, _ = w.Write([]byte(`{"available_plans":["p1"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := NewInstanceTypeProvider(NewClientWithBaseURL("test", server.URL+"/v2", server.Client()))
	if _, err := provider.List(context.Background(), "syd"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.List(context.Background(), "syd"); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("expected one plans request and one availability request, got %d total requests", requests)
	}
}

func TestInstanceTypeProviderUsesStaleCacheOnRefreshFailure(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests <= 2 {
			if r.URL.Path == "/v2/plans" {
				_, _ = w.Write([]byte(`{"plans":[{"id":"p1","vcpu_count":1,"ram":1024,"monthly_cost":5}],"meta":{"links":{"next":""}}}`))
			} else {
				_, _ = w.Write([]byte(`{"available_plans":["p1"]}`))
			}
			return
		}
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	provider := NewInstanceTypeProvider(NewClientWithBaseURL("test", server.URL+"/v2", server.Client()))
	if _, err := provider.List(context.Background(), "syd"); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	entry := provider.cache["syd"]
	entry.refreshedAt = entry.refreshedAt.Add(-availabilityCacheTTL)
	provider.cache["syd"] = entry
	provider.plans.refreshedAt = provider.plans.refreshedAt.Add(-planCacheTTL)
	provider.mu.Unlock()

	types, err := provider.List(context.Background(), "syd")
	if err != nil {
		t.Fatalf("expected stale cache fallback, got %v", err)
	}
	if len(types) != 1 || !types[0].Offerings[0].Available {
		t.Fatalf("unexpected stale result: %#v", types)
	}
}

func TestInstanceTypeProviderFixedPlanFiltersCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plans":
			_, _ = w.Write([]byte(`{"plans":[{"id":"p1","vcpu_count":1,"ram":1024,"monthly_cost":5},{"id":"p2","vcpu_count":2,"ram":2048,"monthly_cost":10}],"meta":{"links":{"next":""}}}`))
		case "/v2/regions/syd/availability":
			_, _ = w.Write([]byte(`{"available_plans":["p1","p2"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := NewInstanceTypeProvider(NewClientWithBaseURL("test", server.URL+"/v2", server.Client()))
	types, err := provider.ListForPlan(context.Background(), "syd", "p2")
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 1 || types[0].Name != "p2" {
		t.Fatalf("expected only p2, got %#v", types)
	}
}

func TestInstanceTypesOrderByPriceIgnoresUnavailableOffering(t *testing.T) {
	makeType := func(name string, price float64, available bool) *cloudprovider.InstanceType {
		return &cloudprovider.InstanceType{
			Name: name,
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, name),
			),
			Offerings: cloudprovider.Offerings{{
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "syd"),
				),
				Price: price, Available: available,
			}},
		}
	}
	its := cloudprovider.InstanceTypes{makeType("cheap-but-unavailable", 0.01, false), makeType("available", 0.02, true)}
	its.OrderByPrice(scheduling.NewRequirements())
	if its[0].Name != "available" {
		t.Fatalf("expected unavailable cheap offering to be ignored, got %s first", its[0].Name)
	}
}

func TestPlanCapacityIncludesDiskAndPods(t *testing.T) {
	plan := Plan{ID: "p1", VCPUCount: 2, RAM: 4096, Disk: 50}
	capacity := planCapacity(plan)

	cpu := capacity[corev1.ResourceCPU]
	if got := cpu.Value(); got != 2 {
		t.Fatalf("cpu = %d", got)
	}

	pods := capacity[corev1.ResourcePods]
	if got := pods.Value(); got != 110 {
		t.Fatalf("pods = %d", got)
	}

	storage := capacity[corev1.ResourceEphemeralStorage]
	if got := storage.Value(); got != 50_000_000_000 {
		t.Fatalf("ephemeral storage = %d", got)
	}
}
