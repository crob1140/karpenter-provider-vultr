package vultr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const defaultBaseURL = "https://api.vultr.com/v2"

type Client struct {
	http            *http.Client
	baseURL, apiKey string
}

func NewClient(apiKey string) *Client {
	return &Client{http: &http.Client{}, baseURL: defaultBaseURL, apiKey: apiKey}
}
func NewClientWithBaseURL(apiKey, baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{http: httpClient, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey}
}

type Plan struct {
	ID          string  `json:"id"`
	VCPUCount   int     `json:"vcpu_count"`
	RAM         int     `json:"ram"`
	MonthlyCost float64 `json:"monthly_cost"`
}
type planResponse struct {
	Plans []Plan `json:"plans"`
}
type instanceResponse struct {
	Instance Instance `json:"instance"`
}
type instancesResponse struct {
	Instances []Instance `json:"instances"`
}

type Instance struct {
	ID           string   `json:"id"`
	Region       string   `json:"region"`
	Plan         string   `json:"plan"`
	OSID         int      `json:"os_id"`
	Label        string   `json:"label"`
	Hostname     string   `json:"hostname"`
	MainIP       string   `json:"main_ip"`
	InternalIP   string   `json:"internal_ip"`
	IPv6         string   `json:"v6_main_ip"`
	Status       string   `json:"status"`
	PowerStatus  string   `json:"power_status"`
	ServerStatus string   `json:"server_status"`
	DateCreated  string   `json:"date_created"`
	Tags         []string `json:"tags"`
}

type CreateInstanceRequest struct {
	Region, Plan                string
	OSID                        *int
	SnapshotID, Hostname, Label string
	SSHKeyIDs, VPCIDs           []string
	UserData                    string
	EnableIPv6                  bool
	Tags                        []string
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("Vultr API %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) ListPlans(ctx context.Context) ([]Plan, error) {
	var out planResponse
	if err := c.do(ctx, http.MethodGet, "/plans", nil, &out); err != nil {
		return nil, err
	}
	return out.Plans, nil
}
func (c *Client) GetInstance(ctx context.Context, id string) (*Instance, error) {
	var out instanceResponse
	if err := c.do(ctx, http.MethodGet, "/instances/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out.Instance, nil
}
func (c *Client) ListInstances(ctx context.Context) ([]Instance, error) {
	var out instancesResponse
	if err := c.do(ctx, http.MethodGet, "/instances", nil, &out); err != nil {
		return nil, err
	}
	return out.Instances, nil
}
func (c *Client) CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error) {
	payload := map[string]any{"region": req.Region, "plan": req.Plan, "hostname": req.Hostname, "label": req.Label, "sshkey_ids": req.SSHKeyIDs, "vpc_ids": req.VPCIDs, "user_data": req.UserData, "enable_ipv6": req.EnableIPv6, "tags": req.Tags}
	if req.OSID != nil {
		payload["os_id"] = *req.OSID
	}
	if req.SnapshotID != "" {
		payload["snapshot_id"] = req.SnapshotID
	}
	var out instanceResponse
	if err := c.do(ctx, http.MethodPost, "/instances", payload, &out); err != nil {
		return nil, err
	}
	return &out.Instance, nil
}
func (c *Client) DeleteInstance(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/instances/"+id, nil, nil)
}
