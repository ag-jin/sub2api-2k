package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/opencode"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

const opencodeUsageRequestTimeout = 30 * time.Second

// OpenCodeUsageFetchOptions contains only request-time values; credentials are
// intentionally not shared with the OpenCode package or any other provider.
type OpenCodeUsageFetchOptions struct {
	APIKey       string
	BaseURL      string
	ProxyURL     string
	AccountID    int64
	Concurrency  int
	HeaderApply  func(http.Header)
	HTTPUpstream HTTPUpstream
}

// OpenCodeUsageFetcher queries GET {base}/usage with a runtime API key.
type OpenCodeUsageFetcher struct {
	httpUpstream HTTPUpstream
}

func NewOpenCodeUsageFetcher(httpUpstream HTTPUpstream) *OpenCodeUsageFetcher {
	return &OpenCodeUsageFetcher{httpUpstream: httpUpstream}
}

func (f *OpenCodeUsageFetcher) FetchUsage(ctx context.Context, opts *OpenCodeUsageFetchOptions) (*opencode.UsageSnapshot, error) {
	if opts == nil || strings.TrimSpace(opts.APIKey) == "" {
		return nil, &opencode.UsageError{Code: "unauthenticated"}
	}
	baseURL, err := validateOpenCodeUsageURL(opts.BaseURL)
	if err != nil {
		return nil, &opencode.UsageError{Code: "invalid_base_url"}
	}
	endpoint, err := url.JoinPath(baseURL, opencode.UsagePath)
	if err != nil {
		return nil, &opencode.UsageError{Code: "invalid_base_url"}
	}

	reqCtx, cancel := context.WithTimeout(ctx, opencodeUsageRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &opencode.UsageError{Code: "invalid_base_url"}
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
		return nil, &opencode.UsageError{Code: "network_error"}
	}
	resp, err := upstream.Do(req, opts.ProxyURL, opts.AccountID, opts.Concurrency)
	if err != nil {
		return nil, &opencode.UsageError{Code: "network_error"}
	}
	if resp == nil || resp.Body == nil {
		return nil, &opencode.UsageError{Code: "network_error"}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &opencode.UsageError{Code: "network_error", HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, &opencode.UsageError{Code: "unauthenticated", HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, &opencode.UsageError{Code: "forbidden", HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &opencode.UsageError{Code: "rate_limited", HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &opencode.UsageError{Code: "upstream_error", HTTPStatus: resp.StatusCode}
	}
	snapshot, err := opencode.ParseUsageResponse(body)
	if err != nil {
		return nil, &opencode.UsageError{Code: "invalid_response", HTTPStatus: resp.StatusCode}
	}
	return snapshot, nil
}

func validateOpenCodeUsageURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = opencode.DefaultBaseURL
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid base url")
	}
	// Keep the provider's default permissive URL policy while still rejecting
	// malformed, non-HTTP, credential-bearing, and private targets early.
	normalized, err := urlvalidator.ValidateHTTPURL(trimmed, false, urlvalidator.ValidationOptions{
		AllowPrivate: false,
	})
	if err != nil {
		return "", errors.New("invalid base url")
	}
	return strings.TrimRight(normalized, "/"), nil
}
