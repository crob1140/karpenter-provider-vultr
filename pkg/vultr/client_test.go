package vultr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateInstanceRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/instances" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Fatalf("missing auth header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"instance":{"id":"abc","region":"syd","plan":"vc2-1c-2gb"}}`))
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("test", srv.URL+"/v2", srv.Client())
	got, err := c.CreateInstance(context.Background(), CreateInstanceRequest{Region: "syd", Plan: "vc2-1c-2gb", OSID: func() *int { v := 1743; return &v }()})
	if err != nil || got.ID != "abc" {
		t.Fatalf("got %#v, err %v", got, err)
	}
}
