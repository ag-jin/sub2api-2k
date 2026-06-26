package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RefreshTokenInvalidError indicates the refresh token is permanently revoked.
// The caller should disable the credential rather than retry.
type RefreshTokenInvalidError struct {
	Message string
}

func (e *RefreshTokenInvalidError) Error() string { return e.Message }

// IsRefreshTokenInvalid reports whether err is a permanent refresh-token failure.
func IsRefreshTokenInvalid(err error) bool {
	var target *RefreshTokenInvalidError
	return errors.As(err, &target)
}

// idcRefreshRequest is the IdC (AWS SSO OIDC) refresh request body.
type idcRefreshRequest struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	RefreshToken string `json:"refreshToken"`
	GrantType    string `json:"grantType"`
}

// idcRefreshResponse is the IdC refresh response body.
type idcRefreshResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresIn    int64  `json:"expiresIn,omitempty"`
	ProfileArn   string `json:"profileArn,omitempty"`
}

// socialRefreshRequest is the Social (Kiro) refresh request body.
type socialRefreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// socialRefreshResponse is the Social refresh response body.
type socialRefreshResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ProfileArn   string `json:"profileArn,omitempty"`
	ExpiresIn    int64  `json:"expiresIn,omitempty"`
}

// RefreshToken refreshes the credential according to its auth method.
// It returns a TokenResult; the caller is responsible for persisting the new
// values (access token, possibly rotated refresh token, expiry, profile ARN).
func RefreshToken(ctx context.Context, cred *KiroCredential, cfg KiroConfig) (*TokenResult, error) {
	if err := validateRefreshToken(cred); err != nil {
		return nil, err
	}
	if cred.IsIdC() {
		return refreshIdC(ctx, cred, cfg)
	}
	return refreshSocial(ctx, cred, cfg)
}

func validateRefreshToken(cred *KiroCredential) error {
	rt := cred.RefreshToken
	if rt == "" {
		return fmt.Errorf("missing refreshToken")
	}
	if len(rt) < 100 || strings.HasSuffix(rt, "...") || strings.Contains(rt, "...") {
		return fmt.Errorf("refreshToken appears truncated (len=%d); Kiro IDE may have deliberately truncated it", len(rt))
	}
	return nil
}

func refreshIdC(ctx context.Context, cred *KiroCredential, cfg KiroConfig) (*TokenResult, error) {
	if cred.ClientID == "" || cred.ClientSecret == "" {
		return nil, fmt.Errorf("IdC refresh requires clientId and clientSecret")
	}
	region := cred.EffectiveAuthRegion()
	url := fmt.Sprintf("https://oidc.%s.amazonaws.com/token", region)

	reqBody, _ := json.Marshal(idcRefreshRequest{
		ClientID:     cred.ClientID,
		ClientSecret: cred.ClientSecret,
		RefreshToken: cred.RefreshToken,
		GrantType:    "refresh_token",
	})

	userAgent := fmt.Sprintf(
		"aws-sdk-js/3.980.0 ua/2.1 os/%s lang/js md/nodejs#%s api/sso-oidc#3.980.0 m/E KiroIDE",
		cred.EffectiveSystemVersion(cfg), cfg.NodeVersion,
	)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-amz-user-agent", "aws-sdk-js/3.980.0 KiroIDE")
	httpReq.Header.Set("user-agent", userAgent)
	httpReq.Header.Set("host", fmt.Sprintf("oidc.%s.amazonaws.com", region))
	httpReq.Header.Set("amz-sdk-invocation-id", newUUID())
	httpReq.Header.Set("amz-sdk-request", "attempt=1; max=4")
	httpReq.Header.Set("Connection", "close")

	client, err := buildHTTPClient(cred.ProxyURL, 60*time.Second)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == 400 &&
			bytes.Contains(body, []byte(`"invalid_grant"`)) &&
			bytes.Contains(body, []byte("Invalid refresh token provided")) {
			return nil, &RefreshTokenInvalidError{
				Message: fmt.Sprintf("IdC refreshToken invalid (invalid_grant): %s", string(body)),
			}
		}
		return nil, fmt.Errorf("IdC token refresh failed: %d %s", resp.StatusCode, string(body))
	}

	var data idcRefreshResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("decode IdC response: %w", err)
	}

	return &TokenResult{
		AccessToken:  data.AccessToken,
		RefreshToken: data.RefreshToken,
		ProfileArn:   data.ProfileArn,
		ExpiresIn:    data.ExpiresIn,
	}, nil
}

func refreshSocial(ctx context.Context, cred *KiroCredential, cfg KiroConfig) (*TokenResult, error) {
	region := cred.EffectiveAuthRegion()
	url := fmt.Sprintf("https://prod.%s.auth.desktop.kiro.dev/refreshToken", region)
	domain := fmt.Sprintf("prod.%s.auth.desktop.kiro.dev", region)

	machineID := cred.EffectiveMachineID()

	reqBody, _ := json.Marshal(socialRefreshRequest{RefreshToken: cred.RefreshToken})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json, text/plain, */*")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", fmt.Sprintf("KiroIDE-%s-%s", cfg.KiroVersion, machineID))
	httpReq.Header.Set("Accept-Encoding", "gzip, compress, deflate, br")
	httpReq.Header.Set("host", domain)
	httpReq.Header.Set("Connection", "close")

	client, err := buildHTTPClient(cred.ProxyURL, 60*time.Second)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == 400 &&
			bytes.Contains(body, []byte(`"invalid_grant"`)) &&
			bytes.Contains(body, []byte("Invalid refresh token provided")) {
			return nil, &RefreshTokenInvalidError{
				Message: fmt.Sprintf("Social refreshToken invalid (invalid_grant): %s", string(body)),
			}
		}
		return nil, fmt.Errorf("Social token refresh failed: %d %s", resp.StatusCode, string(body))
	}

	var data socialRefreshResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("decode Social response: %w", err)
	}

	return &TokenResult{
		AccessToken:  data.AccessToken,
		RefreshToken: data.RefreshToken,
		ProfileArn:   data.ProfileArn,
		ExpiresIn:    data.ExpiresIn,
	}, nil
}

// ApplyTokenResult updates a credential in place with refresh results.
func ApplyTokenResult(cred *KiroCredential, res *TokenResult) {
	cred.AccessToken = res.AccessToken
	if res.RefreshToken != "" {
		cred.RefreshToken = res.RefreshToken
	}
	if res.ProfileArn != "" {
		cred.ProfileArn = res.ProfileArn
	}
	if res.ExpiresIn > 0 {
		cred.ExpiresAt = time.Now().Add(time.Duration(res.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
}
