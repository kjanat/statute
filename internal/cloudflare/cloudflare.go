// Package cloudflare is a tiny Cloudflare DNS API client used for ACME
// DNS-01 challenges. It implements only the endpoints statute needs (list
// zones, create/delete TXT records) and depends solely on the standard
// library. Kept in internal/ so it is not part of the public API surface.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a tiny Cloudflare DNS API client. It implements only the
// endpoints needed for DNS-01 challenges: list zones and create/delete TXT
// records. The full-featured cloudflare-go library is intentionally avoided —
// it pulls in many transitive deps for capabilities we never use.
type Client struct {
	token  string
	client *http.Client
	base   string
}

// New returns a Client authenticated with the given API token.
func New(token string) *Client {
	return &Client{
		token:  token,
		client: &http.Client{Timeout: 30 * time.Second},
		base:   "https://api.cloudflare.com/client/v4",
	}
}

type cfAPIResponse struct {
	Success bool            `json:"success"`
	Errors  []cfError       `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e cfError) Error() string {
	return fmt.Sprintf("cloudflare api: %d %s", e.Code, e.Message)
}

// FindZoneID returns the zone ID for the zone whose name is a suffix of the
// given domain. The lookup walks the DNS labels from most-specific to least
// (a record at sub.example.com tries sub.example.com, then example.com).
func (c *Client) FindZoneID(ctx context.Context, domain string) (string, error) {
	domain = strings.TrimPrefix(domain, "*.")
	labels := strings.Split(domain, ".")
	for i := 0; i < len(labels)-1; i++ {
		candidate := strings.Join(labels[i:], ".")
		id, err := c.getZoneByName(ctx, candidate)
		if err == nil && id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("cloudflare: no zone found for %q (token must have access to the zone)", domain)
}

func (c *Client) getZoneByName(ctx context.Context, name string) (string, error) {
	q := url.Values{"name": {name}, "status": {"active"}}
	var zones []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, "/zones?"+q.Encode(), nil, &zones); err != nil {
		return "", err
	}
	if len(zones) == 0 {
		return "", nil
	}
	return zones[0].ID, nil
}

// AddTXTRecord creates a TXT record under the given zone and returns its ID
// so it can be deleted after the challenge resolves. TTL is set to the
// minimum (60s on Cloudflare's free plan) so propagation is fast.
func (c *Client) AddTXTRecord(ctx context.Context, zoneID, name, value string) (string, error) {
	body := map[string]any{
		"type":    "TXT",
		"name":    name,
		"content": value,
		"ttl":     60,
	}
	var rec struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", body, &rec); err != nil {
		return "", err
	}
	return rec.ID, nil
}

// DeleteRecord removes a record. Errors are returned to the caller; callers
// should not abort renewal on cleanup failure (the TXT record is useless
// after the challenge resolves and Cloudflare expires it).
func (c *Client) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	return c.do(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+recordID, nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var bodyR io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyR = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bodyR)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	var apiResp cfAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return fmt.Errorf("cloudflare api: decode response: %w", err)
	}
	if !apiResp.Success {
		if len(apiResp.Errors) > 0 {
			return apiResp.Errors[0]
		}
		return errors.New("cloudflare api: unspecified error")
	}
	if out != nil && len(apiResp.Result) > 0 {
		return json.Unmarshal(apiResp.Result, out)
	}
	return nil
}
