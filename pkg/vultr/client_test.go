package vultr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// The Vultr create-instance body names these fields `sshkey_id` and
// `attach_vpc`. Vultr ignores unknown fields, so sending `sshkey_ids`/`vpc_ids`
// silently produces an instance with no keys and no VPC rather than an error.
func TestCreateInstanceUsesVultrFieldNames(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"instance":{"id":"abc"}}`))
	}))
	defer server.Close()

	client := NewClientWithBaseURL("test", server.URL+"/v2", server.Client())
	if _, err := client.CreateInstance(context.Background(), CreateInstanceRequest{
		Region:    "syd",
		Plan:      "vc2-1c-2gb",
		SSHKeyIDs: []string{"key-1"},
		VPCIDs:    []string{"vpc-1"},
	}); err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{"sshkey_ids", "vpc_ids"} {
		if _, ok := payload[field]; ok {
			t.Fatalf("create payload still sends non-existent Vultr field %q", field)
		}
	}
	if got := payload["sshkey_id"]; !reflect.DeepEqual(got, []any{"key-1"}) {
		t.Fatalf("sshkey_id = %#v", got)
	}
	if got := payload["attach_vpc"]; !reflect.DeepEqual(got, []any{"vpc-1"}) {
		t.Fatalf("attach_vpc = %#v", got)
	}
}

// Vultr's meta.links.next is an opaque cursor token, not a URL, and is passed
// straight back as ?cursor=<token>.
func TestListPlansFollowsBareCursorToken(t *testing.T) {
	const cursor = "bmV4dF9fMTMxOTgxNQ=="
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == cursor {
			w.Write([]byte(`{"plans":[{"id":"p2","vcpu_count":2,"ram":2048}],"meta":{"total":2,"links":{"next":"","prev":"cHJldg=="}}}`))
			return
		}
		fmt.Fprintf(w, `{"plans":[{"id":"p1","vcpu_count":1,"ram":1024}],"meta":{"total":2,"links":{"next":%q,"prev":""}}}`, cursor)
	}))
	defer server.Close()
	client := NewClientWithBaseURL("test", server.URL+"/v2", server.Client())
	plans, err := client.ListPlans(context.Background())
	if err != nil || len(plans) != 2 || plans[1].ID != "p2" {
		t.Fatalf("ListPlans() = %#v, %v", plans, err)
	}
}

func TestListInstancesFollowsBareCursorToken(t *testing.T) {
	const cursor = "bmV4dF9fOTk5"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == cursor {
			w.Write([]byte(`{"instances":[{"id":"i2"}],"meta":{"total":2,"links":{"next":""}}}`))
			return
		}
		fmt.Fprintf(w, `{"instances":[{"id":"i1"}],"meta":{"total":2,"links":{"next":%q}}}`, cursor)
	}))
	defer server.Close()
	client := NewClientWithBaseURL("test", server.URL+"/v2", server.Client())
	instances, err := client.ListInstances(context.Background())
	if err != nil || len(instances) != 2 || instances[1].ID != "i2" {
		t.Fatalf("ListInstances() = %#v, %v", instances, err)
	}
}

// Link-style pagination is also tolerated so a future API change doesn't break
// the client.
func TestListPlansFollowsCursorURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "next" {
			w.Write([]byte(`{"plans":[{"id":"p2","vcpu_count":2,"ram":2048}],"meta":{"total":2,"links":{"next":""}}}`))
			return
		}
		w.Write([]byte(`{"plans":[{"id":"p1","vcpu_count":1,"ram":1024}],"meta":{"total":2,"links":{"next":"https://api.vultr.com/v2/plans?cursor=next"}}}`))
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
		if r.URL.Query().Get("cursor") == "b3NfX25leHQ=" {
			w.Write([]byte(`{"os":[{"id":2,"name":"Ubuntu","arch":"x86_64","family":"ubuntu"}],"meta":{"links":{"next":""}}}`))
			return
		}
		w.Write([]byte(`{"os":[{"id":1,"name":"Ubuntu","arch":"x86_64","family":"ubuntu"}],"meta":{"links":{"next":"b3NfX25leHQ="}}}`))
	}))
	defer server.Close()
	client := NewClientWithBaseURL("test", server.URL+"/v2", server.Client())
	images, err := client.ListOS(context.Background())
	if err != nil || len(images) != 2 || images[1].ID != 2 {
		t.Fatalf("ListOS() = %#v, %v", images, err)
	}
}
