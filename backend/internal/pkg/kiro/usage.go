package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// UsageLimitsResponse mirrors the Kiro getUsageLimits response (subset).
type UsageLimitsResponse struct {
	NextDateReset        float64                `json:"nextDateReset,omitempty"`
	SubscriptionInfo     *UsageSubscriptionInfo `json:"subscriptionInfo,omitempty"`
	UsageBreakdownList   []UsageBreakdown       `json:"usageBreakdownList,omitempty"`
	OverageConfiguration *OverageConfiguration  `json:"overageConfiguration,omitempty"`
}

// OverageConfiguration is the account-level overage switch state.
type OverageConfiguration struct {
	OverageStatus string `json:"overageStatus,omitempty"` // "ENABLED" | "DISABLED"
	OverageLimit  *int64 `json:"overageLimit,omitempty"`
}

type UsageSubscriptionInfo struct {
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"`
	Type              string `json:"type,omitempty"`
	OverageCapability string `json:"overageCapability,omitempty"` // "OVERAGE_CAPABLE" | "OVERAGE_INCAPABLE"
}

type UsageBreakdown struct {
	CurrentUsage              int64   `json:"currentUsage,omitempty"`
	CurrentUsageWithPrecision float64 `json:"currentUsageWithPrecision,omitempty"`
	UsageLimit                int64   `json:"usageLimit,omitempty"`
	UsageLimitWithPrecision   float64 `json:"usageLimitWithPrecision,omitempty"`
	NextDateReset             float64 `json:"nextDateReset,omitempty"`
	ResourceType              string  `json:"resourceType,omitempty"`
	DisplayName               string  `json:"displayName,omitempty"`
	// Overage fields (Kiro credit overage)
	OverageCap                int64   `json:"overageCap,omitempty"`
	OverageRate               float64 `json:"overageRate,omitempty"`
	CurrentOverages           int64   `json:"currentOverages,omitempty"`
	OverageCharges            float64 `json:"overageCharges,omitempty"`
	Unit                      string  `json:"unit,omitempty"`
}

// OverageCapable reports whether the subscription can use overage at all.
func (r *UsageLimitsResponse) OverageCapable() bool {
	return r.SubscriptionInfo != nil && r.SubscriptionInfo.OverageCapability == "OVERAGE_CAPABLE"
}

// OverageEnabled reports whether overage is currently switched on.
func (r *UsageLimitsResponse) OverageEnabled() bool {
	return r.OverageConfiguration != nil && r.OverageConfiguration.OverageStatus == "ENABLED"
}

// SubscriptionTitle returns the subscription title if present.
func (r *UsageLimitsResponse) SubscriptionTitle() string {
	if r.SubscriptionInfo != nil {
		return r.SubscriptionInfo.SubscriptionTitle
	}
	return ""
}

// Primary returns the first usage breakdown entry (the credit pool), or nil.
func (r *UsageLimitsResponse) Primary() *UsageBreakdown {
	if len(r.UsageBreakdownList) > 0 {
		return &r.UsageBreakdownList[0]
	}
	return nil
}

// EffectiveTotalLimit returns the true total quota ceiling for the primary credit
// pool. Kiro bills in two tiers: a base subscription pool (usageLimit) plus an
// opt-in overage pool (overageCap). The real ceiling is base+cap ONLY when
// overage is enabled; otherwise it's just the base limit.
//
// This prevents the misleading >100% utilization that appears when currentUsage
// (which can run into the overage pool, e.g. 8235) is divided by the base limit
// alone (e.g. 1000 → 823%). Returns 0 if there is no usage breakdown.
// Precision-aware fields are preferred when present.
func (r *UsageLimitsResponse) EffectiveTotalLimit() float64 {
	p := r.Primary()
	if p == nil {
		return 0
	}
	base := p.UsageLimitWithPrecision
	if base == 0 {
		base = float64(p.UsageLimit)
	}
	if r.OverageEnabled() {
		return base + float64(p.OverageCap)
	}
	return base
}

// FetchUsageLimits queries the Kiro getUsageLimits endpoint for a credential.
// The credential must already have a valid access token (call RefreshToken first).
func FetchUsageLimits(ctx context.Context, cred *KiroCredential, cfg KiroConfig) (*UsageLimitsResponse, error) {
	region := cred.EffectiveAPIRegion()
	host := fmt.Sprintf("management.%s.kiro.dev", region)
	// Query params ordered to match a real 0.12.301 getUsageLimits request.
	// url.Values.Encode sorts keys alphabetically (isEmailRequired, origin,
	// profileArn, resourceType) and percent-encodes the ARN.
	q := url.Values{}
	q.Set("isEmailRequired", "true")
	q.Set("origin", "AI_EDITOR")
	q.Set("resourceType", "AGENTIC_REQUEST")
	if cred.ProfileArn != "" {
		q.Set("profileArn", cred.ProfileArn)
	}
	reqURL := fmt.Sprintf("https://%s/getUsageLimits?%s", host, q.Encode())

	mid := cred.EffectiveMachineID()
	userAgent := fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/%s lang/js md/nodejs#%s api/codewhispererruntime#%s m/N,E KiroIDE-%s-%s",
		AwsSdkVersionMgmt, cred.EffectiveSystemVersion(cfg), cfg.NodeVersion, AwsSdkVersionMgmt, cfg.KiroVersion, mid,
	)
	amzUA := fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s-%s", AwsSdkVersionMgmt, cfg.KiroVersion, mid)

	client, err := buildHTTPClient(cred.ProxyURL, 60*time.Second)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-amz-user-agent", amzUA)
	req.Header.Set("user-agent", userAgent)
	req.Header.Set("host", host)
	req.Header.Set("amz-sdk-invocation-id", newUUID())
	req.Header.Set("amz-sdk-request", "attempt=1; max=1")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("Connection", "close")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("getUsageLimits failed: %d %s", resp.StatusCode, string(body))
	}

	var out UsageLimitsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode usage limits: %w", err)
	}
	return &out, nil
}

// listProfilesResponse mirrors the ListAvailableProfiles response. We only need
// the first profile's ARN.
type listProfilesResponse struct {
	Profiles []struct {
		Arn string `json:"arn"`
	} `json:"profiles"`
	NextToken *string `json:"nextToken,omitempty"`
}

// FetchProfileArn calls ListAvailableProfiles on the management endpoint to
// obtain the account's profileArn. This is required because new Kiro 0.12.x
// endpoints (runtime/management.kiro.dev) MANDATE profileArn, but IdC (Enterprise
// SSO) token refresh does NOT return it (unlike Social refresh, which does).
// Kiro IDE itself fetches profileArn via this endpoint at startup.
//
// Request mirrors a real 0.12.301 ListAvailableProfiles call: POST with empty
// JSON body {}, management.kiro.dev host, codewhispererruntime#1.0.0 UA.
// The credential must already carry a valid access token (call after refresh).
func FetchProfileArn(ctx context.Context, cred *KiroCredential, cfg KiroConfig) (string, error) {
	region := cred.EffectiveAPIRegion()
	host := fmt.Sprintf("management.%s.kiro.dev", region)
	reqURL := fmt.Sprintf("https://%s/ListAvailableProfiles", host)

	mid := cred.EffectiveMachineID()
	userAgent := fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/%s lang/js md/nodejs#%s api/codewhispererruntime#%s m/N,E KiroIDE-%s-%s",
		AwsSdkVersionMgmt, cred.EffectiveSystemVersion(cfg), cfg.NodeVersion, AwsSdkVersionMgmt, cfg.KiroVersion, mid,
	)
	amzUA := fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s-%s", AwsSdkVersionMgmt, cfg.KiroVersion, mid)

	client, err := buildHTTPClient(cred.ProxyURL, 60*time.Second)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-amz-user-agent", amzUA)
	req.Header.Set("user-agent", userAgent)
	req.Header.Set("host", host)
	req.Header.Set("amz-sdk-invocation-id", newUUID())
	req.Header.Set("amz-sdk-request", "attempt=1; max=1")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("Connection", "close")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ListAvailableProfiles failed: %d %s", resp.StatusCode, string(body))
	}

	var out listProfilesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode ListAvailableProfiles: %w", err)
	}
	if len(out.Profiles) == 0 || out.Profiles[0].Arn == "" {
		return "", fmt.Errorf("ListAvailableProfiles returned no profile")
	}
	return out.Profiles[0].Arn, nil
}
