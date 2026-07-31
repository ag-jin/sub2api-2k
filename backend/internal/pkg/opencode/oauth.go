package opencode

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// OpenCode GO uses OpenAuth (openauth.js.org) as its OAuth2 server.
// Confirmed live (2026-07): issuer https://auth.opencode.ai, standard
// authorization-code flow, social login behind GitHub/Google providers.
//
// Critical constraints discovered by probing:
//   - client_id is fixed to "app"; custom client_id is rejected.
//   - redirect_uri must be whitelisted. Only opencode's own callback and
//     localhost are accepted; arbitrary https redirect_uris return
//     unauthorized_client. So we use a localhost redirect and have the admin
//     paste the code back (half-automatic flow, same as the kam/Kiro pattern).
//   - PKCE is optional but we send S256 anyway.
//   - The resulting OAuth token is NOT the GO API key (sk-...) and there is no
//     usage API to call with it (see anomalyco/opencode#8911). So login only
//     proves identity; the sk- key is still entered manually.
const (
	OAuthClientID     = "app"
	OAuthAuthorizeURL = "https://auth.opencode.ai/authorize"
	OAuthTokenURL     = "https://auth.opencode.ai/token"
	OAuthJWKSURL      = "https://auth.opencode.ai/.well-known/jwks.json"

	// DefaultRedirectURI is a localhost callback (the only non-opencode
	// redirect OpenAuth accepts for client "app").
	DefaultRedirectURI = "http://localhost:1456/auth/callback"

	// OAuthSessionTTL bounds how long a generated auth session stays valid.
	OAuthSessionTTL = 30 * time.Minute
)

// OAuthProvider is the social login provider selected in the auth URL.
type OAuthProvider string

const (
	OAuthProviderGitHub OAuthProvider = "github"
	OAuthProviderGoogle OAuthProvider = "google"
)

// NewOAuthSessionStore returns a session store for opencode OAuth flows.
// It reuses the generic openai.SessionStore (PKCE state + TTL cleanup) since
// the mechanics are identical.
func NewOAuthSessionStore() *openai.SessionStore {
	return openai.NewSessionStore()
}

// BuildAuthorizationURL builds the OpenAuth authorization URL for the given
// provider. When provider is empty the OpenAuth provider-select page is shown.
func BuildAuthorizationURL(state, codeChallenge, redirectURI string, provider OAuthProvider) string {
	if redirectURI == "" {
		redirectURI = DefaultRedirectURI
	}
	params := map[string]string{
		"response_type":         "code",
		"client_id":             OAuthClientID,
		"redirect_uri":          redirectURI,
		"state":                 state,
		"code_challenge":        codeChallenge,
		"code_challenge_method": "S256",
	}
	if provider != "" {
		params["provider"] = string(provider)
	}
	return OAuthAuthorizeURL + "?" + encodeParams(params)
}

func encodeParams(params map[string]string) string {
	first := true
	var b []byte
	for k, v := range params {
		if !first {
			b = append(b, '&')
		}
		b = append(b, urlEncode(k)...)
		b = append(b, '=')
		b = append(b, urlEncode(v)...)
		first = false
	}
	return string(b)
}

func urlEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b = append(b, c)
		} else {
			b = append(b, '%', hex[c>>4], hex[c&0xf])
		}
	}
	return string(b)
}
