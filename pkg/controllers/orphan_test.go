package controllers

import (
	"testing"

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
