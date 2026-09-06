package cloudprovider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloud "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/crob1140/karpenter-provider-vultr/pkg/vultr"
)

func TestParseProviderID(t *testing.T) {
	got, err := parseProviderID("vultr://abc123")
	if err != nil || got != "abc123" {
		t.Fatalf("got %q, err %v", got, err)
	}
	if _, err := parseProviderID("aws:///syd/abc123"); err == nil {
		t.Fatal("expected invalid provider ID")
	}
}

func TestSelectPlanIntersectsMultiPlanNodeClaimRequirementAndAvailability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plans":
			_, _ = w.Write([]byte(`{"plans":[
				{"id":"cheap","vcpu_count":1,"ram":1024,"monthly_cost":5},
				{"id":"expensive","vcpu_count":2,"ram":2048,"monthly_cost":10}
			],"meta":{"links":{"next":""}}}`))
		case "/v2/regions/syd/availability":
			_, _ = w.Write([]byte(`{"available_plans":["cheap","expensive"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := New(nil, vultr.NewClientWithBaseURL("test", server.URL+"/v2", server.Client()), "test-cluster")
	requirements := scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "cheap", "expensive"))

	selected, plan, err := provider.selectPlan(context.Background(), "syd", requirements, "")
	if err != nil {
		t.Fatal(err)
	}
	if selected != "cheap" || plan.ID != "cheap" {
		t.Fatalf("expected cheapest compatible plan cheap, got %q (%#v)", selected, plan)
	}
}

// Plan selection iterates a map, so equal prices must be broken by a stable
// key or the chosen plan varies run to run.
func TestSelectPlanBreaksPriceTiesDeterministically(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plans":
			_, _ = w.Write([]byte(`{"plans":[
				{"id":"plan-c","vcpu_count":1,"ram":1024,"monthly_cost":5},
				{"id":"plan-a","vcpu_count":1,"ram":1024,"monthly_cost":5},
				{"id":"plan-b","vcpu_count":1,"ram":1024,"monthly_cost":5}
			],"meta":{"links":{"next":""}}}`))
		case "/v2/regions/syd/availability":
			_, _ = w.Write([]byte(`{"available_plans":["plan-a","plan-b","plan-c"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	requirements := scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "plan-a", "plan-b", "plan-c"))
	for i := 0; i < 20; i++ {
		provider := New(nil, vultr.NewClientWithBaseURL("test", server.URL+"/v2", server.Client()), "test-cluster")
		selected, _, err := provider.selectPlan(context.Background(), "syd", requirements, "")
		if err != nil {
			t.Fatal(err)
		}
		if selected != "plan-a" {
			t.Fatalf("iteration %d selected %q, want the lowest-ID plan at the tied price", i, selected)
		}
	}
}

func TestClassifyCreateError(t *testing.T) {
	tests := []struct {
		status            int
		insufficient      bool
		conditionReason   string
		expectCreateError bool
	}{
		{status: 409, insufficient: true},
		{status: 400, conditionReason: "InvalidInstanceConfiguration", expectCreateError: true},
		{status: 422, conditionReason: "InvalidInstanceConfiguration", expectCreateError: true},
		{status: 500, conditionReason: "InstanceCreateFailed", expectCreateError: true},
	}
	for _, tt := range tests {
		err := classifyCreateError(&vultr.APIError{Method: "POST", Path: "/instances", StatusCode: tt.status})
		if got := karpcloud.IsInsufficientCapacityError(err); got != tt.insufficient {
			t.Fatalf("HTTP %d: IsInsufficientCapacityError = %v, want %v", tt.status, got, tt.insufficient)
		}
		if !tt.expectCreateError {
			continue
		}
		var createErr *karpcloud.CreateError
		if !errors.As(err, &createErr) {
			t.Fatalf("HTTP %d: expected a CreateError, got %T", tt.status, err)
		}
		if createErr.ConditionReason != tt.conditionReason {
			t.Fatalf("HTTP %d: condition reason = %q, want %q", tt.status, createErr.ConditionReason, tt.conditionReason)
		}
	}
}

// Karpenter only removes a NodeClaim's finalizer once Delete reports the
// instance is gone, and the node termination controller relies on the same
// signal from Get.
func TestGetAndDeleteTranslateNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	provider := New(nil, vultr.NewClientWithBaseURL("test", server.URL+"/v2", server.Client()), "test-cluster")

	err := provider.Delete(context.Background(), &karpv1.NodeClaim{
		Status: karpv1.NodeClaimStatus{ProviderID: "vultr://gone"},
	})
	if !karpcloud.IsNodeClaimNotFoundError(err) {
		t.Fatalf("Delete() of a missing instance = %v, want NodeClaimNotFoundError", err)
	}

	if _, err := provider.Get(context.Background(), "vultr://gone"); !karpcloud.IsNodeClaimNotFoundError(err) {
		t.Fatalf("Get() of a missing instance = %v, want NodeClaimNotFoundError", err)
	}
}

func TestSelectPlanNeverChoosesUnavailablePlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plans":
			_, _ = w.Write([]byte(`{"plans":[
				{"id":"cheap","vcpu_count":1,"ram":1024,"monthly_cost":5},
				{"id":"available","vcpu_count":2,"ram":2048,"monthly_cost":10}
			],"meta":{"links":{"next":""}}}`))
		case "/v2/regions/syd/availability":
			_, _ = w.Write([]byte(`{"available_plans":["available"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := New(nil, vultr.NewClientWithBaseURL("test", server.URL+"/v2", server.Client()), "test-cluster")
	requirements := scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "cheap", "available"))

	selected, _, err := provider.selectPlan(context.Background(), "syd", requirements, "")
	if err != nil {
		t.Fatal(err)
	}
	if selected != "available" {
		t.Fatalf("expected available plan, got %q", selected)
	}
}
