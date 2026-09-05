package vultr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateInstanceEncodesUserData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/instances" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"instance":{"id":"abc","plan":"vc2-1c-2gb","region":"syd"}}`))
	}))
	defer server.Close()

	client := NewClientWithBaseURL("test", server.URL+"/v2", server.Client())
	got, err := client.CreateInstance(context.Background(), CreateInstanceRequest{Region: "syd", Plan: "vc2-1c-2gb", UserData: "#cloud-config\n"})
	if err != nil || got.ID != "abc" {
		t.Fatalf("CreateInstance() = %#v, %v", got, err)
	}
}

func TestListPlansFollowsCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "next" {
			w.Write([]byte(`{"plans":[{"id":"p2","vcpu_count":2,"ram":2048}],"meta":{"total":2,"links":{"next":""}}}`))
			return
		}
		w.Write([]byte(`{"plans":[{"id":"p1","vcpu_count":1,"ram":1024}],"meta":{"total":2,"links":{"next":"http://example.test/v2/plans?cursor=next"}}}`))
	}))
	defer server.Close()
	client := NewClientWithBaseURL("test", server.URL+"/v2", server.Client())
	plans, err := client.ListPlans(context.Background())
	if err != nil || len(plans) != 2 || plans[1].ID != "p2" {
		t.Fatalf("ListPlans() = %#v, %v", plans, err)
	}
}

func TestGetRegionAvailability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"available_plans":["p1","p2"]}`))
	}))
	defer server.Close()
	client := NewClientWithBaseURL("test", server.URL+"/v2", server.Client())
	availability, err := client.GetRegionAvailability(context.Background(), "syd")
	if err != nil || len(availability.AvailablePlans) != 2 {
		t.Fatalf("GetRegionAvailability() = %#v, %v", availability, err)
	}
}

func TestListOSFollowsCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "next" {
			w.Write([]byte(`{"os":[{"id":2,"name":"Ubuntu","arch":"x86_64","family":"ubuntu"}],"meta":{"links":{"next":""}}}`))
			return
		}
		w.Write([]byte(`{"os":[{"id":1,"name":"Ubuntu","arch":"x86_64","family":"ubuntu"}],"meta":{"links":{"next":"http://example.test/v2/os?cursor=next"}}}`))
	}))
	defer server.Close()
	client := NewClientWithBaseURL("test", server.URL+"/v2", server.Client())
	images, err := client.ListOS(context.Background())
	if err != nil || len(images) != 2 || images[1].ID != 2 {
		t.Fatalf("ListOS() = %#v, %v", images, err)
	}
}
