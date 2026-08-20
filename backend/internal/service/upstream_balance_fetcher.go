package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const upstreamBalanceRequestTimeout = 30 * time.Second

// UpstreamBalanceFetchOptions contains only request-time values; credentials
// are intentionally not shared with any provider package.
type UpstreamBalanceFetchOptions struct {
	APIKey       string
	BaseURL      string
	ProxyURL     string
	AccountID    int64
	Concurrency  int
	HeaderApply  func(http.Header)
	HTTPUpstream HTTPUpstream
}

// UpstreamBalanceFetcher queries GET {base}/usage on sub2api-compatible
// upstreams (openai, kimi, zhipu, deepseek) with a runtime API key.
type UpstreamBalanceFetcher struct {
	httpUpstream HTTPUpstream
}

func NewUpstreamBalanceFetcher(httpUpstream HTTPUpstream) *UpstreamBalanceFetcher {
	return &UpstreamBalanceFetcher{httpUpstream: httpUpstream}
}

func (f *UpstreamBalanceFetcher) FetchUsage(ctx context.Context, opts *UpstreamBalanceFetchOptions) (*UpstreamBalanceUsage, error) {
	if opts == nil || strings.TrimSpace(opts.APIKey) == "" {
		return nil, &UpstreamBalanceError{Code: "unauthenticated"}
	}
	baseURL, err := validateOpenCodeUsageURL(opts.BaseURL)
	if err != nil {
		return nil, &UpstreamBalanceError{Code: "invalid_base_url"}
	}
	endpoint, err := url.JoinPath(baseURL, "/usage")
	if err != nil {
		return nil, &UpstreamBalanceError{Code: "invalid_base_url"}
	}

	reqCtx, cancel := context.WithTimeout(ctx, upstreamBalanceRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &UpstreamBalanceError{Code: "invalid_base_url"}
	}
	// URL validation is complete before this credential-bearing header exists.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(opts.APIKey))
	if opts.HeaderApply != nil {
		opts.HeaderApply(req.Header)
	}

	upstream := opts.HTTPUpstream
	if upstream == nil {
		upstream = f.httpUpstream
	}
	if upstream == nil {
		return nil, &UpstreamBalanceError{Code: "network_error"}
	}
	resp, err := upstream.Do(req, opts.ProxyURL, opts.AccountID, opts.Concurrency)
	if err != nil {
		return nil, &UpstreamBalanceError{Code: "network_error"}
	}
	if resp == nil || resp.Body == nil {
		return nil, &UpstreamBalanceError{Code: "network_error"}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &UpstreamBalanceError{Code: "network_error", HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, &UpstreamBalanceError{Code: "unauthenticated", HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, &UpstreamBalanceError{Code: "forbidden", HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &UpstreamBalanceError{Code: "rate_limited", HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &UpstreamBalanceError{Code: "upstream_error", HTTPStatus: resp.StatusCode}
	}
	usage, err := parseUpstreamBalanceResponse(body)
	if err != nil {
		return nil, &UpstreamBalanceError{Code: "invalid_response", HTTPStatus: resp.StatusCode}
	}
	return usage, nil
}

// UpstreamBalanceError is a stable, credential-safe balance failure category.
type UpstreamBalanceError struct {
	Code       string
	HTTPStatus int
}

func (e *UpstreamBalanceError) Error() string {
	if e == nil || e.Code == "" {
		return "upstream balance error"
	}
	return "upstream balance error: " + e.Code
}

// upstreamBalanceResponse is the raw envelope returned by {base}/usage.
type upstreamBalanceResponse struct {
	Balance   *float64 `json:"balance"`
	Remaining *float64 `json:"remaining"`
	Unit      string   `json:"unit"`
	Mode      string   `json:"mode"`
	PlanName  string   `json:"planName"`
	Usage     struct {
		Today *UpstreamBalanceStats `json:"today"`
		Total *UpstreamBalanceStats `json:"total"`
	} `json:"usage"`
}

// parseUpstreamBalanceResponse parses the upstream balance envelope into a
// typed UpstreamBalanceUsage without exposing body data.
func parseUpstreamBalanceResponse(body []byte) (*UpstreamBalanceUsage, error) {
	var parsed upstreamBalanceResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, errors.New("invalid balance response")
	}
	return &UpstreamBalanceUsage{
		Balance:   parsed.Balance,
		Remaining: parsed.Remaining,
		Unit:      parsed.Unit,
		Mode:      parsed.Mode,
		PlanName:  parsed.PlanName,
		Today:     parsed.Usage.Today,
		Total:     parsed.Usage.Total,
		Status:    "ok",
	}, nil
}
