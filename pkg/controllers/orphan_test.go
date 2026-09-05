package controllers

import "testing"

func TestManagedNodeClaim(t *testing.T) {
	if got, ok := managedNodeClaim([]string{"foo", "karpenter-nodeclaim=abc123"}); !ok || got != "abc123" {
		t.Fatalf("got %q, %v", got, ok)
	}
	if _, ok := managedNodeClaim([]string{"karpenter-nodeclaim="}); ok {
		t.Fatal("expected empty nodeclaim tag to be rejected")
	}
}
