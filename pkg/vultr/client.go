package vultr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	defaultBaseURL = "https://api.vultr.com/v2"
	maxPageSize    = 500
)

type Client struct {
	http            *http.Client
	baseURL, apiKey string
}

type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Vultr API %s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}
func (e *APIError) NotFound() bool { return e.StatusCode == http.StatusNotFound }
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
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
	ID          string   `json:"id"`
	VCPUCount   int      `json:"vcpu_count"`
	RAM         int      `json:"ram"`
	Disk        int      `json:"disk"`
	Bandwidth   int      `json:"bandwidth"`
	MonthlyCost float64  `json:"monthly_cost"`
	Type        string   `json:"type"`
	Locations   []string `json:"locations"`
}

type planResponse struct {
	Plans []Plan `json:"plans"`
	Meta  Meta   `json:"meta"`
}

type PlanAvailability struct {
	AvailablePlans []string `json:"available_plans"`
}

type OperatingSystem struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Arch   string `json:"arch"`
	Family string `json:"family"`
}

type osResponse struct {
	OS   []OperatingSystem `json:"os"`
	Meta Meta              `json:"meta"`
}

type instanceResponse struct {
	Instance Instance `json:"instance"`
}
type instancesResponse struct {
	Instances []Instance `json:"instances"`
	Meta      Meta       `json:"meta"`
}

type Meta struct {
	Total int    `json:"total"`
	Links *Links `json:"links"`
}
type Links struct {
	Next string `json:"next"`
	Prev string `json:"prev"`
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
	return c.doURL(ctx, method, c.baseURL+path, body, out)
}

func (c *Client) doURL(ctx context.Context, method, rawURL string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, r)
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
		return &APIError{Method: method, Path: rawURL, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) ListPlans(ctx context.Context) ([]Plan, error) {
	var result []Plan
	cursor := ""
	for {
		path := "/plans?per_page=" + strconv.Itoa(maxPageSize)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var out planResponse
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		result = append(result, out.Plans...)
		next := ""
		if out.Meta.Links != nil {
			next = out.Meta.Links.Next
		}
		if next == "" {
			return result, nil
		}
		cursor = cursorFromNext(next)
		if cursor == "" {
			return nil, fmt.Errorf("Vultr returned an unparseable plans pagination link %q", next)
		}
	}
}

func (c *Client) ListOS(ctx context.Context) ([]OperatingSystem, error) {
	var result []OperatingSystem
	cursor := ""
	for {
		path := "/os?per_page=" + strconv.Itoa(maxPageSize)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var out osResponse
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		result = append(result, out.OS...)
		next := ""
		if out.Meta.Links != nil {
			next = out.Meta.Links.Next
		}
		if next == "" {
			return result, nil
		}
		cursor = cursorFromNext(next)
		if cursor == "" {
			return nil, fmt.Errorf("Vultr returned an unparseable OS pagination link %q", next)
		}
	}
}

func (c *Client) GetRegionAvailability(ctx context.Context, region string) (*PlanAvailability, error) {
	var out PlanAvailability
	if err := c.do(ctx, http.MethodGet, "/regions/"+url.PathEscape(region)+"/availability", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetInstance(ctx context.Context, id string) (*Instance, error) {
	var out instanceResponse
	if err := c.do(ctx, http.MethodGet, "/instances/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out.Instance, nil
}

func (c *Client) ListInstances(ctx context.Context) ([]Instance, error) {
	var result []Instance
	cursor := ""
	for {
		path := "/instances?per_page=" + strconv.Itoa(maxPageSize)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var out instancesResponse
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		result = append(result, out.Instances...)
		next := ""
		if out.Meta.Links != nil {
			next = out.Meta.Links.Next
		}
		if next == "" {
			return result, nil
		}
		cursor = cursorFromNext(next)
		if cursor == "" {
			return nil, fmt.Errorf("Vultr returned an unparseable instances pagination link %q", next)
		}
	}
}

// cursorFromNext resolves the value to send as the `cursor` query parameter for
// the next page.
//
// Vultr's `meta.links.next` is an opaque cursor token (for example
// "bmV4dF9fMTMxOTgxNQ=="), not a URL, and it is passed straight back as
// `?cursor=<token>`. An absolute URL carrying a `cursor` parameter is also
// accepted so that a future API change to link-style pagination keeps working.
func cursorFromNext(next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return ""
	}
	if u, err := url.Parse(next); err == nil && u.IsAbs() {
		if cursor := u.Query().Get("cursor"); cursor != "" {
			return cursor
		}
	}
	return next
}

func (c *Client) CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error) {
	payload := map[string]any{"region": req.Region, "plan": req.Plan, "hostname": req.Hostname, "label": req.Label, "enable_ipv6": req.EnableIPv6, "tags": req.Tags}
	// Vultr names these fields `sshkey_id` and `attach_vpc`, both arrays of IDs.
	// Sending `sshkey_ids`/`vpc_ids` is silently ignored by the API, which
	// produces instances with no SSH keys and no VPC attachment.
	if len(req.SSHKeyIDs) > 0 {
		payload["sshkey_id"] = req.SSHKeyIDs
	}
	if len(req.VPCIDs) > 0 {
		payload["attach_vpc"] = req.VPCIDs
	}
	if req.UserData != "" {
		payload["user_data"] = base64.StdEncoding.EncodeToString([]byte(req.UserData))
	}
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
	return c.do(ctx, http.MethodDelete, "/instances/"+url.PathEscape(id), nil, nil)
}
