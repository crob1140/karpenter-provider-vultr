package cloudprovider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
