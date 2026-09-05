package cloudprovider

import "testing"

func TestParseProviderID(t *testing.T) {
	got, err := parseProviderID("vultr://abc123")
	if err != nil || got != "abc123" {
		t.Fatalf("got %q, err %v", got, err)
	}
	if _, err := parseProviderID("aws:///syd/abc123"); err == nil {
		t.Fatal("expected invalid provider ID")
	}
}
