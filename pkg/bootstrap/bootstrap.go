package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	BootstrapTokenNamespace       = "kube-system"
	BootstrapTokenManagedLabel    = "karpenter.vultr.com/bootstrap-token"
	BootstrapTokenManagedValue    = "true"
	bootstrapTokenTTL             = 12 * time.Hour
	bootstrapTokenRefresh         = 2 * time.Hour
	bootstrapTokenSecretType      = corev1.SecretType("bootstrap.kubernetes.io/token")
	bootstrapTokenIDKey           = "token-id"
	bootstrapTokenSecretKey       = "token-secret"
	bootstrapTokenExpirationKey   = "expiration"
	bootstrapTokenUsageSigningKey = "usage-bootstrap-signing"
	bootstrapTokenUsageAuthKey    = "usage-bootstrap-authentication"
	bootstrapTokenExtraGroupsKey  = "auth-extra-groups"
	bootstrapTokenDescriptionKey  = "description"
)

var (
	tokenIDRE = regexp.MustCompile(`^[a-z0-9]{6}$`)
	tokenRE   = regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)
)

// BootstrapConfig describes the cluster-specific inputs needed to join a
// kubeadm-managed worker node.
type BootstrapConfig struct {
	KubernetesVersion string
	ClusterEndpoint   string
	CACertHash        string
	NodeName          string
	ExtraUserData     string
}

// TokenProvider manages short-lived kubeadm bootstrap tokens stored as
// bootstrap.kubernetes.io/token Secrets. Tokens are created with the standard
// kubeadm bootstrapper group and are allowed to expire naturally.
type TokenProvider struct {
	client client.Client
	now    func() time.Time
}

func NewTokenProvider(c client.Client) *TokenProvider {
	return &TokenProvider{client: c, now: time.Now}
}

// Token returns a current kubeadm bootstrap token, creating a fresh token when
// the newest managed token has less than two hours remaining.
func (p *TokenProvider) Token(ctx context.Context) (string, error) {
	list := &corev1.SecretList{}
	if err := p.client.List(ctx, list,
		client.InNamespace(BootstrapTokenNamespace),
		client.MatchingLabels(labels.Set{
			BootstrapTokenManagedLabel: BootstrapTokenManagedValue,
		}),
	); err != nil {
		return "", fmt.Errorf("listing managed bootstrap tokens: %w", err)
	}

	var best *corev1.Secret
	var bestExpiry time.Time
	for i := range list.Items {
		token, ok := bootstrapTokenFromSecret(&list.Items[i])
		if !ok {
			continue
		}
		expiry, err := time.Parse(time.RFC3339, string(list.Items[i].Data[bootstrapTokenExpirationKey]))
		if err != nil || !expiry.After(p.now().Add(bootstrapTokenRefresh)) {
			continue
		}
		if best == nil || expiry.After(bestExpiry) {
			best = &list.Items[i]
			bestExpiry = expiry
			_ = token
		}
	}

	if best != nil {
		token, _ := bootstrapTokenFromSecret(best)
		return token, nil
	}

	var createdToken string
	err := func() error {
		// Another controller replica may have created a valid token since the
		// initial list, so check again before creating another one.
		current := &corev1.SecretList{}
		if err := p.client.List(ctx, current,
			client.InNamespace(BootstrapTokenNamespace),
			client.MatchingLabels(labels.Set{
				BootstrapTokenManagedLabel: BootstrapTokenManagedValue,
			}),
		); err != nil {
			return err
		}
		for i := range current.Items {
			if token, ok := bootstrapTokenFromSecret(&current.Items[i]); ok {
				expiry, err := time.Parse(time.RFC3339, string(current.Items[i].Data[bootstrapTokenExpirationKey]))
				if err == nil && expiry.After(p.now().Add(bootstrapTokenRefresh)) {
					createdToken = token
					return nil
				}
			}
		}

		tokenID, tokenSecret, err := generateToken()
		if err != nil {
			return err
		}
		expiration := p.now().Add(bootstrapTokenTTL).UTC()
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				// Bootstrap tokens are required to use this name format so the
				// Kubernetes bootstrap controllers recognize them.
				Name:      "bootstrap-token-" + tokenID,
				Namespace: BootstrapTokenNamespace,
				Labels: map[string]string{
					BootstrapTokenManagedLabel: BootstrapTokenManagedValue,
				},
			},
			Type: bootstrapTokenSecretType,
			Data: map[string][]byte{
				bootstrapTokenIDKey:           []byte(tokenID),
				bootstrapTokenSecretKey:       []byte(tokenSecret),
				bootstrapTokenUsageSigningKey: []byte("true"),
				bootstrapTokenUsageAuthKey:    []byte("true"),
				bootstrapTokenExtraGroupsKey:  []byte("system:bootstrappers:kubeadm:default-node-token"),
				bootstrapTokenDescriptionKey:  []byte("Managed by karpenter-provider-vultr"),
				bootstrapTokenExpirationKey:   []byte(expiration.Format(time.RFC3339)),
			},
		}
		if err := p.client.Create(ctx, secret); err != nil {
			return err
		}
		createdToken = tokenID + "." + tokenSecret
		return nil
	}()
	if err != nil {
		return "", fmt.Errorf("creating bootstrap token: %w", err)
	}
	return createdToken, nil
}

func bootstrapTokenFromSecret(secret *corev1.Secret) (string, bool) {
	id := string(secret.Data[bootstrapTokenIDKey])
	secretPart := string(secret.Data[bootstrapTokenSecretKey])
	token := id + "." + secretPart
	return token, tokenIDRE.MatchString(id) && tokenRE.MatchString(token)
}

func generateToken() (string, string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	random := make([]byte, 22)
	if _, err := rand.Read(random); err != nil {
		return "", "", err
	}
	for i := range random {
		random[i] = alphabet[int(random[i])%len(alphabet)]
	}
	return string(random[:6]), string(random[6:]), nil
}

// Render generates Vultr Cloud-Init user-data for a kubeadm worker.
//
// The instance ID is intentionally not embedded: Vultr only returns the ID
// after CreateInstance completes, while user-data must be supplied as part of
// the create request. The Vultr CCM therefore remains responsible for
// assigning Node.spec.providerID.
func Render(cfg BootstrapConfig, token string) (string, error) {
	if err := validateConfig(cfg, token); err != nil {
		return "", err
	}
	minor, err := kubernetesMinor(cfg.KubernetesVersion)
	if err != nil {
		return "", err
	}
	endpoint, err := normalizeEndpoint(cfg.ClusterEndpoint)
	if err != nil {
		return "", err
	}

	script := fmt.Sprintf(`#!/usr/bin/env bash
set -Eeuo pipefail
exec > >(tee -a /var/log/karpenter-vultr-bootstrap.log) 2>&1

KUBERNETES_MINOR=%q
API_SERVER=%q
BOOTSTRAP_TOKEN=%q
CA_CERT_HASH=%q
NODE_NAME=%q
EXTRA_USER_DATA_B64=%q

export DEBIAN_FRONTEND=noninteractive

retry() {
  local attempts=0
  until "$@"; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 10 ]; then
      echo "command failed after ${attempts} attempts: $*" >&2
      return 1
    fi
    sleep $((attempts * 5))
  done
}

# kubeadm refuses to operate with active swap.
swapoff -a || true
sed -ri '/[[:space:]]swap[[:space:]]/ s/^/#/' /etc/fstab || true

cat >/etc/modules-load.d/k8s.conf <<'EOF'
overlay
br_netfilter
EOF
modprobe overlay
modprobe br_netfilter

cat >/etc/sysctl.d/99-kubernetes-k8s.conf <<'EOF'
net.ipv4.ip_forward = 1
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
EOF
sysctl --system

retry apt-get update
# conntrack, socat and ethtool are kubeadm preflight requirements and are not
# present on Vultr's minimal Ubuntu cloud images.
retry apt-get install -y ca-certificates curl gpg containerd conntrack socat ethtool

mkdir -p /etc/containerd
containerd config default >/etc/containerd/config.toml

# Configure systemd cgroups for both containerd 1.x and 2.x.
if grep -q 'io.containerd.cri.v1.runtime' /etc/containerd/config.toml; then
  sed -i '/^\[plugins.*io.containerd.cri.v1.runtime.*runtimes.runc.options\]/,/^\[/ s/^[[:space:]]*SystemdCgroup[[:space:]]*=.*/    SystemdCgroup = true/' /etc/containerd/config.toml
  if ! grep -q 'SystemdCgroup = true' /etc/containerd/config.toml; then
    printf '\n[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc.options]\nSystemdCgroup = true\n' >>/etc/containerd/config.toml
  fi
else
  sed -i '/^\[plugins.*io.containerd.grpc.v1.cri.*containerd.runtimes.runc.options\]/,/^\[/ s/^[[:space:]]*SystemdCgroup[[:space:]]*=.*/        SystemdCgroup = true/' /etc/containerd/config.toml
  if ! grep -q 'SystemdCgroup = true' /etc/containerd/config.toml; then
    printf '\n[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]\nSystemdCgroup = true\n' >>/etc/containerd/config.toml
  fi
fi

# Some distribution packages ship with CRI disabled; Kubernetes requires CRI v1.
sed -ri 's/^[[:space:]]*disabled_plugins[[:space:]]*=.*/disabled_plugins = []/' /etc/containerd/config.toml

systemctl enable --now containerd
systemctl restart containerd

mkdir -p -m 755 /etc/apt/keyrings
curl -fsSL "https://pkgs.k8s.io/core:/stable:/$KUBERNETES_MINOR/deb/Release.key" |
  gpg --dearmor --yes -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
cat >/etc/apt/sources.list.d/kubernetes.list <<EOF
deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/$KUBERNETES_MINOR/deb/ /
EOF

retry apt-get update
retry apt-get install -y kubelet kubeadm
apt-mark hold kubelet kubeadm
systemctl enable kubelet

# kubeadm join accepts no flag for passing kubelet arguments. The kubeadm
# systemd drop-in (/usr/lib/systemd/system/kubelet.service.d/10-kubeadm.conf) sources
# /etc/default/kubelet and appends $KUBELET_EXTRA_ARGS last, which is the
# supported way to add kubelet flags on a kubeadm-managed node.
#
# --cloud-provider=external hands node initialization to the Vultr CCM, which
# assigns spec.providerID. karpenter.sh/unregistered:NoExecute is Karpenter's
# startup taint: it keeps workloads off the node until Karpenter has finished
# syncing NodeClaim labels/taints onto it, and Karpenter removes it during
# registration.
cat >/etc/default/kubelet <<'EOF'
KUBELET_EXTRA_ARGS=--cloud-provider=external --register-with-taints=karpenter.sh/unregistered:NoExecute
EOF

# Token-based discovery is CA-pinned. kubeadm then uses the same short-lived
# bootstrap token for TLS bootstrap and obtains a permanent client certificate.
join_cluster() {
  kubeadm join "$API_SERVER" \
    --token "$BOOTSTRAP_TOKEN" \
    --discovery-token-ca-cert-hash "$CA_CERT_HASH" \
    --node-name "$NODE_NAME" \
    --cri-socket unix:///run/containerd/containerd.sock
}

# A partially completed join leaves /etc/kubernetes and /var/lib/kubelet
# populated, which makes every later attempt fail preflight instead of
# retrying the transient failure. Reset before each retry.
join_attempt=0
until join_cluster; do
  join_attempt=$((join_attempt + 1))
  if [ "$join_attempt" -ge 5 ]; then
    echo "kubeadm join failed after ${join_attempt} attempts" >&2
    exit 1
  fi
  kubeadm reset --force --cri-socket unix:///run/containerd/containerd.sock || true
  sleep $((join_attempt * 15))
done

if [ -n "$EXTRA_USER_DATA_B64" ]; then
  printf '%%s' "$EXTRA_USER_DATA_B64" | base64 -d >/run/karpenter-vultr-extra-bootstrap.sh
  chmod 0700 /run/karpenter-vultr-extra-bootstrap.sh
  /run/karpenter-vultr-extra-bootstrap.sh
  rm -f /run/karpenter-vultr-extra-bootstrap.sh
fi

rm -f /usr/local/sbin/karpenter-vultr-bootstrap
`, minor, endpoint, token, cfg.CACertHash, cfg.NodeName, base64.StdEncoding.EncodeToString([]byte(cfg.ExtraUserData)))

	return fmt.Sprintf(`#cloud-config
write_files:
  - path: /usr/local/sbin/karpenter-vultr-bootstrap
    permissions: "0700"
    encoding: b64
    content: %s
runcmd:
  - ["/usr/local/sbin/karpenter-vultr-bootstrap"]
`, base64.StdEncoding.EncodeToString([]byte(script))), nil
}

func validateConfig(cfg BootstrapConfig, token string) error {
	if cfg.KubernetesVersion == "" {
		return fmt.Errorf("kubernetesVersion is required")
	}
	if _, err := kubernetesMinor(cfg.KubernetesVersion); err != nil {
		return err
	}
	if _, err := normalizeEndpoint(cfg.ClusterEndpoint); err != nil {
		return err
	}
	if !strings.HasPrefix(cfg.CACertHash, "sha256:") || len(strings.TrimPrefix(cfg.CACertHash, "sha256:")) != 64 {
		return fmt.Errorf("caCertHash must be sha256:<64 hex characters>")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(cfg.CACertHash, "sha256:")); err != nil {
		return fmt.Errorf("caCertHash contains invalid hex: %w", err)
	}
	if cfg.NodeName == "" {
		return fmt.Errorf("node name is required")
	}
	if !tokenRE.MatchString(token) {
		return fmt.Errorf("invalid kubeadm bootstrap token")
	}
	return nil
}

func kubernetesMinor(version string) (string, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(version, ".")
	if len(parts) < 2 || parts[0] != "1" {
		return "", fmt.Errorf("kubernetesVersion must be a Kubernetes 1.x version, got %q", version)
	}
	if _, err := parsePositiveInt(parts[1]); err != nil {
		return "", fmt.Errorf("invalid kubernetes minor version %q", version)
	}
	return "v" + parts[0] + "." + parts[1], nil
}

func parsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty integer")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not an integer")
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return n, nil
}

func normalizeEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("clusterEndpoint is required")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("invalid clusterEndpoint %q", raw)
		}
		if u.Scheme != "https" {
			return "", fmt.Errorf("clusterEndpoint must use https when a scheme is provided")
		}
		raw = u.Host
	}
	raw = strings.TrimSuffix(raw, "/")
	if strings.ContainsAny(raw, " \t\r\n") {
		return "", fmt.Errorf("clusterEndpoint must not contain whitespace")
	}
	return raw, nil
}

// CACertHashFromConfigMap calculates the kubeadm discovery CA hash from a
// kube-root-ca.crt ConfigMap. kubeadm hashes the DER-encoded SubjectPublicKeyInfo.
func CACertHashFromConfigMap(cm *corev1.ConfigMap) (string, error) {
	pemData := []byte(cm.Data["ca.crt"])
	block, _ := pem.Decode(pemData)
	if block == nil {
		return "", fmt.Errorf("ConfigMap %s/%s does not contain a PEM ca.crt", cm.Namespace, cm.Name)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parsing cluster CA certificate: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshalling cluster CA public key: %w", err)
	}
	sum := sha256.Sum256(spki)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func ClusterCAHash(ctx context.Context, c client.Client) (string, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{
		Name:      "kube-root-ca.crt",
		Namespace: BootstrapTokenNamespace,
	}, cm); err != nil {
		return "", fmt.Errorf("reading kube-system/kube-root-ca.crt: %w", err)
	}
	return CACertHashFromConfigMap(cm)
}
