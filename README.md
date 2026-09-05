# Karpenter Provider Vultr

A Karpenter CloudProvider implementation for Vultr.

> **Status:** repository skeleton / first implementation pass. The provider targets Karpenter v1.12.1 and Kubernetes v1.35 APIs. The Vultr API integration and bootstrap path are deliberately kept small so they can be validated against a real Vultr cluster before adding more features.

## Architecture

```text
Karpenter core
    |
    | CloudProvider interface
    v
karpenter-provider-vultr
    |-- VultrNodeClass controller
    |-- Vultr CloudProvider
    |     |-- instance provider
    |     |-- plan/instance-type provider
    |     `-- bootstrap/user-data provider
    `-- Vultr API client
             |
             v
         Vultr API

Vultr Cloud Controller Manager runs separately and owns Kubernetes cloud-provider integration such as node addresses/state and LoadBalancers.
```

Karpenter's v1.12.1 `CloudProvider` interface requires `Create`, `Delete`, `Get`, `List`, `GetInstanceTypes`, `IsDrifted`, `RepairPolicies`, `Name`, and `GetSupportedNodeClasses`. See `pkg/cloudprovider/types.go` in the upstream Karpenter source.

## Current scope

- `VultrNodeClass` CRD with region, OS, SSH keys, VPC IDs, and user-data.
- Vultr plan discovery translated into Karpenter `InstanceType` objects.
- Vultr instance create/get/list/delete operations.
- Karpenter provider IDs in the form `vultr://<instance-id>`.
- Karpenter core controller wiring.
- Separate NodeClass reconciliation with Ready condition.
- Helm chart and example NodePool/NodeClass.

## Deliberate next steps

1. Validate bootstrap against the exact Kubernetes distribution and cluster endpoint.
2. Add cloud-init generation for kubelet/container runtime registration.
3. Decide whether to use a Vultr VPC-only/private networking mode.
4. Add pricing/availability caching and plan filtering.
5. Add drift detection based on a NodeClass hash.
6. Add integration tests using a fake Vultr API and a Kubernetes envtest cluster.
7. Add optional reserved/fixed plans and GPU plans.

## Prerequisites

- Kubernetes 1.35.x
- Karpenter v1.12.1
- Go 1.26.x
- Vultr API key
- Vultr Cloud Controller Manager installed in the cluster

## Development

```bash
go mod tidy
go test ./...
go build ./cmd/controller
```

The development container used to create this skeleton has Go 1.23, while Karpenter v1.12.1 requires Go 1.26.3, so the commands above should be run with Go 1.26+.
