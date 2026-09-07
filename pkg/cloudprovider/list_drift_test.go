package cloudprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	vultrv1 "github.com/crob1140/karpenter-provider-vultr/pkg/apis/v1alpha1"
	"github.com/crob1140/karpenter-provider-vultr/pkg/vultr"
)

func instancesAPI(t *testing.T, instances []vultr.Instance) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/instances":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"instances": instances,
				"meta":      map[string]any{"links": map[string]string{"next": ""}},
			})
		case "/v2/plans":
			_, _ = w.Write([]byte(`{"plans":[{"id":"vc2-1c-2gb","vcpu_count":1,"ram":2048,"disk":50,"monthly_cost":10}],"meta":{"links":{"next":""}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func providerIDs(claims []*karpv1.NodeClaim) []string {
	out := make([]string, 0, len(claims))
	for _, c := range claims {
		out = append(out, c.Status.ProviderID)
	}
	sort.Strings(out)
	return out
}

// List is what Karpenter's garbage collector compares against the NodeClaims in
// the cluster: anything it omits is a candidate for deletion, and anything it
// wrongly includes belongs to somebody else.
func TestListReturnsOnlyThisClustersInstances(t *testing.T) {
	server := instancesAPI(t, []vultr.Instance{
		{ID: "ours", Plan: "vc2-1c-2gb", Region: "syd", Tags: vultr.InstanceTags("prod", "default-a", "default", "vultr-default")},
		{ID: "other-cluster", Plan: "vc2-1c-2gb", Region: "syd", Tags: vultr.InstanceTags("staging", "default-b", "default", "vultr-default")},
		{ID: "unmanaged", Plan: "vc2-1c-2gb", Region: "syd", Tags: []string{"someone-elses-tag"}},
		{ID: "no-nodeclaim-tag", Plan: "vc2-1c-2gb", Region: "syd", Tags: []string{"karpenter-cluster=prod"}},
	})

	provider := New(nil, vultr.NewClientWithBaseURL("test", server.URL+"/v2", server.Client()), "prod")
	claims, err := provider.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"vultr://ours"}
	if got := providerIDs(claims); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("List() = %v, want %v", got, want)
	}
}

func TestListHydratesNodeClaimLabelsAndCapacity(t *testing.T) {
	server := instancesAPI(t, []vultr.Instance{
		{ID: "ours", Plan: "vc2-1c-2gb", Region: "syd", OSID: 1743, Tags: vultr.InstanceTags("prod", "default-a", "default", "vultr-default")},
	})

	provider := New(nil, vultr.NewClientWithBaseURL("test", server.URL+"/v2", server.Client()), "prod")
	claims, err := provider.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("List() returned %d claims, want 1", len(claims))
	}
	claim := claims[0]

	for key, want := range map[string]string{
		corev1.LabelInstanceTypeStable: "vc2-1c-2gb",
		corev1.LabelTopologyRegion:     "syd",
		// Vultr has no zone dimension, so the region doubles as the synthetic
		// zone. This has to match the offering requirements or Karpenter sees a
		// NodeClaim that no offering satisfies.
		corev1.LabelTopologyZone:    "syd",
		corev1.LabelArchStable:      "amd64",
		corev1.LabelOSStable:        "linux",
		karpv1.CapacityTypeLabelKey: karpv1.CapacityTypeOnDemand,
	} {
		if got := claim.Labels[key]; got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}

	if claim.Status.ImageID != "1743" {
		t.Errorf("ImageID = %q, want %q", claim.Status.ImageID, "1743")
	}
	cpu := claim.Status.Capacity[corev1.ResourceCPU]
	if cpu.Value() != 1 {
		t.Errorf("cpu capacity = %d, want 1", cpu.Value())
	}
	memory := claim.Status.Capacity[corev1.ResourceMemory]
	if memory.Value() != 2048*1024*1024 {
		t.Errorf("memory capacity = %d, want %d", memory.Value(), 2048*1024*1024)
	}
	if len(claim.Status.Allocatable) == 0 {
		t.Error("allocatable was not populated from the resolved plan")
	}
}

// An instance whose plan has vanished from the catalogue must still be listed,
// otherwise Karpenter garbage-collects a live node.
func TestListIncludesInstancesWithAnUnknownPlan(t *testing.T) {
	server := instancesAPI(t, []vultr.Instance{
		{ID: "ours", Plan: "vc2-retired", Region: "syd", Tags: vultr.InstanceTags("prod", "default-a", "default", "vultr-default")},
	})

	provider := New(nil, vultr.NewClientWithBaseURL("test", server.URL+"/v2", server.Client()), "prod")
	claims, err := provider.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Status.ProviderID != "vultr://ours" {
		t.Fatalf("List() = %v, want the instance to be listed anyway", providerIDs(claims))
	}
	if len(claims[0].Status.Capacity) != 0 {
		t.Errorf("capacity should be empty for an unresolvable plan, got %v", claims[0].Status.Capacity)
	}
}

func TestListPropagatesAPIErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	provider := New(nil, vultr.NewClientWithBaseURL("test", server.URL+"/v2", server.Client()), "prod")
	if _, err := provider.List(context.Background()); err == nil {
		t.Fatal("expected a listing error, got nil")
	}
}

func driftTestProvider(t *testing.T, nodeClass *vultrv1.VultrNodeClass) *CloudProvider {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := vultrv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if nodeClass != nil {
		builder = builder.WithObjects(nodeClass)
	}
	return New(builder.Build(), nil, "prod")
}

func driftNodeClass() *vultrv1.VultrNodeClass {
	osID := 1743
	return &vultrv1.VultrNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: vultrv1.VultrNodeClassSpec{
			Region:            "syd",
			OSID:              &osID,
			KubernetesVersion: "v1.35",
			ClusterEndpoint:   "https://kubernetes.example.com:6443",
		},
	}
}

func driftNodeClaim(hash string) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "default-abc12"},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: vultrv1.GroupVersion.Group,
				Kind:  "VultrNodeClass",
				Name:  "default",
			},
		},
	}
	if hash != "" {
		nc.Annotations = map[string]string{
			vultrv1.NodeClassHashAnnotation:        hash,
			vultrv1.NodeClassHashVersionAnnotation: vultrv1.NodeClassHashVersion,
		}
	}
	return nc
}

func TestIsDriftedMatchesCurrentNodeClassHash(t *testing.T) {
	nodeClass := driftNodeClass()
	provider := driftTestProvider(t, nodeClass)

	reason, err := provider.IsDrifted(context.Background(), driftNodeClaim(nodeClass.Hash()))
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Fatalf("IsDrifted() = %q, want no drift", reason)
	}
}

func TestIsDriftedDetectsSpecChanges(t *testing.T) {
	original := driftNodeClass()
	originalHash := original.Hash()

	for _, tt := range []struct {
		name   string
		mutate func(*vultrv1.VultrNodeClass)
	}{
		{"region", func(nc *vultrv1.VultrNodeClass) { nc.Spec.Region = "ewr" }},
		{"os id", func(nc *vultrv1.VultrNodeClass) { id := 2136; nc.Spec.OSID = &id }},
		{"kubernetes version", func(nc *vultrv1.VultrNodeClass) { nc.Spec.KubernetesVersion = "v1.34" }},
		{"cluster endpoint", func(nc *vultrv1.VultrNodeClass) { nc.Spec.ClusterEndpoint = "https://other.example.com:6443" }},
		{"plan", func(nc *vultrv1.VultrNodeClass) { nc.Spec.Plan = "vc2-2c-4gb" }},
		{"extra user data", func(nc *vultrv1.VultrNodeClass) { nc.Spec.ExtraUserData = "#!/bin/sh\necho hi\n" }},
		{"ssh keys", func(nc *vultrv1.VultrNodeClass) { nc.Spec.SSHKeyIDs = []string{"key-1"} }},
		{"vpcs", func(nc *vultrv1.VultrNodeClass) { nc.Spec.VPCIDs = []string{"vpc-1"} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed := driftNodeClass()
			tt.mutate(changed)
			if changed.Hash() == originalHash {
				t.Fatalf("changing %s did not change the NodeClass hash, so drift can never be detected", tt.name)
			}

			provider := driftTestProvider(t, changed)
			reason, err := provider.IsDrifted(context.Background(), driftNodeClaim(originalHash))
			if err != nil {
				t.Fatal(err)
			}
			if reason != "VultrNodeClassDrifted" {
				t.Fatalf("IsDrifted() = %q, want VultrNodeClassDrifted", reason)
			}
		})
	}
}

// A NodeClaim with no recorded hash cannot be compared against anything.
// Absence of evidence is not drift: replacing a healthy node because an
// annotation is missing is a worse failure than missing a drift, and the
// NodeClass controller re-stamps these so drift resumes on the next pass.
func TestIsDriftedIgnoresNodeClaimWithNoRecordedHash(t *testing.T) {
	provider := driftTestProvider(t, driftNodeClass())

	reason, err := provider.IsDrifted(context.Background(), driftNodeClaim(""))
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Fatalf("IsDrifted() = %q, want no drift for an unhashed NodeClaim", reason)
	}
}

// The whole point of the hash version: a NodeClaim hashed under an older scheme
// must not be reported as drifted, or upgrading the provider would replace
// every node in the cluster at once.
func TestIsDriftedIgnoresStaleHashVersion(t *testing.T) {
	nodeClass := driftNodeClass()
	provider := driftTestProvider(t, nodeClass)

	claim := driftNodeClaim("a-hash-from-the-old-scheme")
	claim.Annotations[vultrv1.NodeClassHashVersionAnnotation] = "v0"

	reason, err := provider.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Fatalf("IsDrifted() = %q, want no drift across a hash-version change", reason)
	}
}

func TestIsDriftedRequiresAHashVersion(t *testing.T) {
	provider := driftTestProvider(t, driftNodeClass())

	claim := driftNodeClaim("some-hash")
	delete(claim.Annotations, vultrv1.NodeClassHashVersionAnnotation)

	reason, err := provider.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Fatalf("IsDrifted() = %q, want no drift when the hash version is unknown", reason)
	}
}

// A deleted NodeClass must not be reported as drift, or Karpenter would churn
// every node while the NodeClass is being replaced.
func TestIsDriftedIgnoresMissingNodeClass(t *testing.T) {
	provider := driftTestProvider(t, nil)

	reason, err := provider.IsDrifted(context.Background(), driftNodeClaim("12345"))
	if err != nil {
		t.Fatalf("a missing NodeClass should not error, got %v", err)
	}
	if reason != "" {
		t.Fatalf("IsDrifted() = %q, want no drift", reason)
	}
}

func TestIsDriftedRejectsForeignNodeClassRef(t *testing.T) {
	provider := driftTestProvider(t, driftNodeClass())

	claim := driftNodeClaim("12345")
	claim.Spec.NodeClassRef.Group = "karpenter.k8s.aws"
	claim.Spec.NodeClassRef.Kind = "EC2NodeClass"

	if _, err := provider.IsDrifted(context.Background(), claim); err == nil {
		t.Fatal("expected a NodeClassRef from another provider to be rejected")
	}
}
