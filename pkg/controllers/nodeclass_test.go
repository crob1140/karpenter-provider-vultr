package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vultrv1 "github.com/crob1140/karpenter-provider-vultr/pkg/apis/v1alpha1"
	"github.com/crob1140/karpenter-provider-vultr/pkg/vultr"
)

// vultrAPIResponses configures the fake Vultr API backing a reconcile. A zero
// status means 200 with the paired body.
type vultrAPIResponses struct {
	os           string
	osStatus     int
	plans        string
	plansStatus  int
	avail        string
	availStatus  int
	requestCount *int
}

const (
	defaultOSBody    = `{"os":[{"id":1743,"name":"Ubuntu 24.04 LTS x64","arch":"x64","family":"ubuntu"}],"meta":{"links":{"next":""}}}`
	defaultPlansBody = `{"plans":[{"id":"vc2-1c-2gb","vcpu_count":1,"ram":2048,"monthly_cost":10},{"id":"vc2-2c-4gb","vcpu_count":2,"ram":4096,"monthly_cost":20}],"meta":{"links":{"next":""}}}`
	defaultAvailBody = `{"available_plans":["vc2-2c-4gb","vc2-1c-2gb"]}`
)

func newVultrAPI(t *testing.T, r vultrAPIResponses) *httptest.Server {
	t.Helper()
	write := func(w http.ResponseWriter, body, fallback string, code int) {
		if code != 0 && code != http.StatusOK {
			http.Error(w, "vultr api failure", code)
			return
		}
		if body == "" {
			body = fallback
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.requestCount != nil {
			*r.requestCount++
		}
		switch req.URL.Path {
		case "/v2/os":
			write(w, r.os, defaultOSBody, r.osStatus)
		case "/v2/plans":
			write(w, r.plans, defaultPlansBody, r.plansStatus)
		case "/v2/regions/syd/availability":
			write(w, r.avail, defaultAvailBody, r.availStatus)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func validNodeClass(mutate func(*vultrv1.VultrNodeClass)) *vultrv1.VultrNodeClass {
	osID := 1743
	nc := &vultrv1.VultrNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: vultrv1.VultrNodeClassSpec{
			Region:            "syd",
			OSID:              &osID,
			KubernetesVersion: "v1.35",
			ClusterEndpoint:   "https://kubernetes.example.com:6443",
		},
	}
	if mutate != nil {
		mutate(nc)
	}
	return nc
}

func reconcileNodeClass(t *testing.T, nc *vultrv1.VultrNodeClass, api *httptest.Server) (client.Client, *vultrv1.VultrNodeClass, reconcile.Result, error) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := vultrv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(nc).
		WithStatusSubresource(&vultrv1.VultrNodeClass{}).
		Build()

	controller := NewNodeClassController(kubeClient, vultr.NewClientWithBaseURL("test", api.URL+"/v2", api.Client()))
	result, err := controller.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: nc.Name},
	})

	updated := &vultrv1.VultrNodeClass{}
	if getErr := kubeClient.Get(context.Background(), types.NamespacedName{Name: nc.Name}, updated); getErr != nil {
		t.Fatal(getErr)
	}
	return kubeClient, updated, result, err
}

func readyCondition(t *testing.T, nc *vultrv1.VultrNodeClass) status.Condition {
	t.Helper()
	cond := nc.StatusConditions().Get(status.ConditionReady)
	if cond == nil {
		t.Fatal("VultrNodeClass has no Ready condition")
	}
	return *cond
}

func TestNodeClassReconcileMarksValidNodeClassReady(t *testing.T) {
	api := newVultrAPI(t, vultrAPIResponses{})
	_, updated, result, err := reconcileNodeClass(t, validNodeClass(nil), api)
	if err != nil {
		t.Fatal(err)
	}

	if cond := readyCondition(t, updated); !cond.IsTrue() {
		t.Fatalf("Ready = %s (%s: %s), want True", cond.Status, cond.Reason, cond.Message)
	}
	// Resolved plans come from a map, so the controller must sort them or the
	// status churns on every reconcile.
	want := []string{"vc2-1c-2gb", "vc2-2c-4gb"}
	if !stringSlicesEqual(updated.Status.ResolvedPlanIDs, want) {
		t.Fatalf("ResolvedPlanIDs = %v, want %v", updated.Status.ResolvedPlanIDs, want)
	}
	if updated.Status.ObservedGeneration != updated.Generation {
		t.Fatalf("ObservedGeneration = %d, want %d", updated.Status.ObservedGeneration, updated.Generation)
	}
	// Regional availability changes without a spec edit, so a healthy NodeClass
	// still has to be re-resolved on a timer.
	if result.RequeueAfter != nodeClassRefreshInterval {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, nodeClassRefreshInterval)
	}
}

func TestNodeClassReconcileRejectsInvalidSpecs(t *testing.T) {
	osID := 1743
	tests := []struct {
		name   string
		mutate func(*vultrv1.VultrNodeClass)
		reason string
	}{
		{"no region", func(nc *vultrv1.VultrNodeClass) { nc.Spec.Region = "   " }, "InvalidRegion"},
		{"no image", func(nc *vultrv1.VultrNodeClass) { nc.Spec.OSID = nil }, "ImageNotConfigured"},
		{"two images", func(nc *vultrv1.VultrNodeClass) { nc.Spec.SnapshotID = "snap-1" }, "MultipleImagesConfigured"},
		{"no kubernetes version", func(nc *vultrv1.VultrNodeClass) { nc.Spec.KubernetesVersion = "" }, "KubernetesVersionMissing"},
		{"no endpoint", func(nc *vultrv1.VultrNodeClass) { nc.Spec.ClusterEndpoint = "" }, "ClusterEndpointMissing"},
		{"plaintext endpoint", func(nc *vultrv1.VultrNodeClass) { nc.Spec.ClusterEndpoint = "http://k8s.example.com:6443" }, "InvalidClusterEndpoint"},
		{"schemeless endpoint", func(nc *vultrv1.VultrNodeClass) { nc.Spec.ClusterEndpoint = "k8s.example.com:6443" }, "InvalidClusterEndpoint"},
		{"short ca hash", func(nc *vultrv1.VultrNodeClass) { nc.Spec.CACertHash = "sha256:abc" }, "InvalidCACertHash"},
		{"unprefixed ca hash", func(nc *vultrv1.VultrNodeClass) {
			nc.Spec.CACertHash = "0000000000000000000000000000000000000000000000000000000000000000"
		}, "InvalidCACertHash"},
		{"non-hex ca hash", func(nc *vultrv1.VultrNodeClass) {
			nc.Spec.CACertHash = "sha256:zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
		}, "InvalidCACertHash"},
		{"snapshot only is valid", func(nc *vultrv1.VultrNodeClass) {
			nc.Spec.OSID = nil
			nc.Spec.SnapshotID = "snap-1"
		}, ""},
		{"os and valid ca hash", func(nc *vultrv1.VultrNodeClass) {
			nc.Spec.OSID = &osID
			nc.Spec.CACertHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newVultrAPI(t, vultrAPIResponses{})
			_, updated, _, err := reconcileNodeClass(t, validNodeClass(tt.mutate), api)
			if err != nil {
				t.Fatal(err)
			}
			cond := readyCondition(t, updated)
			if tt.reason == "" {
				if !cond.IsTrue() {
					t.Fatalf("Ready = %s (%s: %s), want True", cond.Status, cond.Reason, cond.Message)
				}
				return
			}
			if cond.IsTrue() {
				t.Fatalf("expected Ready=False with reason %q, got True", tt.reason)
			}
			if cond.Reason != tt.reason {
				t.Fatalf("Ready reason = %q (%s), want %q", cond.Reason, cond.Message, tt.reason)
			}
		})
	}
}

func TestNodeClassReconcileValidatesOSImage(t *testing.T) {
	tests := []struct {
		name     string
		os       string
		osStatus int
		reason   string
		requeue  bool
	}{
		{
			name:   "unknown os id",
			os:     `{"os":[{"id":9999,"name":"Other","arch":"x64"}],"meta":{"links":{"next":""}}}`,
			reason: "InvalidOS",
		},
		{
			name:   "arm64 image",
			os:     `{"os":[{"id":1743,"name":"Ubuntu arm64","arch":"arm64"}],"meta":{"links":{"next":""}}}`,
			reason: "UnsupportedOSArchitecture",
		},
		{
			name: "x86_64 alias is accepted",
			os:   `{"os":[{"id":1743,"name":"Ubuntu","arch":"x86_64"}],"meta":{"links":{"next":""}}}`,
		},
		{
			// Regression: Vultr reports "x64", not "amd64"/"x86_64". Rejecting
			// it marks every osID-backed NodeClass unusable and no node is ever
			// provisioned.
			name: "vultr reports x64",
			os:   `{"os":[{"id":1743,"name":"Ubuntu 24.04 LTS x64","arch":"x64","family":"ubuntu"}],"meta":{"links":{"next":""}}}`,
		},
		{
			name:   "32-bit image",
			os:     `{"os":[{"id":1743,"name":"Debian i386","arch":"i386"}],"meta":{"links":{"next":""}}}`,
			reason: "UnsupportedOSArchitecture",
		},
		{
			name:     "lookup failure requeues",
			osStatus: http.StatusServiceUnavailable,
			reason:   "OSLookupFailed",
			requeue:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newVultrAPI(t, vultrAPIResponses{os: tt.os, osStatus: tt.osStatus})
			_, updated, result, err := reconcileNodeClass(t, validNodeClass(nil), api)
			if err != nil {
				t.Fatal(err)
			}
			cond := readyCondition(t, updated)
			if tt.reason == "" {
				if !cond.IsTrue() {
					t.Fatalf("Ready = %s (%s: %s), want True", cond.Status, cond.Reason, cond.Message)
				}
				return
			}
			if cond.Reason != tt.reason {
				t.Fatalf("Ready reason = %q (%s), want %q", cond.Reason, cond.Message, tt.reason)
			}
			// A transient API failure must retry sooner than the normal refresh.
			if tt.requeue && result.RequeueAfter != 30*time.Second {
				t.Fatalf("RequeueAfter = %s, want 30s", result.RequeueAfter)
			}
		})
	}
}

// A snapshot-backed NodeClass has no OS ID to validate, so /v2/os must not be
// consulted at all.
func TestNodeClassReconcileSkipsOSLookupForSnapshots(t *testing.T) {
	api := newVultrAPI(t, vultrAPIResponses{osStatus: http.StatusInternalServerError})
	_, updated, _, err := reconcileNodeClass(t, validNodeClass(func(nc *vultrv1.VultrNodeClass) {
		nc.Spec.OSID = nil
		nc.Spec.SnapshotID = "snap-1"
	}), api)
	if err != nil {
		t.Fatal(err)
	}
	if cond := readyCondition(t, updated); !cond.IsTrue() {
		t.Fatalf("Ready = %s (%s: %s), want True", cond.Status, cond.Reason, cond.Message)
	}
}

func TestNodeClassReconcileValidatesRegionAndPlan(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*vultrv1.VultrNodeClass)
		responses   vultrAPIResponses
		reason      string
		requeue     bool
		wantPlanIDs []string
	}{
		{
			name:      "region with no capacity",
			responses: vultrAPIResponses{avail: `{"available_plans":[]}`},
			reason:    "NoPlansAvailable",
		},
		{
			name:      "unknown region",
			responses: vultrAPIResponses{availStatus: http.StatusNotFound},
			reason:    "VultrAPIUnavailable",
			requeue:   true,
		},
		{
			name:      "availability lookup failure",
			responses: vultrAPIResponses{availStatus: http.StatusServiceUnavailable},
			reason:    "VultrAPIUnavailable",
			requeue:   true,
		},
		{
			name:      "fixed plan missing from the catalogue",
			mutate:    func(nc *vultrv1.VultrNodeClass) { nc.Spec.Plan = "vc2-99c-99gb" },
			responses: vultrAPIResponses{},
			reason:    "InvalidPlan",
		},
		{
			name:   "fixed plan withdrawn from the region",
			mutate: func(nc *vultrv1.VultrNodeClass) { nc.Spec.Plan = "vc2-2c-4gb" },
			// The plan exists in the catalogue but not in this region.
			responses: vultrAPIResponses{avail: `{"available_plans":["vc2-1c-2gb"]}`},
			reason:    "PlanUnavailable",
		},
		{
			name:        "fixed plan available",
			mutate:      func(nc *vultrv1.VultrNodeClass) { nc.Spec.Plan = "vc2-2c-4gb" },
			responses:   vultrAPIResponses{},
			wantPlanIDs: []string{"vc2-1c-2gb", "vc2-2c-4gb"},
		},
		{
			name:      "plan catalogue lookup failure",
			mutate:    func(nc *vultrv1.VultrNodeClass) { nc.Spec.Plan = "vc2-2c-4gb" },
			responses: vultrAPIResponses{plansStatus: http.StatusServiceUnavailable},
			reason:    "PlanLookupFailed",
			requeue:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newVultrAPI(t, tt.responses)
			_, updated, result, err := reconcileNodeClass(t, validNodeClass(tt.mutate), api)
			if err != nil {
				t.Fatal(err)
			}
			cond := readyCondition(t, updated)
			if tt.reason == "" {
				if !cond.IsTrue() {
					t.Fatalf("Ready = %s (%s: %s), want True", cond.Status, cond.Reason, cond.Message)
				}
			} else if cond.Reason != tt.reason {
				t.Fatalf("Ready reason = %q (%s), want %q", cond.Reason, cond.Message, tt.reason)
			}
			if tt.requeue && result.RequeueAfter != 30*time.Second {
				t.Fatalf("RequeueAfter = %s, want 30s", result.RequeueAfter)
			}
			if tt.wantPlanIDs != nil && !stringSlicesEqual(updated.Status.ResolvedPlanIDs, tt.wantPlanIDs) {
				t.Fatalf("ResolvedPlanIDs = %v, want %v", updated.Status.ResolvedPlanIDs, tt.wantPlanIDs)
			}
		})
	}
}

// The controller re-reconciles on a timer, so it must not write status when
// nothing changed. Otherwise every refresh bumps resourceVersion and wakes every
// watcher of the NodeClass.
func TestNodeClassReconcileDoesNotWriteUnchangedStatus(t *testing.T) {
	api := newVultrAPI(t, vultrAPIResponses{})

	scheme := runtime.NewScheme()
	if err := vultrv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(validNodeClass(nil)).
		WithStatusSubresource(&vultrv1.VultrNodeClass{}).
		Build()

	controller := NewNodeClassController(kubeClient, vultr.NewClientWithBaseURL("test", api.URL+"/v2", api.Client()))
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "default"}}

	if _, err := controller.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	first := &vultrv1.VultrNodeClass{}
	if err := kubeClient.Get(context.Background(), req.NamespacedName, first); err != nil {
		t.Fatal(err)
	}

	if _, err := controller.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	second := &vultrv1.VultrNodeClass{}
	if err := kubeClient.Get(context.Background(), req.NamespacedName, second); err != nil {
		t.Fatal(err)
	}

	if first.ResourceVersion != second.ResourceVersion {
		t.Fatalf("second reconcile rewrote unchanged status (resourceVersion %s -> %s)", first.ResourceVersion, second.ResourceVersion)
	}
}

func TestNodeClassReconcileIgnoresDeletedNodeClass(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := vultrv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	api := newVultrAPI(t, vultrAPIResponses{})
	controller := NewNodeClassController(kubeClient, vultr.NewClientWithBaseURL("test", api.URL+"/v2", api.Client()))

	result, err := controller.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "gone"},
	})
	if err != nil {
		t.Fatalf("reconciling a deleted VultrNodeClass returned %v, want nil", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %s, want 0", result.RequeueAfter)
	}
}

func TestValidEndpoint(t *testing.T) {
	valid := []string{"https://k8s.example.com:6443", "https://10.0.0.1:6443", "https://k8s.example.com"}
	invalid := []string{"", "k8s.example.com:6443", "http://k8s.example.com:6443", "https://", "://nope"}
	for _, value := range valid {
		if !validEndpoint(value) {
			t.Fatalf("validEndpoint(%q) = false, want true", value)
		}
	}
	for _, value := range invalid {
		if validEndpoint(value) {
			t.Fatalf("validEndpoint(%q) = true, want false", value)
		}
	}
}

func TestIsAMD64(t *testing.T) {
	// "x64" is the spelling Vultr's /v2/os actually returns for 64-bit x86
	// images; the rest are accepted defensively.
	for _, arch := range []string{"x64", "X64", "amd64", "x86_64", "x86-64", "X86_64", "AMD64"} {
		if !isAMD64(arch) {
			t.Fatalf("isAMD64(%q) = false, want true", arch)
		}
	}
	for _, arch := range []string{"arm64", "aarch64", "i386", ""} {
		if isAMD64(arch) {
			t.Fatalf("isAMD64(%q) = true, want false", arch)
		}
	}
}
