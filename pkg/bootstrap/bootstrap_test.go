package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRender(t *testing.T) {
	cfg := BootstrapConfig{
		KubernetesVersion: "v1.35",
		ClusterEndpoint:   "https://k8s.example.com:6443",
		CACertHash:        "sha256:" + strings.Repeat("a", 64),
		NodeName:          "nodeclaim-abc123",
		ExtraUserData:     "#!/bin/bash\necho hello\n",
	}

	got, err := Render(cfg, "abcdef.0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(got, "#cloud-config") {
		t.Fatal("rendered user-data is not Cloud-Init config")
	}
	for _, want := range []string{
		"v1.35",
		"k8s.example.com:6443",
		"abcdef.0123456789abcdef",
		"sha256:" + strings.Repeat("a", 64),
		"--cloud-provider=external",
		"--node-name",
		"containerd",
		"pkgs.k8s.io",
	} {
		if !strings.Contains(decodeRenderedScript(got), want) {
			t.Fatalf("rendered bootstrap script does not contain %q", want)
		}
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	tests := map[string]string{
		"https://k8s.example.com:6443": "k8s.example.com:6443",
		"k8s.example.com:6443":         "k8s.example.com:6443",
	}
	for input, want := range tests {
		got, err := normalizeEndpoint(input)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("normalizeEndpoint(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCACertHashFromConfigMap(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          new(big.Int).SetInt64(1),
		Subject:               pkix.Name{CommonName: "kubernetes"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(spki)
	want := "sha256:" + hex.EncodeToString(sum[:])

	got, err := CACertHashFromConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "kube-system"},
		Data:       map[string]string{"ca.crt": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("CA hash = %q, want %q", got, want)
	}
}

func TestTokenProviderCreatesBootstrapToken(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	provider := NewTokenProvider(fakeClient)
	provider.now = func() time.Time {
		return time.Date(2026, 9, 5, 5, 0, 0, 0, time.UTC)
	}

	token, err := provider.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !tokenRE.MatchString(token) {
		t.Fatalf("invalid token %q", token)
	}

	parts := strings.Split(token, ".")
	secret := &corev1.Secret{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{
		Name:      "bootstrap-token-" + parts[0],
		Namespace: BootstrapTokenNamespace,
	}, secret); err != nil {
		t.Fatal(err)
	}
	if secret.Type != bootstrapTokenSecretType {
		t.Fatalf("secret type = %q", secret.Type)
	}
	if string(secret.Data[bootstrapTokenUsageAuthKey]) != "true" {
		t.Fatal("authentication usage is not enabled")
	}
	if string(secret.Data[bootstrapTokenUsageSigningKey]) != "true" {
		t.Fatal("signing usage is not enabled")
	}
	if string(secret.Data[bootstrapTokenExtraGroupsKey]) != "system:bootstrappers:kubeadm:default-node-token" {
		t.Fatal("unexpected bootstrap group")
	}
}

func decodeRenderedScript(userData string) string {
	const marker = "    content: "
	idx := strings.Index(userData, marker)
	if idx < 0 {
		return ""
	}
	encoded := strings.TrimSpace(strings.SplitN(userData[idx+len(marker):], "\n", 2)[0])
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	return string(decoded)
}
