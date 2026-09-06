package controllers

import (
	"context"
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/crob1140/karpenter-provider-vultr/pkg/vultr"
)

func TestNodeClaimNameRequiresMatchingClusterTag(t *testing.T) {
	owned := &vultr.Instance{Tags: []string{"karpenter-cluster=prod", "karpenter-nodeclaim=abc123"}}
	if got, ok := vultr.NodeClaimName(owned, "prod"); !ok || got != "abc123" {
		t.Fatalf("got %q, %v", got, ok)
	}

	// An instance created by a different Karpenter installation sharing the
	// same Vultr account must never be treated as ours.
	if _, ok := vultr.NodeClaimName(owned, "staging"); ok {
		t.Fatal("expected an instance tagged for another cluster to be ignored")
	}

	// Pre-cluster-tag instances are also ignored: adopting them would risk
	// deleting instances that belong to somebody else.
	untagged := &vultr.Instance{Tags: []string{"karpenter-nodeclaim=abc123"}}
	if _, ok := vultr.NodeClaimName(untagged, "prod"); ok {
		t.Fatal("expected an instance with no cluster tag to be ignored")
	}

	empty := &vultr.Instance{Tags: []string{"karpenter-cluster=prod", "karpenter-nodeclaim="}}
	if _, ok := vultr.NodeClaimName(empty, "prod"); ok {
		t.Fatal("expected empty nodeclaim tag to be rejected")
	}
}

func TestInstanceTagsRoundTrip(t *testing.T) {
	tags := vultr.InstanceTags("prod", "default-abc12", "default", "vultr-default")
	instance := &vultr.Instance{Tags: tags}

	name, ok := vultr.NodeClaimName(instance, "prod")
	if !ok || name != "default-abc12" {
		t.Fatalf("NodeClaimName() = %q, %v", name, ok)
	}
	if pool, ok := vultr.TagValue(tags, vultr.TagNodePoolPrefix); !ok || pool != "default" {
		t.Fatalf("nodepool tag = %q, %v", pool, ok)
	}
	if class, ok := vultr.TagValue(tags, vultr.TagNodeClassPrefix); !ok || class != "vultr-default" {
		t.Fatalf("nodeclass tag = %q, %v", class, ok)
	}
}

var orphanTestNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

// orphanTestAPI is a minimal Vultr instances endpoint that records deletions.
type orphanTestAPI struct {
	*httptest.Server
	mu           sync.Mutex
	instances    []vultr.Instance
	deleted      []string
	listStatus   int
	deleteStatus map[string]int
}

func newOrphanTestAPI(t *testing.T, instances []vultr.Instance) *orphanTestAPI {
	t.Helper()
	api := &orphanTestAPI{instances: instances, deleteStatus: map[string]int{}}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/instances":
			if api.listStatus != 0 {
				http.Error(w, "vultr api failure", api.listStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"instances": api.instances,
				"meta":      map[string]any{"links": map[string]string{"next": ""}},
			})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v2/instances/"):
			id := strings.TrimPrefix(r.URL.Path, "/v2/instances/")
			if code, ok := api.deleteStatus[id]; ok {
				http.Error(w, "vultr api failure", code)
				return
			}
			api.deleted = append(api.deleted, id)
			w.WriteHeader(http.StatusNoContent)

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	return api
}

func (a *orphanTestAPI) deletedIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]string(nil), a.deleted...)
	sort.Strings(out)
	return out
}

// orphanInstance builds an instance owned by cluster "prod", created ageMinutes
// ago relative to the controller's fixed clock.
func orphanInstance(id, nodeClaim string, ageMinutes int) vultr.Instance {
	return vultr.Instance{
		ID:          id,
		Tags:        vultr.InstanceTags("prod", nodeClaim, "default", "vultr-default"),
		DateCreated: orphanTestNow.Add(-time.Duration(ageMinutes) * time.Minute).UTC().Format(time.RFC3339),
	}
}

func newOrphanController(t *testing.T, api *orphanTestAPI, nodeClaims ...*karpv1.NodeClaim) *OrphanController {
	t.Helper()

	scheme := runtime.NewScheme()
	gv := schema.GroupVersion{Group: "karpenter.sh", Version: "v1"}
	scheme.AddKnownTypes(gv, &karpv1.NodeClaim{}, &karpv1.NodeClaimList{})
	metav1.AddToGroupVersion(scheme, gv)

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, nc := range nodeClaims {
		builder = builder.WithObjects(nc)
	}

	controller := NewOrphanController(builder.Build(), vultr.NewClientWithBaseURL("test", api.URL+"/v2", api.Client()), "prod")
	controller.now = func() time.Time { return orphanTestNow }
	return controller
}

func nodeClaimNamed(name string) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func TestOrphanReconcileDeletesInstanceWithNoNodeClaim(t *testing.T) {
	api := newOrphanTestAPI(t, []vultr.Instance{orphanInstance("i-orphan", "default-gone", 30)})
	controller := newOrphanController(t, api)

	result, err := controller.Reconcile(context.Background(), reconcile.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if got := api.deletedIDs(); len(got) != 1 || got[0] != "i-orphan" {
		t.Fatalf("deleted = %v, want [i-orphan]", got)
	}
	if result.RequeueAfter != orphanScanInterval {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, orphanScanInterval)
	}
}

func TestOrphanReconcilePreservesInstancesItMustNotTouch(t *testing.T) {
	otherCluster := vultr.Instance{
		ID:          "i-other-cluster",
		Tags:        vultr.InstanceTags("staging", "staging-abc", "default", "vultr-default"),
		DateCreated: orphanTestNow.Add(-time.Hour).UTC().Format(time.RFC3339),
	}
	untagged := vultr.Instance{
		ID:          "i-unmanaged",
		Tags:        []string{"some-unrelated-tag"},
		DateCreated: orphanTestNow.Add(-time.Hour).UTC().Format(time.RFC3339),
	}
	unparseableDate := vultr.Instance{
		ID:          "i-bad-date",
		Tags:        vultr.InstanceTags("prod", "default-baddate", "default", "vultr-default"),
		DateCreated: "not-a-timestamp",
	}

	api := newOrphanTestAPI(t, []vultr.Instance{
		otherCluster,
		untagged,
		unparseableDate,
		// Inside the grace period: the instance may still be bootstrapping.
		orphanInstance("i-young", "default-young", 5),
		// Aged out, but its NodeClaim still exists.
		orphanInstance("i-live", "default-live", 60),
		// Aged out, NodeClaim is terminating: Karpenter owns this teardown.
		orphanInstance("i-terminating", "default-terminating", 60),
		// The only genuine orphan.
		orphanInstance("i-orphan", "default-gone", 60),
	})

	terminating := nodeClaimNamed("default-terminating")
	terminating.Finalizers = []string{"karpenter.sh/termination"}
	deletedAt := metav1.NewTime(orphanTestNow.Add(-time.Minute))
	terminating.DeletionTimestamp = &deletedAt

	controller := newOrphanController(t, api, nodeClaimNamed("default-live"), terminating)

	if _, err := controller.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatal(err)
	}
	if got := api.deletedIDs(); len(got) != 1 || got[0] != "i-orphan" {
		t.Fatalf("deleted = %v, want only [i-orphan]", got)
	}
}

// A 404 means somebody else already removed the instance, which is the outcome
// the cleaner wanted; it must not abort the rest of the scan.
func TestOrphanReconcileToleratesAlreadyDeletedInstance(t *testing.T) {
	api := newOrphanTestAPI(t, []vultr.Instance{
		orphanInstance("i-gone", "default-gone-1", 60),
		orphanInstance("i-orphan", "default-gone-2", 60),
	})
	api.deleteStatus["i-gone"] = http.StatusNotFound

	controller := newOrphanController(t, api)
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("a 404 on delete should not fail the scan, got %v", err)
	}
	if got := api.deletedIDs(); len(got) != 1 || got[0] != "i-orphan" {
		t.Fatalf("deleted = %v, want [i-orphan]", got)
	}
}

func TestOrphanReconcileSurfacesAPIErrors(t *testing.T) {
	t.Run("list failure", func(t *testing.T) {
		api := newOrphanTestAPI(t, nil)
		api.listStatus = http.StatusServiceUnavailable
		if _, err := newOrphanController(t, api).Reconcile(context.Background(), reconcile.Request{}); err == nil {
			t.Fatal("expected a listing error to be returned")
		}
	})

	t.Run("delete failure", func(t *testing.T) {
		api := newOrphanTestAPI(t, []vultr.Instance{orphanInstance("i-orphan", "default-gone", 60)})
		api.deleteStatus["i-orphan"] = http.StatusInternalServerError
		_, err := newOrphanController(t, api).Reconcile(context.Background(), reconcile.Request{})
		if err == nil {
			t.Fatal("expected a delete error to be returned")
		}
		if !strings.Contains(err.Error(), "i-orphan") {
			t.Fatalf("error should name the instance, got %v", err)
		}
	})
}

// Guards the grace-period boundary: instances younger than it are left alone so
// a slow bootstrap is never killed mid-flight.
func TestOrphanReconcileRespectsGracePeriodBoundary(t *testing.T) {
	for _, tt := range []struct {
		name       string
		created    time.Time
		wantDelete bool
	}{
		{"one minute inside", orphanTestNow.Add(-orphanGracePeriod + time.Minute), false},
		{"exactly at the boundary", orphanTestNow.Add(-orphanGracePeriod), true},
		{"one minute past", orphanTestNow.Add(-orphanGracePeriod - time.Minute), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := newOrphanTestAPI(t, []vultr.Instance{{
				ID:          "i-orphan",
				Tags:        vultr.InstanceTags("prod", "default-gone", "default", "vultr-default"),
				DateCreated: tt.created.UTC().Format(time.RFC3339),
			}})
			if _, err := newOrphanController(t, api).Reconcile(context.Background(), reconcile.Request{}); err != nil {
				t.Fatal(err)
			}
			if got := len(api.deletedIDs()) == 1; got != tt.wantDelete {
				t.Fatalf("deleted = %v, want %v (created %s before now)", got, tt.wantDelete, orphanTestNow.Sub(tt.created))
			}
		})
	}
}
