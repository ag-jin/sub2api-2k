package service

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/opencode"
	"github.com/imroc/req/v3"
)

// OpenCodeOAuthService drives the half-automatic OpenAuth login for OpenCode GO.
//
// Flow: GenerateAuthURL → admin logs in via browser (GitHub/Google) → OpenAuth
// redirects to a localhost callback with ?code= → admin pastes code back →
// ExchangeCode swaps it for tokens. Because OpenAuth only whitelists localhost
// (not arbitrary redirect_uris) for client "app", and because the resulting
// token is neither the sk- API key nor usable against any usage API
// (anomalyco/opencode#8911), this login only captures identity/email. The sk-
// key is still entered manually when creating the account.
type OpenCodeOAuthService struct {
	sessionStore *openai.SessionStore
}

// NewOpenCodeOAuthService constructs the service with its own session store.
func NewOpenCodeOAuthService() *OpenCodeOAuthService {
	return &OpenCodeOAuthService{sessionStore: opencode.NewOAuthSessionStore()}
}

// OpenCodeAuthURLResult is returned to the frontend to drive the login popup.
type OpenCodeAuthURLResult struct {
	AuthURL   string `json:"auth_url"`
	SessionID string `json:"session_id"`
}

// GenerateAuthURL builds an OpenAuth authorization URL + session for the given
// social provider (github/google). redirectURI defaults to the localhost
// callback OpenAuth accepts.
func (s *OpenCodeOAuthService) GenerateAuthURL(ctx context.Context, provider, redirectURI string) (*OpenCodeAuthURLResult, error) {
	state, err := openai.GenerateState()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENCODE_OAUTH_STATE_FAILED", "failed to generate state: %v", err)
	}
	codeVerifier, err := openai.GenerateCodeVerifier()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENCODE_OAUTH_VERIFIER_FAILED", "failed to generate code verifier: %v", err)
	}
	codeChallenge := openai.GenerateCodeChallenge(codeVerifier)
	sessionID, err := openai.GenerateSessionID()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENCODE_OAUTH_SESSION_FAILED", "failed to generate session ID: %v", err)
	}

	if redirectURI == "" {
		redirectURI = opencode.DefaultRedirectURI
	}

	s.sessionStore.Set(sessionID, &openai.OAuthSession{
		State:        state,
		CodeVerifier: codeVerifier,
		ClientID:     opencode.OAuthClientID,
		RedirectURI:  redirectURI,
		CreatedAt:    time.Now(),
	})

	authURL := opencode.BuildAuthorizationURL(state, codeChallenge, redirectURI, opencode.OAuthProvider(strings.TrimSpace(provider)))
	return &OpenCodeAuthURLResult{AuthURL: authURL, SessionID: sessionID}, nil
}

// OpenCodeExchangeCodeInput carries the code-exchange parameters.
type OpenCodeExchangeCodeInput struct {
	SessionID   string
	Code        string
	State       string
	RedirectURI string
}

// OpenCodeTokenInfo is the decoded token result.
type OpenCodeTokenInfo struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"`
	Email        string `json:"email,omitempty"`
}

// ExchangeCode swaps an authorization code for OpenAuth tokens.
func (s *OpenCodeOAuthService) ExchangeCode(ctx context.Context, input *OpenCodeExchangeCodeInput) (*OpenCodeTokenInfo, error) {
	session, ok := s.sessionStore.Get(input.SessionID)
	if !ok {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENCODE_OAUTH_SESSION_NOT_FOUND", "session not found or expired")
	}
	if strings.TrimSpace(input.State) == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENCODE_OAUTH_STATE_REQUIRED", "oauth state is required")
	}
	if subtle.ConstantTimeCompare([]byte(input.State), []byte(session.State)) != 1 {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENCODE_OAUTH_INVALID_STATE", "invalid oauth state")
	}

	redirectURI := input.RedirectURI
	if redirectURI == "" {
		redirectURI = session.RedirectURI
	}
	if redirectURI == "" {
		redirectURI = opencode.DefaultRedirectURI
	}

	client := req.C().SetTimeout(30 * time.Second)
	formData := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     opencode.OAuthClientID,
		"code":          strings.TrimSpace(input.Code),
		"redirect_uri":  redirectURI,
		"code_verifier": session.CodeVerifier,
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	resp, err := client.R().
		SetContext(ctx).
		SetHeader("User-Agent", "sub2api-opencode/1.0").
		SetFormData(formData).
		SetSuccessResult(&tokenResp).
		Post(opencode.OAuthTokenURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENCODE_OAUTH_REQUEST_FAILED", "token request failed: %v", err)
	}
	if !resp.IsSuccessState() {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENCODE_OAUTH_TOKEN_EXCHANGE_FAILED", "token exchange failed: status %d, body: %s", resp.StatusCode, resp.String())
	}

	s.sessionStore.Delete(input.SessionID)

	info := &OpenCodeTokenInfo{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		IDToken:      tokenResp.IDToken,
		ExpiresIn:    tokenResp.ExpiresIn,
	}
	if tokenResp.ExpiresIn > 0 {
		info.ExpiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Unix()
	}
	info.Email = emailFromIDToken(tokenResp.IDToken)
	return info, nil
}

// emailFromIDToken best-effort extracts the email claim from an unverified JWT
// id_token payload. Used only to pre-fill the account name; not a security
// boundary (the token is never trusted for auth).
func emailFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Email
}
