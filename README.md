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
- The `karpenter.sh` CRDs (`NodePool`, `NodeClaim`, `NodeOverlay`) installed from the matching Karpenter release — this binary embeds Karpenter's core controllers but does not ship their CRDs
- Vultr Cloud Controller Manager configured for an external cloud provider
- Linux Vultr image; the examples use Ubuntu OS ID `1743`
- A Kubernetes API endpoint reachable from the Vultr worker subnet
- `kube-system/kube-root-ca.crt` available, unless `spec.caCertHash` is explicitly configured

### Required configuration

| Environment variable | Purpose |
| --- | --- |
| `VULTR_API_KEY` | Vultr API key used for all compute operations. |
| `CLUSTER_NAME` | Scopes Vultr instance ownership to this Karpenter installation. |

`CLUSTER_NAME` is mandatory and the controller refuses to start without it.
Vultr instances carry no cluster identity of their own, so every instance this
provider creates is tagged `karpenter-cluster=<CLUSTER_NAME>`, and `List`, drift
reconciliation and orphan cleanup only ever consider instances carrying that
exact tag. **Two Karpenter installations sharing one Vultr account must use
different values**, otherwise each would treat the other's instances as its own
and delete them.

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

This binary embeds Karpenter's own core controllers, so its ServiceAccount needs
Karpenter's full core RBAC in addition to the provider-specific rules:

- Karpenter core: `nodepools`, `nodeclaims`, `nodeoverlays` (plus status and
  finalizers), `nodes`, `pods`, `pods/eviction`, PV/PVC and storage topology,
  workload owners (`apps`), `poddisruptionbudgets`, `events`
- leader-election `leases` in the controller's namespace
- read/list/watch and create access to the managed bootstrap token Secrets
- read access to `kube-system/kube-root-ca.crt`
- the VultrNodeClass permissions

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
10. `/etc/default/kubelet` is written with the node's `KUBELET_EXTRA_ARGS`.
11. `kubeadm join` performs CA-pinned discovery and TLS bootstrap.
12. The kubelet registers with `--cloud-provider=external` and Karpenter's
    `karpenter.sh/unregistered:NoExecute` startup taint.
13. Vultr CCM initializes the node and supplies Vultr-specific node metadata/provider ID.
14. Karpenter matches the Node to its NodeClaim by `spec.providerID`, syncs
    labels and taints onto it, and removes the startup taint.

`kubeadm join` has no flag for passing kubelet arguments, so `--cloud-provider`
and `--register-with-taints` are supplied through `/etc/default/kubelet`, which
the kubeadm systemd drop-in sources and appends last. A failed join is reset with
`kubeadm reset --force` before each retry, because a partially completed join
otherwise makes every subsequent attempt fail preflight rather than retrying the
original transient error.

## Important security consideration

Vultr user data is accessible through Vultr's instance user-data mechanisms. The bootstrap token therefore has to be treated as a credential exposed to the cloud provider.

The implementation mitigates this by using Kubernetes bootstrap tokens rather than a long-lived service-account or administrator credential, and by giving them a short lifetime. Do not replace this with a permanent kubeconfig.

For higher-security environments, the next logical enhancement is an out-of-band bootstrap mechanism that avoids placing the bearer token in cloud user data entirely.

## Support status

This checklist records the provider's **current implementation status**, rather than the full set of features exposed by Karpenter. A checked item means the provider implements the capability; it does not necessarily mean that the capability has completed real-cluster production validation.

> Items in **Operational readiness** are the exception: there a checked item means
> the automated coverage exists and passes, not merely that the behaviour is
> implemented.

### Core provisioning

- [x] Karpenter v1.12.1 CloudProvider integration
- [x] Karpenter core controllers embedded in this binary (provisioning, disruption,
      NodeClaim lifecycle, cluster state). Their CRDs are **not** shipped here and
      must be installed from the matching Karpenter release
- [x] Dynamic NodePool provisioning from pending Pods
- [x] VultrNodeClass resolution and readiness checks
- [x] Vultr plan selection from NodePool/NodeClaim requirements
- [x] Fixed plan selection via `VultrNodeClass.spec.plan`
- [x] Multi-plan NodePool scheduling and cheapest compatible available plan selection
- [x] Vultr regional plan availability filtering
- [x] On-demand capacity
- [x] AMD64 Linux nodes
- [x] Vultr region exposed as a synthetic Karpenter zone
- [x] CPU, memory, pod-count and ephemeral-storage capacity reporting
- [x] Approximate hourly pricing derived from Vultr monthly pricing

### Node lifecycle

- [x] Instance creation
- [x] NodeClaim provider ID generation and reconciliation
- [x] Provider `Get` and `List` operations
- [x] Instance deletion
- [x] Idempotent handling of already-deleted Vultr instances
- [x] Managed-instance identification using Karpenter tags
- [x] Per-cluster instance ownership scoping via `CLUSTER_NAME`
- [x] Orphan-instance cleanup for instances whose NodeClaim disappears
- [x] NodeClass drift detection
- [ ] VultrNodeClass deletion protection: a NodeClass can be deleted while live
      NodeClaims still reference it. Karpenter core ships no NodeClass controller,
      so the finalizer is the provider's responsibility and this one has none
- [ ] NodeClass hash versioning: `NodeClassHashVersion` is declared but unused,
      so changing the hash function would drift every existing node at once
- [ ] Handling for instances stuck in Vultr's intermediate teardown states —
      only an outright HTTP 404 is translated to "gone"
- [ ] Real-cluster end-to-end provisioning and termination validation
- [ ] Real-cluster failure/retry testing across bootstrap and Vultr API failures

### Bootstrap

- [x] Ubuntu/Debian-style Linux bootstrap
- [x] kubeadm-based cluster join
- [x] Short-lived Kubernetes bootstrap token
- [x] CA-pinned kubeadm discovery
- [x] Cloud-Init generation and Vultr user-data integration
- [x] containerd and kubelet installation/configuration
- [x] External cloud-provider mode for Vultr CCM
- [x] Optional additional post-join user data
- [ ] Non-kubeadm cluster bootstrap (k3s, RKE2, Talos, MicroK8s, etc.)
- [ ] Managed VKE worker registration
- [ ] Out-of-band bootstrap that avoids placing a bootstrap token in Vultr user data

### NodeClass configuration

- [x] Region
- [x] Vultr OS image
- [x] Snapshot-backed image source
- [x] Kubernetes minor version selection for bootstrap
- [x] Cluster API endpoint
- [x] CA hash discovery/configuration
- [x] SSH key IDs
- [x] VPC IDs
- [x] IPv6 enablement
- [x] Additional bootstrap user data
- [ ] Provider-managed kubelet/system-reservation configuration

### Capacity and scheduling

- [x] Vultr plan catalogue discovery
- [x] Region-specific capacity availability discovery
- [x] Plan and availability caching
- [x] Stale-cache fallback for transient discovery failures
- [x] Instance-type requirements
- [x] Region and synthetic-zone requirements
- [x] Capacity-type requirements for on-demand instances
- [x] Pod resource-fit scheduling
- [x] Unavailable offering filtering during scheduling
- [x] Regression tests for fixed and multi-plan scheduling paths
- [ ] Spot capacity
- [ ] GPU plans / extended GPU resources
- [ ] Reserved capacity
- [ ] Vultr-specific instance attributes beyond the standard Karpenter requirements

### Disruption and advanced lifecycle

- [x] Provider-side drift detection
- [x] Karpenter-compatible instance pricing for consolidation decisions
- [ ] Production validation of consolidation
- [ ] Production validation of drift replacement
- [ ] Vultr-specific interruption handling
- [ ] Provider repair policies / automatic node repair
- [ ] Provider-specific disruption reasons

### Operational readiness

- [x] Unit tests for the Vultr API client (pagination, create-request field names)
- [x] Unit tests for plan catalogue, regional availability, caching and pricing
- [x] Unit tests for bootstrap rendering, token management and CA hashing
- [x] Unit tests for plan selection, including deterministic tie-breaking when prices are equal
- [x] Unit tests for API error translation across `Create`, `Get` and `Delete`
- [x] Unit tests for instance ownership tagging and cluster scoping
- [x] Unit tests for the VultrNodeClass controller — spec validation, OS image and
      architecture checks, region/plan resolution, status conditions and requeue behaviour
- [x] Scheduler and consolidation regression tests
- [x] Kubernetes envtest lifecycle coverage using a fake Vultr API
- [x] Startup wiring test that both provider controllers register with the manager
- [x] Envtest suite wired to actually execute in CI
- [x] Standard Karpenter CloudProvider metrics (via `metrics.Decorate`)
- [ ] **A recorded green run of the envtest suite.** It has never executed: the
      checked-in kubebuilder assets are Linux-only and CI skipped it until now
- [ ] Unit tests for the orphan reconcile loop itself (only its tag helpers are covered)
- [ ] Unit tests for `List` and `IsDrifted`
- [ ] Real Vultr/Kubernetes integration test suite
- [ ] Automated end-to-end provisioning test against real Vultr capacity in CI
- [ ] Documented upgrade/compatibility policy for supported Kubernetes and Karpenter versions
- [ ] Production deployment/upgrade runbook
- [ ] Provider-specific Prometheus metrics and operational dashboards

> **Production-readiness note:** this project should not be considered production-ready solely because the automated tests pass. The remaining unchecked lifecycle and integration items are intentionally tracked here until they have been exercised against a real Vultr-backed Kubernetes cluster.

### Assumptions to verify on the first real deployment

These are behaviours the code depends on that could not be confirmed from
documentation or SDK source alone. Check them first when testing against a real
Vultr account:

- **`=` in Vultr tags.** Ownership metadata is encoded as `karpenter-cluster=<name>`.
  Vultr does not publish a tag character set, and `cluster-api-provider-vultr`
  uses `:` as its separator (`sigs-k8s-io:capvultr:<cluster>`). If Vultr rejects
  or rewrites `=`, instance creation fails or every instance becomes invisible to
  `List` and the orphan cleaner — switch the separator in `pkg/vultr/tags.go`.
- **containerd version.** The bootstrap installs Ubuntu's distribution
  `containerd` package. Confirm it is recent enough for the target Kubernetes
  minor; if not, install `containerd.io` from Docker's repository instead.
- **Ubuntu OS ID `1743`.** The examples assume this is a current amd64 Ubuntu
  image. The NodeClass controller validates existence and architecture, but the
  ID itself should be re-checked against `GET /v2/os`.
- **Plan universe.** `GET /v2/plans` returns every non-bare-metal plan family,
  including GPU plans. Nothing filters those out; cheapest-first selection means
  they are unlikely to be chosen, but a NodePool that explicitly requests one
  gets no GPU resources advertised.

## Files

- `pkg/bootstrap/bootstrap.go` — token management, CA hashing and Cloud-Init generation
- `pkg/cloudprovider/cloudprovider.go` — invokes bootstrap during `Create`
- `pkg/apis/v1alpha1/vultrnodeclass_types.go` — bootstrap configuration API
- `config/rbac/role.yaml` — permission to manage bootstrap tokens
- `config/rbac/rolebinding.yaml` — binds those permissions to the provider
- `config/samples/vultr-nodeclass.yaml` — example NodeClass
- `pkg/integration/cloudprovider_test.go` — opt-in envtest lifecycle test with a fake Vultr API
- `.github/workflows/test.yaml` — unit and envtest CI

## NodeClaim lifecycle and failed bootstrap handling

Karpenter v1.12.1 already contains the authoritative NodeClaim launch and registration liveness controllers. The Vultr provider therefore does not duplicate those timers or delete instances based solely on a guessed kubeadm timeout. A normal failed bootstrap follows this path:

1. Vultr instance is created and receives Cloud-Init.
2. kubeadm/kubelet attempt to join the cluster.
3. If the Node never registers, Karpenter's NodeClaim registration/liveness logic eventually terminates the NodeClaim.
4. Karpenter's normal node termination path calls this provider's `Delete`.
5. `Delete` now translates a Vultr HTTP 404 into Karpenter's `NodeClaimNotFoundError`, allowing termination to complete cleanly.

There is also a provider-specific orphan controller. It scans only instances carrying **both** the provider's `karpenter-cluster=<CLUSTER_NAME>` tag and a `karpenter-nodeclaim=<name>` tag. If the corresponding NodeClaim no longer exists and the Vultr instance is older than 20 minutes, the controller deletes it. This protects the narrow failure window where the Vultr API successfully creates an instance but the NodeClaim cannot subsequently be persisted or observed. It deliberately does **not** delete instances whose NodeClaim still exists, including NodeClaims currently being terminated, so it does not race Karpenter's normal lifecycle controller.

The 20-minute orphan grace period is intentionally longer than the expected node registration window. It should be shortened only after measuring real bootstrap times in the target Vultr region/image.

The provider also keeps bootstrap failures out of the Vultr API credentials path: Cloud-Init never receives the Vultr API key. A kubeadm bootstrap token is the only cluster credential embedded in user-data, and it is short-lived.

## Integration testing

The repository contains an opt-in Kubernetes `envtest` suite under `pkg/integration`. It starts a real Kubernetes API server and etcd, installs the provider CRD, uses the real `CloudProvider` implementation, and points the Vultr client at an in-process fake Vultr API. The current integration test exercises the most important launch lifecycle boundary:

1. resolve a `VultrNodeClass` from the Kubernetes API;
2. discover a compatible and region-available Vultr plan;
3. calculate the cluster CA hash;
4. create a short-lived bootstrap token Secret;
5. render and submit base64-encoded Cloud-Init to the Vultr API;
6. translate the returned Vultr instance into a Karpenter `NodeClaim`; and
7. delete the created Vultr instance through the provider lifecycle.

The suite does **not** start kubelet, kubeadm, Vultr CCM, or an actual Vultr VM. Real-cluster provisioning, bootstrap success, node registration, consolidation, drift replacement, and failure recovery therefore remain production-validation work.

Run the normal unit suite with:

```bash
go test ./...
```

Run the envtest suite with Kubernetes test assets installed:

```bash
make test-integration
```

The CI workflow installs the controller-runtime `setup-envtest` helper and runs this integration suite automatically. The real Vultr E2E suite remains intentionally separate because it requires cloud credentials, a reachable Kubernetes control plane, and paid/real infrastructure.

## Current limitations

This bootstrap implementation intentionally targets kubeadm. It does not attempt to support k3s, RKE2, Talos, MicroK8s, or managed VKE worker registration.

The provider currently assumes Ubuntu/Debian-style package management for the bootstrap image. When `spec.osID` is set, the NodeClass controller validates that the image exists and is amd64. Snapshot-backed NodeClasses are supported as an image source, but the snapshot is expected to contain a compatible Linux userspace.

The Vultr instance ID cannot be inserted into user data before instance creation, so provider ID assignment is delegated to Vultr CCM rather than being supplied directly to kubelet.

## Production instance lifecycle

The Vultr API integration treats Karpenter's `CloudProvider` as the source of truth for
normal NodeClaim lifecycle operations. The provider now:

- follows Vultr cursor pagination for instance, plan and OS lists (up to 500 objects/page). Vultr's `meta.links.next` is an **opaque cursor token**, not a URL, and is sent straight back as `?cursor=<token>`
- uses Vultr's documented create-instance field names: `sshkey_id` for SSH keys and `attach_vpc` for VPC attachment. Vultr ignores unknown fields, so the wrong names produce an instance with no keys and no VPC rather than an API error
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
