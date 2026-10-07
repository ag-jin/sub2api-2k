package bigmodel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
)

// bigmodelBaseURL is the bigmodel (zhipu) web origin: the browser login page
// and the account management API live here (research FINAL-REPORT §六).
const bigmodelBaseURL = "https://bigmodel.cn"

// oauthTokenEndpoint is the ZCode relay that performs the bigmodel
// authorization-code exchange (research FINAL-REPORT §六).
const oauthTokenEndpoint = "https://zcode.z.ai/api/v1/oauth/token"

// loginAppID is the application id the bigmodel login page expects.
const loginAppID = "zcode"

// DefaultZCodeMinCallInterval paces the risk client ExchangeCode builds when no
// doer is injected; it mirrors the Gateway.Zhipu default of 30s and is also the
// fallback the service layer uses when no config is injected.
const DefaultZCodeMinCallInterval = 30 * time.Second

// defaultHTTPTimeout bounds one default-client call when no doer is injected.
const defaultHTTPTimeout = 30 * time.Second

// GenerateState returns a fresh CSRF state value for one login session: 32
// lowercase hex characters (16 random bytes).
func GenerateState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("bigmodel: generate login state: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// BuildLoginURL builds the authorization URL the administrator must open in a
// browser: https://bigmodel.cn/login?appId=zcode&redirect=<redirectURI>&state=<state>.
// Both redirectURI and state are percent-encoded; the URL carries no credential.
func BuildLoginURL(redirectURI, state string) string {
	query := url.Values{}
	query.Set("appId", loginAppID)
	query.Set("redirect", redirectURI)
	query.Set("state", state)
	return bigmodelBaseURL + "/login?" + query.Encode()
}

// tokenExchangeRequest is the wire body of the exchange: exactly
// {provider:"bigmodel", code, redirect_uri, state} (research FINAL-REPORT §六).
type tokenExchangeRequest struct {
	Provider    string `json:"provider"`
	Code        string `json:"code"`
	RedirectURI string `json:"redirect_uri"`
	State       string `json:"state"`
}

// tokenExchangeResponse is the exchange envelope: {code, msg, data:{...}}.
type tokenExchangeResponse struct {
	Code int                `json:"code"`
	Msg  string             `json:"msg"`
	Data *tokenExchangeData `json:"data"`
}

// tokenExchangeData carries the relayed credentials.
type tokenExchangeData struct {
	// Token is the zcode.z.ai JWT used by the reset-card endpoint.
	Token string `json:"token"`
	// ExpiresIn is optional.
	ExpiresIn int64 `json:"expires_in"`
	// Bigmodel holds the bigmodel side of the account.
	Bigmodel *tokenExchangeBigmodel `json:"bigmodel"`
}

// tokenExchangeBigmodel carries the bigmodel credentials. RefreshTokenAlt
// exists because the relay has been observed to answer either the snake_case or
// the camelCase spelling (research driver bigmodel-login2.mjs).
type tokenExchangeBigmodel struct {
	AccessToken     string `json:"access_token"`
	RefreshToken    string `json:"refresh_token"`
	RefreshTokenAlt string `json:"refreshToken"`
}

// TokenExchangeResult is the parsed exchange outcome. RefreshToken may be
// empty: the relay is allowed to answer without one and that is not a failure,
// so callers must gate "store the refresh token" on a non-empty value. An
// empty ZCodeJWTToken is likewise tolerated.
type TokenExchangeResult struct {
	AccessToken   string
	RefreshToken  string
	ZCodeJWTToken string
	ExpiresIn     int64
}

// Op values carried by Error.
const (
	opExchangeCode  = "exchange_code"
	opResolveAPIKey = "resolve_api_key"
)

// ErrorKind classifies a bigmodel protocol failure so callers (handler 04,
// keeper 09) map it without string matching and without seeing raw bodies.
type ErrorKind string

const (
	// ErrorKindConfig: the call was configured incoherently and never started.
	ErrorKindConfig ErrorKind = "config"
	// ErrorKindInvalidInput: a required caller argument is empty.
	ErrorKindInvalidInput ErrorKind = "invalid_input"
	// ErrorKindTransport: the request never produced an HTTP response.
	ErrorKindTransport ErrorKind = "transport"
	// ErrorKindHTTPStatus: upstream answered with a non-200 status.
	ErrorKindHTTPStatus ErrorKind = "http_status"
	// ErrorKindUpstreamCode: upstream answered 200 with a non-zero body code.
	ErrorKindUpstreamCode ErrorKind = "upstream_code"
	// ErrorKindInvalidBody: upstream answered with a body that is not the
	// expected JSON envelope.
	ErrorKindInvalidBody ErrorKind = "invalid_body"
	// ErrorKindMissingField: the JSON envelope is valid but a required field is
	// absent or empty; Field names it.
	ErrorKindMissingField ErrorKind = "missing_field"
	// ErrorKindRiskControl: the shared risk client refused the call before
	// touching the network (backoff window or minimum call interval).
	ErrorKindRiskControl ErrorKind = "risk_control"
)

// maxErrorMessageBytes bounds the upstream text copied into an error message.
const maxErrorMessageBytes = 200

// Error is the classified failure of one bigmodel protocol call. It carries no
// credentials: upstream text is bounded by maxErrorMessageBytes and only the
// upstream response body (never the request payload) may be embedded, so an
// error can name the upstream logid without echoing the authorization code.
type Error struct {
	// Op names the call: "exchange_code" or "resolve_api_key".
	Op string
	// Kind is the failure class.
	Kind ErrorKind
	// Status is the upstream HTTP status, 0 when no response was received.
	Status int
	// Code is the upstream body "code", 0 when absent.
	Code int
	// Field is the missing field path for Kind=missing_field.
	Field string
	// Message is the bounded upstream text, when available.
	Message string
	// Risk is set when the risk client refused the call.
	Risk *RiskError

	err error
}

func (e *Error) Error() string {
	if e == nil {
		return "bigmodel: <nil>"
	}

	details := make([]string, 0, 4)
	if e.Status != 0 {
		details = append(details, fmt.Sprintf("http %d", e.Status))
	}
	if e.Code != 0 {
		details = append(details, fmt.Sprintf("code %d", e.Code))
	}
	if e.Field != "" {
		details = append(details, "field "+e.Field)
	}
	if e.Risk != nil {
		details = append(details, fmt.Sprintf("risk %s, retry after %s", e.Risk.Kind, e.Risk.RetryAfter))
	}

	message := e.Message
	if message == "" && e.err != nil {
		message = e.err.Error()
	}

	summary := "bigmodel " + e.Op + ": " + string(e.Kind)
	if len(details) > 0 {
		summary += " (" + strings.Join(details, ", ") + ")"
	}
	if message != "" {
		summary += ": " + message
	}
	return summary
}

// Unwrap exposes the underlying cause for errors.Is/As.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// IsAuthRejected reports whether upstream rejected the credential: HTTP 401/403
// or the same codes inside a 200 body envelope. The keeper treats exactly this
// as "needs relogin"; every other failure must not flip that flag.
func IsAuthRejected(err error) bool {
	var classified *Error
	if !errors.As(err, &classified) {
		return false
	}
	return isAuthStatusCode(classified.Status) || isAuthStatusCode(classified.Code)
}

// isAuthStatusCode reports whether code is an authorization rejection.
func isAuthStatusCode(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden
}

// IsRiskBlocked reports whether risk control caused the failure: the risk
// client refused the call, or upstream answered with its risk verdict
// (3012/3001) or a rate limit. It maps to "retry later, or fall back to pasting
// the authCode" guidance in the admin UI.
func IsRiskBlocked(err error) bool {
	var classified *Error
	if !errors.As(err, &classified) {
		return false
	}
	if classified.Kind == ErrorKindRiskControl {
		return true
	}
	if classified.Status == http.StatusTooManyRequests {
		return true
	}
	return classified.Kind == ErrorKindUpstreamCode &&
		(classified.Code == riskCodeUnusualActivity || classified.Code == riskCodeParameterError)
}

// RetryAfter reports how long the risk client's window still blocks this
// endpoint, when the failure was a risk-client refusal.
func RetryAfter(err error) (time.Duration, bool) {
	var riskErr *RiskError
	if !errors.As(err, &riskErr) || riskErr.RetryAfter <= 0 {
		return 0, false
	}
	return riskErr.RetryAfter, true
}

// truncateMessage bounds upstream text before it is copied into an error.
func truncateMessage(message string) string {
	if len(message) <= maxErrorMessageBytes {
		return message
	}
	return message[:maxErrorMessageBytes] + "..."
}

// invalidInputError builds an invalid-input failure for a caller argument.
func invalidInputError(op, field string) *Error {
	return &Error{Op: op, Kind: ErrorKindInvalidInput, Field: field, Message: "required value is empty"}
}

// configError builds a config-classified failure.
func configError(op string, cause error) *Error {
	return &Error{Op: op, Kind: ErrorKindConfig, err: cause}
}

// transportError builds a transport-classified failure; a risk-client refusal
// is classified separately so callers can tell "blocked by us" from "network".
func transportError(op string, cause error) *Error {
	var riskErr *RiskError
	if errors.As(cause, &riskErr) {
		return &Error{Op: op, Kind: ErrorKindRiskControl, Risk: riskErr, err: cause}
	}
	return &Error{Op: op, Kind: ErrorKindTransport, err: cause}
}

// ExchangeCode redeems the browser authCode for bigmodel credentials through
// the ZCode relay.
//
// doer is the shared risk client and the only way the exchange reaches the
// network: the call cannot be issued outside the risk gate (design M1).
// Production passes the *RiskClient built from the Gateway.Zhipu configuration;
// a nil doer builds a default risk client over a plain HTTP transport, which is
// the only case proxyURL is honoured. Supplying both is a configuration error
// rather than a silent ignore: once a doer is injected, the caller owns
// transport selection (proxy included).
func ExchangeCode(ctx context.Context, doer *RiskClient, proxyURL, code, redirectURI, state string) (*TokenExchangeResult, error) {
	if strings.TrimSpace(code) == "" {
		return nil, invalidInputError(opExchangeCode, "code")
	}
	if strings.TrimSpace(redirectURI) == "" {
		return nil, invalidInputError(opExchangeCode, "redirect_uri")
	}
	if strings.TrimSpace(state) == "" {
		return nil, invalidInputError(opExchangeCode, "state")
	}

	risk, err := resolveRiskClient(doer, proxyURL)
	if err != nil {
		return nil, err
	}

	payload, err := json.Marshal(tokenExchangeRequest{
		Provider:    "bigmodel",
		Code:        code,
		RedirectURI: redirectURI,
		State:       state,
	})
	if err != nil {
		return nil, &Error{Op: opExchangeCode, Kind: ErrorKindInvalidInput, err: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, &Error{Op: opExchangeCode, Kind: ErrorKindInvalidInput, err: err}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := risk.Do(ctx, RiskEndpointOAuthToken, req)
	if err != nil {
		return nil, transportError(opExchangeCode, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, transportError(opExchangeCode, fmt.Errorf("read response: %w", err))
	}

	if resp.StatusCode != http.StatusOK {
		// 带上上游响应体（截断）：上游对一切被拒兑换统一回 500
		// {"code":2007,"msg":"http error","logid":...}，logid 是向智谱侧定位的唯一线索。
		// 只带响应体，不带请求入参（授权码/state/redirect_uri 都不回显）。
		return nil, &Error{
			Op:      opExchangeCode,
			Kind:    ErrorKindHTTPStatus,
			Status:  resp.StatusCode,
			Message: truncateMessage(strings.TrimSpace(string(raw))),
		}
	}

	return parseTokenExchangeResponse(raw, resp.StatusCode)
}

// parseTokenExchangeResponse validates a 200 exchange envelope and maps it onto
// the typed result. Every failure is classified and carries no credential: only
// the upstream code/message may be embedded, never the request payload.
func parseTokenExchangeResponse(raw []byte, status int) (*TokenExchangeResult, error) {
	var parsed tokenExchangeResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, &Error{Op: opExchangeCode, Kind: ErrorKindInvalidBody, Status: status, err: err}
	}
	if parsed.Code != 0 {
		return nil, &Error{
			Op:      opExchangeCode,
			Kind:    ErrorKindUpstreamCode,
			Status:  status,
			Code:    parsed.Code,
			Message: truncateMessage(parsed.Msg),
		}
	}
	if parsed.Data == nil {
		return nil, &Error{Op: opExchangeCode, Kind: ErrorKindMissingField, Status: status, Field: "data"}
	}
	if parsed.Data.Bigmodel == nil || parsed.Data.Bigmodel.AccessToken == "" {
		return nil, &Error{
			Op:     opExchangeCode,
			Kind:   ErrorKindMissingField,
			Status: status,
			Field:  "data.bigmodel.access_token",
		}
	}

	refreshToken := parsed.Data.Bigmodel.RefreshToken
	if refreshToken == "" {
		refreshToken = parsed.Data.Bigmodel.RefreshTokenAlt
	}
	return &TokenExchangeResult{
		AccessToken:   parsed.Data.Bigmodel.AccessToken,
		RefreshToken:  refreshToken,
		ZCodeJWTToken: parsed.Data.Token,
		ExpiresIn:     parsed.Data.ExpiresIn,
	}, nil
}

// resolveRiskClient resolves the risk-controlled seam of one call. An injected
// client is used as is so its pacing and negative cache are shared with the
// other zcode.z.ai callers (M4 reset-card); only a nil doer builds the default
// transport, where proxyURL applies.
func resolveRiskClient(doer *RiskClient, proxyURL string) (*RiskClient, error) {
	if doer != nil {
		if proxyURL != "" {
			return nil, configError(opExchangeCode, errors.New(
				"cannot combine an injected risk client with a proxy URL: the caller owns transport selection"))
		}
		return doer, nil
	}

	client, err := newDefaultHTTPClient(proxyURL)
	if err != nil {
		return nil, configError(opExchangeCode, err)
	}
	return NewRiskClient(client, DefaultZCodeMinCallInterval), nil
}

// newDefaultHTTPClient builds the transport used only when no doer is injected,
// applying proxyURL when it is set.
func newDefaultHTTPClient(proxyURL string) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("bigmodel: default transport is not *http.Transport")
	}
	clone := transport.Clone()
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("bigmodel: parse proxy URL: %w", err)
		}
		if err := proxyutil.ConfigureTransportProxy(clone, parsed); err != nil {
			return nil, fmt.Errorf("bigmodel: configure proxy: %w", err)
		}
	}
	return &http.Client{Transport: clone, Timeout: defaultHTTPTimeout}, nil
}
