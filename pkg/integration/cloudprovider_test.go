package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	sigsyaml "sigs.k8s.io/yaml"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloud "sigs.k8s.io/karpenter/pkg/cloudprovider"

	vultrv1 "github.com/crob1140/karpenter-provider-vultr/pkg/apis/v1alpha1"
	vultrprovider "github.com/crob1140/karpenter-provider-vultr/pkg/cloudprovider"
	"github.com/crob1140/karpenter-provider-vultr/pkg/vultr"
)

// These tests deliberately do not run as part of the normal unit-test suite.
// They start a real Kubernetes API server/etcd pair and are intended to be run
// by `make test-integration` or CI with KUBEBUILDER_ASSETS configured.
func TestCloudProviderCreateAndDelete(t *testing.T) {
	if os.Getenv("VULTR_INTEGRATION_TESTS") != "1" {
		t.Skip("set VULTR_INTEGRATION_TESTS=1 to run envtest integration tests")
	}

	scheme := k8sruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	// Karpenter v1.12.1 does not expose SchemeBuilder from pkg/apis/v1.
	// Register the v1 API types explicitly.
	//
	// Karpenter's stable API group/version is karpenter.sh/v1.
	karpenterGroupVersion := schema.GroupVersion{
		Group:   "karpenter.sh",
		Version: "v1",
	}

	scheme.AddKnownTypes(
		karpenterGroupVersion,
		&karpv1.NodePool{},
		&karpv1.NodePoolList{},
		&karpv1.NodeClaim{},
		&karpv1.NodeClaimList{},
	)
	metav1.AddToGroupVersion(scheme, karpenterGroupVersion)

	if err := vultrv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{crdPath(t)},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf(
			"starting envtest: %v (set KUBEBUILDER_ASSETS to kube-apiserver/etcd assets)",
			err,
		)
	}

	defer func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	}()

	kubeClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	namespace := &corev1.Namespace{}
	if err := kubeClient.Get(
		context.Background(),
		client.ObjectKey{Name: "kube-system"},
		namespace,
	); apierrors.IsNotFound(err) {
		if err := kubeClient.Create(
			context.Background(),
			&corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: "kube-system"},
			},
		); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}

	caHash := createClusterCA(t, kubeClient)

	api := newFakeVultrAPI()
	defer api.Close()

	vultrClient := vultr.NewClientWithBaseURL(
		"integration-test-key",
		api.URL+"/v2",
		api.Client(),
	)

	provider := vultrprovider.New(kubeClient, vultrClient)

	nodeClass := &vultrv1.VultrNodeClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
		},
		Spec: vultrv1.VultrNodeClassSpec{
			Region:            "syd",
			OSID:              intPtr(1743),
			Plan:              "vc2-1c-2gb",
			KubernetesVersion: "v1.35",
			ClusterEndpoint:   "https://kubernetes.example.com:6443",
			CACertHash:        caHash,
			ExtraUserData:     "#!/bin/bash\necho integration-test\n",
		},
	}

	if err := kubeClient.Create(context.Background(), nodeClass); err != nil {
		t.Fatal(err)
	}

	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "integration-nodeclaim",
			Labels: map[string]string{
				karpv1.NodePoolLabelKey: "default",
			},
		},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: vultrv1.GroupVersion.Group,
				Kind:  "VultrNodeClass",
				Name:  nodeClass.Name,
			},
			Requirements: []karpv1.NodeSelectorRequirementWithMinValues{
				{
					Key:      corev1.LabelInstanceTypeStable,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"vc2-1c-2gb"},
				},
				{
					Key:      corev1.LabelArchStable,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"amd64"},
				},
				{
					Key:      corev1.LabelOSStable,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"linux"},
				},
			},
		},
	}

	created, err := provider.Create(context.Background(), nodeClaim)
	if err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	if created.Status.ProviderID != "vultr://instance-123" {
		t.Fatalf("unexpected provider ID %q", created.Status.ProviderID)
	}

	cpuCapacity := created.Status.Capacity[corev1.ResourceCPU]
	if cpuCapacity.Value() != 2 {
		t.Fatalf("unexpected CPU capacity: %v", cpuCapacity)
	}

	api.mu.Lock()
	createRequest := api.createRequest
	api.mu.Unlock()

	if createRequest.Authorization != "Bearer integration-test-key" {
		t.Fatalf(
			"unexpected authorization header %q",
			createRequest.Authorization,
		)
	}

	if createRequest.Region != "syd" ||
		createRequest.Plan != "vc2-1c-2gb" ||
		createRequest.OSID != 1743 {
		t.Fatalf("unexpected create request: %#v", createRequest)
	}

	decodedUserData, err := base64.StdEncoding.DecodeString(createRequest.UserData)
	if err != nil {
		t.Fatalf("decoding Vultr user data: %v", err)
	}

	bootstrapScript := extractBootstrapScript(t, decodedUserData)

	assertContains(t, bootstrapScript, "kubeadm join", "kubeadm join command")
	assertContains(
		t,
		bootstrapScript,
		`API_SERVER="kubernetes.example.com:6443"`,
		"API server configuration",
	)
	assertContains(
		t,
		bootstrapScript,
		fmt.Sprintf(`CA_CERT_HASH="%s"`, caHash),
		"cluster CA certificate hash",
	)
	assertContains(
		t,
		bootstrapScript,
		`NODE_NAME="integration-nodeclaim"`,
		"node name",
	)
	assertContains(
		t,
		bootstrapScript,
		`--discovery-token-ca-cert-hash "$CA_CERT_HASH"`,
		"kubeadm CA discovery argument",
	)
	assertContains(
		t,
		bootstrapScript,
		`--node-name "$NODE_NAME"`,
		"kubeadm node-name argument",
	)
	assertContains(
		t,
		bootstrapScript,
		`--cri-socket unix:///run/containerd/containerd.sock`,
		"containerd CRI socket",
	)

	// ExtraUserData is base64 encoded into the bootstrap script and executed
	// after the kubeadm join command. Verify that it survives all layers of
	// user-data encoding.
	extraUserDataB64 := base64.StdEncoding.EncodeToString(
		[]byte("#!/bin/bash\necho integration-test\n"),
	)
	assertContains(
		t,
		bootstrapScript,
		fmt.Sprintf(`EXTRA_USER_DATA_B64="%s"`, extraUserDataB64),
		"extra user data",
	)

	assertContains(
		t,
		bootstrapScript,
		`printf '%s' "$EXTRA_USER_DATA_B64" | base64 -d`,
		"extra user data decoding",
	)

	assertContains(
		t,
		string(decodedUserData),
		"#cloud-config",
		"cloud-config header",
	)

	secrets := &corev1.SecretList{}
	if err := kubeClient.List(
		context.Background(),
		secrets,
		client.InNamespace("kube-system"),
		client.MatchingLabels{
			"karpenter.vultr.com/bootstrap-token": "true",
		},
	); err != nil {
		t.Fatal(err)
	}

	if len(secrets.Items) != 1 {
		t.Fatalf(
			"expected one managed bootstrap token, got %d",
			len(secrets.Items),
		)
	}

	bootstrapSecret := secrets.Items[0]
	if len(bootstrapSecret.Data) == 0 {
		t.Fatal("managed bootstrap token secret contains no data")
	}

	var bootstrapToken string
	for key, value := range bootstrapSecret.Data {
		if len(value) == 0 {
			continue
		}

		// The provider may use a provider-specific key for the token.
		// For this integration test, the important invariant is that the
		// managed Secret contains a non-empty token value.
		if strings.Contains(strings.ToLower(key), "token") {
			bootstrapToken = string(value)
			break
		}
	}

	if bootstrapToken == "" {
		t.Fatalf(
			"managed bootstrap token secret does not contain a non-empty token field: keys=%v",
			secretDataKeys(bootstrapSecret.Data),
		)
	}

	if err := provider.Delete(context.Background(), created); err != nil {
		t.Fatalf("Delete() failed: %v", err)
	}

	api.mu.Lock()
	deleteID := api.deletedID
	api.mu.Unlock()

	if deleteID != "instance-123" {
		t.Fatalf(
			"expected instance-123 to be deleted, got %q",
			deleteID,
		)
	}
}

type cloudConfig struct {
	WriteFiles []cloudConfigWriteFile `json:"write_files"`
	RunCmd     [][]string             `json:"runcmd"`
}

type cloudConfigWriteFile struct {
	Path        string `json:"path"`
	Permissions string `json:"permissions"`
	Encoding    string `json:"encoding"`
	Content     string `json:"content"`
}

func extractBootstrapScript(t *testing.T, userData []byte) string {
	t.Helper()

	const bootstrapPath = "/usr/local/sbin/karpenter-vultr-bootstrap"

	var config cloudConfig
	if err := sigsyaml.Unmarshal(userData, &config); err != nil {
		t.Fatalf(
			"parsing generated cloud-config YAML: %v\nuser data:\n%s",
			err,
			userData,
		)
	}

	var bootstrapFile *cloudConfigWriteFile
	for i := range config.WriteFiles {
		if config.WriteFiles[i].Path == bootstrapPath {
			bootstrapFile = &config.WriteFiles[i]
			break
		}
	}

	if bootstrapFile == nil {
		t.Fatalf(
			"generated cloud-config does not contain %q\nuser data:\n%s",
			bootstrapPath,
			userData,
		)
	}

	if bootstrapFile.Encoding != "b64" {
		t.Fatalf(
			"bootstrap script has unexpected encoding %q, expected %q",
			bootstrapFile.Encoding,
			"b64",
		)
	}

	if bootstrapFile.Permissions != "0700" {
		t.Fatalf(
			"bootstrap script has unexpected permissions %q, expected %q",
			bootstrapFile.Permissions,
			"0700",
		)
	}

	bootstrapBytes, err := base64.StdEncoding.DecodeString(
		strings.TrimSpace(bootstrapFile.Content),
	)
	if err != nil {
		t.Fatalf(
			"decoding base64 bootstrap script from cloud-config: %v",
			err,
		)
	}

	bootstrapScript := string(bootstrapBytes)

	if len(bootstrapScript) == 0 {
		t.Fatal("decoded bootstrap script is empty")
	}

	foundBootstrapCommand := false
	for _, command := range config.RunCmd {
		if len(command) == 1 && command[0] == bootstrapPath {
			foundBootstrapCommand = true
			break
		}
	}

	if !foundBootstrapCommand {
		t.Fatalf(
			"cloud-config runcmd does not execute %q",
			bootstrapPath,
		)
	}

	return bootstrapScript
}

func assertContains(t *testing.T, value, expected, description string) {
	t.Helper()

	if !strings.Contains(value, expected) {
		t.Fatalf(
			"generated bootstrap script is missing %s %q:\n%s",
			description,
			expected,
			value,
		)
	}
}

func crdPath(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("unable to determine test source path")
	}

	return filepath.Join(
		filepath.Dir(file),
		"..",
		"..",
		"config",
		"crd",
	)
}

func secretDataKeys(data map[string][]byte) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	return keys
}

func createClusterCA(t *testing.T, c client.Client) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "integration-test-ca"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(
		rand.Reader,
		template,
		template,
		&key.PublicKey,
		key,
	)
	if err != nil {
		t.Fatal(err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kube-root-ca.crt",
			Namespace: "kube-system",
		},
		Data: map[string]string{
			"ca.crt": string(
				pem.EncodeToMemory(&pem.Block{
					Type:  "CERTIFICATE",
					Bytes: der,
				}),
			),
		},
	}

	if err := c.Create(context.Background(), cm); err != nil {
		t.Fatal(err)
	}

	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(spki)

	return fmt.Sprintf("sha256:%x", sum[:])
}

func intPtr(value int) *int {
	return &value
}

type fakeCreateRequest struct {
	Authorization string
	Region        string
	Plan          string
	OSID          int
	UserData      string
}

type fakeVultrAPI struct {
	*httptest.Server
	mu            sync.Mutex
	createRequest fakeCreateRequest
	deletedID     string
}

func newFakeVultrAPI() *fakeVultrAPI {
	api := &fakeVultrAPI{}
	api.Server = httptest.NewServer(http.HandlerFunc(api.handle))
	return api
}

func (a *fakeVultrAPI) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/plans":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plans": []any{
				map[string]any{
					"id":           "vc2-1c-2gb",
					"vcpu_count":   2,
					"ram":          2048,
					"disk":         50,
					"monthly_cost": 10,
				},
			},
			"meta": map[string]any{
				"links": map[string]string{
					"next": "",
				},
			},
		})

	case r.Method == http.MethodGet &&
		r.URL.Path == "/v2/regions/syd/availability":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"available_plans": []string{"vc2-1c-2gb"},
		})

	case r.Method == http.MethodPost &&
		r.URL.Path == "/v2/instances":
		var payload struct {
			Region   string `json:"region"`
			Plan     string `json:"plan"`
			OSID     int    `json:"os_id"`
			UserData string `json:"user_data"`
		}

		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		a.mu.Lock()
		a.createRequest = fakeCreateRequest{
			Authorization: r.Header.Get("Authorization"),
			Region:        payload.Region,
			Plan:          payload.Plan,
			OSID:          payload.OSID,
			UserData:      payload.UserData,
		}
		a.mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{
			"instance": map[string]any{
				"id":           "instance-123",
				"region":       payload.Region,
				"plan":         payload.Plan,
				"os_id":        payload.OSID,
				"date_created": time.Now().UTC().Format(time.RFC3339),
			},
		})

	case r.Method == http.MethodDelete &&
		r.URL.Path == "/v2/instances/instance-123":
		a.mu.Lock()
		a.deletedID = "instance-123"
		a.mu.Unlock()

		w.WriteHeader(http.StatusNoContent)

	default:
		http.NotFound(w, r)
	}
}

// Make sure the integration test exercises the real provider interface rather
// than an implementation detail that happens to compile against a different
// Karpenter version.
var _ karpcloud.CloudProvider = (*vultrprovider.CloudProvider)(nil)
