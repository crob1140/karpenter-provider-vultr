# karpenter-provider-vultr

A Karpenter v1.12 provider for Vultr Compute.

> **Current bootstrap model:** the provider provisions Linux worker nodes from a Vultr Ubuntu image and bootstraps them into a **kubeadm-managed** Kubernetes cluster using Cloud-Init, a short-lived Kubernetes bootstrap token, and CA-pinned `kubeadm join`.

## Bootstrap architecture

```text
Karpenter
   |
   | NodeClaim
   v
Vultr CloudProvider
   |
   +--> Kubernetes API
   |      |
   |      +--> kube-system/kube-root-ca.crt
   |      +--> bootstrap-token-<id> Secret
   |
   +--> Vultr API
          |
          +--> Compute instance
                 |
                 +--> Cloud-Init
                        |
                        +--> containerd
                        +--> kubelet + kubeadm
                        +--> kubeadm join
                               |
                               v
                         Kubernetes Node
                               |
                               v
                         Vultr CCM
```

The provider does **not** need to know the Vultr instance ID before creating the VM. Vultr only returns the instance ID after the create request, so the bootstrap script starts kubelet with `--cloud-provider=external`; the Vultr Cloud Controller Manager then supplies the node's provider ID and cloud metadata.

Vultr documents Cloud-Init as the supported mechanism for initialization and notes that the API accepts base64-encoded user data. This provider therefore base64-encodes generated Cloud-Init before sending it to the Vultr API.

## Requirements

- Karpenter v1.12.1
- Kubernetes 1.35 (the repository's current target)
- A **self-managed kubeadm cluster**
- Vultr Cloud Controller Manager configured for an external cloud provider
- Linux Vultr image; the examples use Ubuntu OS ID `1743`
- A Kubernetes API endpoint reachable from the Vultr worker subnet
- `kube-system/kube-root-ca.crt` available, unless `spec.caCertHash` is explicitly configured

Kubernetes 1.35's package repository is minor-version specific. The bootstrapper accepts `v1.35` and installs the current patch from the v1.35 repository. As of 5 September 2026, Kubernetes 1.35.8 is the latest released patch in that series.

## Why kubeadm bootstrap tokens?

Kubernetes bootstrap tokens are specifically designed for joining new nodes. They are stored as Secrets in `kube-system`, and expired tokens are removed by the TokenCleaner controller.

This provider creates a bootstrap token with:

- `usage-bootstrap-authentication=true`
- `usage-bootstrap-signing=true`
- `auth-extra-groups=system:bootstrappers:kubeadm:default-node-token`
- a 12-hour lifetime
- automatic rotation when less than two hours remain

The token is placed in Vultr user data because `kubeadm join` needs it on the new host. The provider does not create a permanent kubeconfig or copy a cluster-admin credential to the node.

`kubeadm join` also uses `--discovery-token-ca-cert-hash`, so the API server CA is pinned during discovery.

## VultrNodeClass

Example:

```yaml
apiVersion: karpenter.vultr.com/v1alpha1
kind: VultrNodeClass
metadata:
  name: default
spec:
  region: syd
  osID: 1743

  # Same Kubernetes minor version as the control plane.
  kubernetesVersion: v1.35

  # Must be reachable from newly-created Vultr instances.
  clusterEndpoint: https://kubernetes.example.com:6443

  # Optional. By default this is calculated from
  # kube-system/kube-root-ca.crt.
  # caCertHash: sha256:<64-hex-character-SPKI-hash>

  sshKeyIDs: []
  vpcIDs: []
```

Optional post-bootstrap customization can be supplied as a shell script:

```yaml
spec:
  extraUserData: |
    #!/usr/bin/env bash
    set -euo pipefail
    # additional node configuration
```

`extraUserData` runs only after `kubeadm join` succeeds.

## RBAC

The provider's ServiceAccount needs:

- read/list access to the managed bootstrap token Secrets
- create access to bootstrap token Secrets
- read access to `kube-system/kube-root-ca.crt`
- the existing VultrNodeClass permissions

Apply:

```bash
kubectl apply -f config/rbac/serviceaccount.yaml
kubectl apply -f config/rbac/vultr-nodeclass-rbac.yaml
kubectl apply -f config/rbac/role.yaml
kubectl apply -f config/rbac/rolebinding.yaml
```

## Cluster prerequisites

The control plane must already be configured for an external cloud provider when using Vultr CCM. In particular, the kubelet on existing nodes and the control-plane components should use the external cloud-provider configuration expected by the Vultr CCM.

The Vultr CCM project documents that the cluster must be configured to use an external cloud provider. It provides node metadata such as hostname, region, plan ID and IP addresses, and manages Vultr LoadBalancers.

The CNI must also already be installed on the cluster. `kubeadm join` registers the node; the CNI DaemonSet then configures pod networking.

## Bootstrap sequence

For a new NodeClaim:

1. Karpenter selects a Vultr plan.
2. The provider resolves the VultrNodeClass.
3. The provider obtains the cluster CA hash.
4. The provider obtains or creates a short-lived kubeadm bootstrap token.
5. Cloud-Init is generated.
6. The Vultr API creates the instance with that Cloud-Init.
7. Cloud-Init installs/configures containerd.
8. Cloud-Init installs `kubeadm` and `kubelet` from the configured Kubernetes minor repository.
9. Swap is disabled and Kubernetes networking sysctls are configured.
10. `kubeadm join` performs CA-pinned discovery and TLS bootstrap.
11. The kubelet registers with `--cloud-provider=external`.
12. Vultr CCM initializes the node and supplies Vultr-specific node metadata/provider ID.
13. Karpenter observes the NodeClaim's node and continues normal lifecycle management.

## Important security consideration

Vultr user data is accessible through Vultr's instance user-data mechanisms. The bootstrap token therefore has to be treated as a credential exposed to the cloud provider.

The implementation mitigates this by using Kubernetes bootstrap tokens rather than a long-lived service-account or administrator credential, and by giving them a short lifetime. Do not replace this with a permanent kubeconfig.

For higher-security environments, the next logical enhancement is an out-of-band bootstrap mechanism that avoids placing the bearer token in cloud user data entirely.

## Files

- `pkg/bootstrap/bootstrap.go` — token management, CA hashing and Cloud-Init generation
- `pkg/cloudprovider/cloudprovider.go` — invokes bootstrap during `Create`
- `pkg/apis/v1alpha1/vultrnodeclass_types.go` — bootstrap configuration API
- `config/rbac/role.yaml` — permission to manage bootstrap tokens
- `config/rbac/rolebinding.yaml` — binds those permissions to the provider
- `config/samples/vultr-nodeclass.yaml` — example NodeClass

## NodeClaim lifecycle and failed bootstrap handling

Karpenter v1.12.1 already contains the authoritative NodeClaim launch and registration liveness controllers. The Vultr provider therefore does not duplicate those timers or delete instances based solely on a guessed kubeadm timeout. A normal failed bootstrap follows this path:

1. Vultr instance is created and receives Cloud-Init.
2. kubeadm/kubelet attempt to join the cluster.
3. If the Node never registers, Karpenter's NodeClaim registration/liveness logic eventually terminates the NodeClaim.
4. Karpenter's normal node termination path calls this provider's `Delete`.
5. `Delete` now translates a Vultr HTTP 404 into Karpenter's `NodeClaimNotFoundError`, allowing termination to complete cleanly.

There is also a provider-specific orphan controller. It scans only instances carrying the provider's `karpenter-nodeclaim=<name>` tag. If the corresponding NodeClaim no longer exists and the Vultr instance is older than 20 minutes, the controller deletes it. This protects the narrow failure window where the Vultr API successfully creates an instance but the NodeClaim cannot subsequently be persisted or observed. It deliberately does **not** delete instances whose NodeClaim still exists, including NodeClaims currently being terminated, so it does not race Karpenter's normal lifecycle controller.

The 20-minute orphan grace period is intentionally longer than the expected node registration window. It should be shortened only after measuring real bootstrap times in the target Vultr region/image.

The provider also keeps bootstrap failures out of the Vultr API credentials path: Cloud-Init never receives the Vultr API key. A kubeadm bootstrap token is the only cluster credential embedded in user-data, and it is short-lived.

## Current limitations

This bootstrap implementation intentionally targets kubeadm. It does not attempt to support k3s, RKE2, Talos, MicroK8s, or managed VKE worker registration.

The provider currently assumes Ubuntu/Debian-style package management for the bootstrap image. When `spec.osID` is set, the NodeClass controller validates that the image exists and is amd64. Snapshot-backed NodeClasses are supported as an image source, but the snapshot is expected to contain a compatible Linux userspace.

The Vultr instance ID cannot be inserted into user data before instance creation, so provider ID assignment is delegated to Vultr CCM rather than being supplied directly to kubelet.

## Production instance lifecycle

The Vultr API integration treats Karpenter's `CloudProvider` as the source of truth for
normal NodeClaim lifecycle operations. The provider now:

- follows Vultr cursor pagination for instance and plan lists (up to 500 objects/page)
- treats `/plans` as the plan catalogue, not as proof that a plan is deployable in every region
- queries `/regions/{region}/availability` for the regional set of currently deployable plans
- keeps every valid plan in Karpenter's instance-type universe while setting `Offering.Available` from the regional availability result
- caches the plan catalogue for 5 minutes and regional availability for 60 seconds
- falls back to the last successful discovery result during a transient refresh failure
- exposes Vultr's region as a single synthetic Karpenter zone, because Vultr's compute API does not expose a separate availability-zone dimension for these instances
- uses the NodePool's multi-plan instance-type requirement during launch selection rather than arbitrarily taking the first requirement value
- converts `monthly_cost` to an approximate hourly price using 730 hours/month, which is the price Karpenter uses for scheduling and consolidation comparisons
- translates Vultr HTTP 404s during deletion into Karpenter's `NodeClaimNotFoundError`
- preserves the orphan cleaner as a safety net for instances whose NodeClaim disappears entirely

This matches Karpenter's `CloudProvider.GetInstanceTypes` contract: instance types should remain present even when
individual offerings have no current capacity. Karpenter's scheduler filters `Available: false` offerings when it
simulates a launch, while the retained price information remains available for later reconciliation and
consolidation.

### Vultr availability model

Vultr exposes two complementary pieces of information:

1. `GET /plans` returns plan attributes such as vCPU, RAM, disk, bandwidth, monthly cost, type, and a `locations`
   list. The `locations` field describes where the plan is generally offered, but Vultr explicitly documents that
   not all plans are available in all regions.
2. `GET /regions/{region-id}/availability` returns `available_plans`, which is the region-specific set used by this
   provider to determine whether an offering is currently launchable.

The provider therefore never assumes that every `/plans` result is available in the configured region. A plan may
exist in the catalogue and still be returned to Karpenter with an unavailable offering. This is important because
Karpenter expects the instance-type universe to be stable while capacity availability changes over time.
