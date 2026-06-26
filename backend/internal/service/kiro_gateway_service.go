package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// kiroDebugBody reports whether full request bodies should be logged (for
// maintenance/diagnosing "request body drift" after Kiro upgrades). Off by
// default; set KIRO_DEBUG_BODY=1 to enable. Bodies may contain user content,
// so this is opt-in only.
func kiroDebugBody() bool {
	return os.Getenv("KIRO_DEBUG_BODY") == "1"
}

// KiroGatewayService forwards Anthropic Messages requests to the Kiro IDE backend.
// It mirrors the role of AntigravityGatewayService for the Kiro platform.
type KiroGatewayService struct {
	kiroCfg kiro.KiroConfig

	// accountRepo persists refreshed credentials back to the DB and marks
	// accounts rate-limited / temp-unschedulable on upstream errors. May be nil
	// in lightweight contexts (account connection test) where no persistence is
	// needed; all repo access is nil-guarded.
	accountRepo AccountRepository

	// tokenMu guards per-account in-memory token refresh to avoid stampedes.
	tokenMu sync.Map // accountID -> *sync.Mutex

	// tokenCache caches refreshed access tokens per account so we don't perform
	// an AWS SSO OIDC refresh on every request (which added ~0.65s of first-token
	// latency). Mirrors kiro.rs TokenManager's in-memory credential cache.
	// accountID -> *cachedToken. Invalidated on upstream token rejection.
	tokenCache sync.Map
}

// cachedToken holds a refreshed access token and its expiry for reuse across
// requests. accessToken is reused while !expired (5-min buffer, see IsExpired).
type cachedToken struct {
	accessToken string
	expiresAt   string // RFC3339, mirrors KiroCredential.ExpiresAt
	profileArn  string // resolved via ListAvailableProfiles (IdC) or refresh (Social); cached to avoid re-fetching
}

// NewKiroGatewayService constructs a KiroGatewayService. accountRepo may be nil
// (e.g. for connection-test paths that never persist or fail over).
func NewKiroGatewayService(accountRepo AccountRepository) *KiroGatewayService {
	return &KiroGatewayService{
		kiroCfg:     kiro.DefaultKiroConfig(),
		accountRepo: accountRepo,
	}
}

// ginSSEWriter adapts gin.Context to kiro.SSEWriter, emitting Anthropic SSE frames.
type ginSSEWriter struct {
	c            *gin.Context
	flusher      http.Flusher
	wroteHeader  bool
	firstTokenAt *time.Time
	startedAt    time.Time
	mu           sync.Mutex // guards concurrent writes (SSE events vs keepalive pings)
}

func (g *ginSSEWriter) WriteSSE(event string, data map[string]interface{}) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.wroteHeader {
		g.c.Writer.Header().Set("Content-Type", "text/event-stream")
		g.c.Writer.Header().Set("Cache-Control", "no-cache")
		g.c.Writer.Header().Set("Connection", "keep-alive")
		g.c.Writer.WriteHeader(http.StatusOK)
		g.wroteHeader = true
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if g.firstTokenAt == nil && event == "content_block_delta" {
		now := time.Now()
		g.firstTokenAt = &now
	}
	if _, err := fmt.Fprintf(g.c.Writer, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return err
	}
	if g.flusher != nil {
		g.flusher.Flush()
	}
	return nil
}

// writePing sends an SSE keepalive ping, guarded by the same mutex so it never
// interleaves with a real event mid-write. Mirrors kiro.rs PING_INTERVAL_SECS.
func (g *ginSSEWriter) writePing() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.wroteHeader {
		g.c.Writer.Header().Set("Content-Type", "text/event-stream")
		g.c.Writer.Header().Set("Cache-Control", "no-cache")
		g.c.Writer.Header().Set("Connection", "keep-alive")
		g.c.Writer.WriteHeader(http.StatusOK)
		g.wroteHeader = true
	}
	_, _ = fmt.Fprint(g.c.Writer, "event: ping\ndata: {\"type\": \"ping\"}\n\n")
	if g.flusher != nil {
		g.flusher.Flush()
	}
}

// kiroPingInterval is the SSE keepalive interval (matches kiro.rs 25s), used to
// keep long thinking / large-context streams from timing out client-side.
const kiroPingInterval = 25 * time.Second

// startPingLoop runs keepalive pings every kiroPingInterval until stop is
// closed. Returns a stop function to call when streaming completes.
func startPingLoop(sw *ginSSEWriter) func() {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(kiroPingInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				sw.writePing()
			}
		}
	}()
	return func() { close(stop) }
}

// credentialFromAccount builds a kiro.KiroCredential from the account's JSONB credentials.
func credentialFromAccount(account *Account) (*kiro.KiroCredential, error) {
	raw, err := json.Marshal(account.Credentials)
	if err != nil {
		return nil, fmt.Errorf("marshal credentials: %w", err)
	}
	var cred kiro.KiroCredential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("unmarshal kiro credential: %w", err)
	}
	// Proxy URL: prefer the account's bound proxy (sub2api proxy pool); fall
	// back to a proxyUrl embedded in the credential JSON. This is the primary
	// per-account egress-IP isolation that prevents multi-account correlation.
	if account.ProxyID != nil && account.Proxy != nil {
		if pu := account.Proxy.URL(); pu != "" {
			cred.ProxyURL = pu
		}
	}
	if cred.RefreshToken == "" {
		return nil, fmt.Errorf("kiro credential missing refreshToken")
	}
	return &cred, nil
}

func (s *KiroGatewayService) accountMutex(accountID int64) *sync.Mutex {
	m, _ := s.tokenMu.LoadOrStore(accountID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// ensureToken makes cred carry a valid access token, refreshing via AWS SSO only
// when necessary. Resolution order (mirrors kiro.rs TokenManager):
//  1. cred already carries a non-expired token (embedded in the account JSON) → use it.
//  2. a cached token for this account is still valid → inject it, skip refresh.
//  3. otherwise lock, double-check the cache, refresh once, cache + apply the result.
//
// This eliminates the ~0.65s OIDC refresh that previously ran on every request.
// tokenSource describes how the access token was obtained, for maintenance logs.
//   "embedded"  - the account JSON already carried a valid token
//   "cache_hit" - reused a still-valid cached token (no OIDC refresh)
//   "refreshed" - performed a real OIDC/Social refresh
//
// ensureToken also guarantees cred.ProfileArn is populated when possible: new
// Kiro 0.12.x endpoints (runtime/management.kiro.dev) mandate profileArn. Social
// refresh returns it; IdC refresh does NOT, so for IdC we fetch it via
// ListAvailableProfiles once and cache it.
func (s *KiroGatewayService) ensureToken(ctx context.Context, account *Account, cred *kiro.KiroCredential) (string, error) {
	source, err := s.acquireToken(ctx, account, cred)
	if err != nil {
		return "", err
	}
	s.ensureProfileArn(ctx, account, cred)
	return source, nil
}

// acquireToken resolves a valid access token (embedded / cache / refresh).
func (s *KiroGatewayService) acquireToken(ctx context.Context, account *Account, cred *kiro.KiroCredential) (string, error) {
	accountID := account.ID
	if cred.AccessToken != "" && !cred.IsExpired() {
		return "embedded", nil
	}
	// Try the shared cache before taking the refresh lock.
	if s.applyCachedToken(accountID, cred) {
		return "cache_hit", nil
	}
	mu := s.accountMutex(accountID)
	mu.Lock()
	defer mu.Unlock()
	// Re-check the cred and cache after acquiring the lock (another goroutine may
	// have refreshed while we waited).
	if cred.AccessToken != "" && !cred.IsExpired() {
		return "embedded", nil
	}
	if s.applyCachedToken(accountID, cred) {
		return "cache_hit", nil
	}
	res, err := kiro.RefreshToken(ctx, cred, s.kiroCfg)
	if err != nil {
		return "", err
	}
	kiro.ApplyTokenResult(cred, res)
	s.storeCachedToken(accountID, cred)
	// Persist the rotated token back to the DB so it survives restarts and is
	// shared across instances (mirrors kam persist_account_refresh). Best-effort.
	s.persistRefreshedCredential(ctx, account, cred)
	return "refreshed", nil
}

// ensureProfileArn guarantees cred.ProfileArn is set when the account needs it.
// Social accounts get profileArn from token refresh, so this is usually a no-op
// for them. IdC (Enterprise SSO) accounts do NOT get profileArn from refresh, so
// we fetch it once via ListAvailableProfiles and cache it. Best-effort: on any
// fetch error we log and leave profileArn empty (the upstream call will surface
// the real "profileArn is required" error if it truly needs it).
func (s *KiroGatewayService) ensureProfileArn(ctx context.Context, account *Account, cred *kiro.KiroCredential) {
	accountID := account.ID
	if cred.ProfileArn != "" {
		return // already have it (embedded, social refresh, or cache)
	}
	if cred.AccessToken == "" {
		return // no token to authenticate the ListAvailableProfiles call
	}
	// Serialize per-account so concurrent requests don't all fetch at once.
	mu := s.accountMutex(accountID)
	mu.Lock()
	defer mu.Unlock()
	// Re-check after acquiring the lock (another goroutine may have filled it).
	if cred.ProfileArn != "" {
		return
	}
	if v, ok := s.tokenCache.Load(accountID); ok {
		if ct, ok := v.(*cachedToken); ok && ct.profileArn != "" {
			cred.ProfileArn = ct.profileArn
			return
		}
	}
	arn, err := kiro.FetchProfileArn(ctx, cred, s.kiroCfg)
	if err != nil {
		logger.FromContext(ctx).Warn("kiro.fetch_profile_arn_failed",
			zap.Int64("account_id", accountID), zap.Error(err))
		return
	}
	cred.ProfileArn = arn
	// Persist into the token cache so later requests reuse it without re-fetching.
	s.storeCachedToken(accountID, cred)
	// Persist the resolved profileArn back to the DB too, so it survives restarts.
	s.persistRefreshedCredential(ctx, account, cred)
	logger.FromContext(ctx).Info("kiro.profile_arn_resolved",
		zap.Int64("account_id", accountID))
}

// applyCachedToken injects a still-valid cached token into cred and returns true
// on a cache hit; false on miss or if the cached token is expired.
func (s *KiroGatewayService) applyCachedToken(accountID int64, cred *kiro.KiroCredential) bool {
	v, ok := s.tokenCache.Load(accountID)
	if !ok {
		return false
	}
	ct := v.(*cachedToken)
	probe := kiro.KiroCredential{AccessToken: ct.accessToken, ExpiresAt: ct.expiresAt}
	if probe.AccessToken == "" || probe.IsExpired() {
		return false
	}
	cred.AccessToken = ct.accessToken
	cred.ExpiresAt = ct.expiresAt
	if ct.profileArn != "" && cred.ProfileArn == "" {
		cred.ProfileArn = ct.profileArn
	}
	return true
}

// storeCachedToken records the freshly refreshed token for reuse by later requests.
func (s *KiroGatewayService) storeCachedToken(accountID int64, cred *kiro.KiroCredential) {
	if cred.AccessToken == "" {
		return
	}
	s.tokenCache.Store(accountID, &cachedToken{
		accessToken: cred.AccessToken,
		expiresAt:   cred.ExpiresAt,
		profileArn:  cred.ProfileArn,
	})
}

// invalidateCachedToken drops a cached token (used when the upstream rejects it),
// so the next ensureToken/forceRefresh performs a real refresh.
func (s *KiroGatewayService) invalidateCachedToken(accountID int64) {
	s.tokenCache.Delete(accountID)
}

// Forward converts the Anthropic request to Kiro, sends it, and streams the SSE response.
func (s *KiroGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
	start := time.Now()
	reqLog := logger.FromContext(ctx)

	cred, err := credentialFromAccount(account)
	if err != nil {
		return nil, fmt.Errorf("kiro credential: %w", err)
	}

	// Parse the Anthropic request.
	var areq kiro.AnthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}

	// Maintenance log: request entry snapshot (no token/body content).
	reqLog.Info("kiro.request_start",
		zap.Int64("account_id", account.ID),
		zap.String("model", areq.Model),
		zap.Bool("stream", areq.Stream),
		zap.Int("body_size", len(body)),
		zap.Int("messages", len(areq.Messages)),
		zap.Bool("web_search", kiro.HasWebSearchTool(&areq)),
		zap.Bool("has_thinking", areq.Thinking != nil),
		zap.Int("tools", len(areq.Tools)),
	)
	if kiroDebugBody() {
		reqLog.Debug("kiro.request_body_raw", zap.Int64("account_id", account.ID), zap.ByteString("body", body))
	}

	// Refresh token if needed.
	tokenStart := time.Now()
	tokenSource, err := s.ensureToken(ctx, account, cred)
	if err != nil {
		if kiro.IsRefreshTokenInvalid(err) {
			reqLog.Warn("kiro.token_refresh_invalid", zap.Int64("account_id", account.ID), zap.Error(err))
			return nil, fmt.Errorf("kiro refresh token invalid: %w", err)
		}
		reqLog.Warn("kiro.token_refresh_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return nil, fmt.Errorf("kiro token refresh: %w", err)
	}
	reqLog.Debug("kiro.token",
		zap.Int64("account_id", account.ID),
		zap.String("source", tokenSource),
		zap.Bool("oidc_refresh", tokenSource == "refreshed"),
		zap.Int64("ms", time.Since(tokenStart).Milliseconds()),
	)

	// WebSearch: a pure web_search tool request is handled via the Kiro MCP
	// endpoint and a synthesized Anthropic SSE stream.
	if kiro.HasWebSearchTool(&areq) {
		flusher, _ := c.Writer.(http.Flusher)
		sw := &ginSSEWriter{c: c, flusher: flusher, startedAt: start}
		inputTokens := kiro.CountInputTokens(body)
		if err := kiro.HandleWebSearch(ctx, sw, cred, s.kiroCfg, &areq, inputTokens); err != nil {
			reqLog.Warn("kiro.websearch_error", zap.Error(err))
			if !sw.wroteHeader {
				return nil, fmt.Errorf("kiro websearch: %w", err)
			}
		}
		return &ForwardResult{
			RequestID: "msg_" + uuid.NewString(),
			Model:     areq.Model,
			Stream:    true,
			Duration:  time.Since(start),
			Usage:     ClaudeUsage{InputTokens: inputTokens, OutputTokens: 1},
		}, nil
	}

	// Apply account-level model mapping (whitelist/mapping configured on the
	// account's credentials.model_mapping) before converting. This lets the
	// edit-page "model restriction" settings take effect for Kiro accounts.
	if mapped := account.GetMappedModel(areq.Model); mapped != "" && mapped != areq.Model {
		reqLog.Info("kiro.model_mapped", zap.String("from", areq.Model), zap.String("to", mapped))
		areq.Model = mapped
	}

	// Convert request (V2 returns tool-name map for response restoration).
	kiroBody, modelID, toolNameMap, err := kiro.ConvertRequestV2(&areq)
	if err != nil {
		return nil, fmt.Errorf("convert request: %w", err)
	}

	// Inject profileArn into root if present.
	if cred.ProfileArn != "" {
		var m map[string]interface{}
		if err := json.Unmarshal(kiroBody, &m); err == nil {
			m["profileArn"] = cred.ProfileArn
			if b, err := kiro.MarshalNoEscape(m); err == nil {
				kiroBody = b
			}
		}
	}

	ep := kiro.NewEndpoint(s.kiroCfg)
	url := ep.APIURL(cred)

	// Maintenance log: the built Kiro request shape (no body content).
	reqLog.Info("kiro.request_built",
		zap.Int64("account_id", account.ID),
		zap.String("model_id", modelID),
		zap.String("agent_mode", s.kiroCfg.EffectiveAgentMode()),
		zap.Int("kiro_body_size", len(kiroBody)),
		zap.String("region", cred.EffectiveAPIRegion()),
		zap.String("url", url),
		zap.Bool("has_thinking_tag", bytes.Contains(kiroBody, []byte("<thinking_mode>"))),
		zap.Bool("has_profile_arn", cred.ProfileArn != ""),
		zap.Bool("via_proxy", cred.ProxyURL != ""),
	)
	if kiroDebugBody() {
		reqLog.Debug("kiro.kiro_body_raw", zap.Int64("account_id", account.ID), zap.ByteString("body", kiroBody))
	}

	client, err := kiro.BuildHTTPClientExported(cred.ProxyURL, 720*time.Second)
	if err != nil {
		return nil, fmt.Errorf("build http client: %w", err)
	}

	// doOnce sends a single upstream attempt.
	doOnce := func() (*http.Response, error) {
		httpReq, e := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(kiroBody))
		if e != nil {
			return nil, e
		}
		ep.DecorateRequest(httpReq, cred, cred.AccessToken)
		return client.Do(httpReq)
	}

	// doUpstream wraps doOnce with in-place retry for transient upstream errors,
	// mirroring Kiro IDE's conversation retry strategy (AdaptiveRetryStrategy,
	// maxAttempts=3). 5xx/408 retry the SAME account with a fast backoff. 429
	// (ThrottlingException) ALSO retries the same account, but with the slower
	// throttle backoff (500ms base ×5, cap 20s, +jitter) the IDE uses — a 429 is
	// traffic protection ("slow down"), so waiting and retrying the same
	// credential is correct; the jittered backoff also staggers concurrent
	// requests pinned to one account instead of stampeding a failover. Only when
	// the in-place retries are exhausted does the caller fail over to another
	// credential in the group (a multi-account safety net the IDE lacks).
	doUpstream := func() (*http.Response, error) {
		var resp *http.Response
		var e error
		for attempt := 0; attempt < kiro.MaxTransientRetries; attempt++ {
			resp, e = doOnce()
			if e != nil {
				// network error: also transient, retry with backoff
				if attempt+1 < kiro.MaxTransientRetries {
					reqLog.Warn("kiro.upstream_network_retry", zap.Int("attempt", attempt+1), zap.Error(e))
					time.Sleep(kiro.RetryDelay(attempt))
					continue
				}
				return nil, e
			}
			// Non-retryable status: return immediately for the caller to classify.
			if !kiro.IsInPlaceRetryStatus(resp.StatusCode) {
				return resp, nil
			}
			// in-place retryable HTTP status (429/5xx/408): drain+close body, back
			// off (throttle backoff for 429, fast backoff for 5xx/408), retry.
			respStatus := resp.StatusCode
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			throttled := kiro.IsThrottleStatus(respStatus)
			reqLog.Warn("kiro.upstream_transient_retry",
				zap.Int("status", respStatus),
				zap.Bool("throttled", throttled),
				zap.Int("attempt", attempt+1),
				zap.Int("max", kiro.MaxTransientRetries),
				zap.Int64("account_id", account.ID))
			if attempt+1 < kiro.MaxTransientRetries {
				if throttled {
					time.Sleep(kiro.ThrottleRetryDelay(attempt))
				} else {
					time.Sleep(kiro.RetryDelay(attempt))
				}
				continue
			}
			// retries exhausted: re-issue one final request so the caller gets a
			// live response/body to classify and surface.
			return doOnce()
		}
		return resp, e
	}

	resp, err := doUpstream()
	if err != nil {
		reqLog.Warn("kiro.upstream_failed",
			zap.Int64("account_id", account.ID),
			zap.Int64("elapsed_ms", time.Since(start).Milliseconds()),
			zap.Error(err))
		return nil, fmt.Errorf("kiro upstream request: %w", err)
	}

	reqLog.Info("kiro.upstream_response",
		zap.Int64("account_id", account.ID),
		zap.Int("status", resp.StatusCode),
		zap.Int64("elapsed_ms", time.Since(start).Milliseconds()))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		resp.Body.Close()
		errBody := buf.Bytes()

		// Bearer token invalid: force-refresh the access token and retry once on
		// the SAME account (mirrors kiro.rs is_bearer_token_invalid handling).
		if kiro.IsBearerTokenInvalid(errBody) {
			reqLog.Warn("kiro.bearer_token_invalid_retry", zap.Int64("account_id", account.ID))
			if rerr := s.forceRefresh(ctx, account, cred); rerr == nil {
				resp, err = doUpstream()
				if err != nil {
					return nil, fmt.Errorf("kiro upstream request (after refresh): %w", err)
				}
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					buf2 := new(bytes.Buffer)
					_, _ = buf2.ReadFrom(resp.Body)
					resp.Body.Close()
					return nil, s.classifyUpstreamError(c, reqLog, account, resp.StatusCode, buf2.Bytes())
				}
				// success after refresh — fall through with new resp
			} else {
				return nil, s.classifyUpstreamError(c, reqLog, account, resp.StatusCode, errBody)
			}
		} else {
			return nil, s.classifyUpstreamError(c, reqLog, account, resp.StatusCode, errBody)
		}
	}
	defer resp.Body.Close()

	messageID := "msg_" + uuid.NewString()
	thinking := kiro.IsThinkingModel(areq.Model) || areq.Thinking != nil

	// Non-streaming: aggregate the upstream event stream into a single
	// Anthropic message JSON (mirrors kiro.rs handle_non_stream_request).
	if !areq.Stream {
		agg := kiro.NewAggregatingWriter()
		sc := kiro.NewStreamConverter(agg, modelID, messageID, thinking)
		sc.SetToolNameMap(toolNameMap)
		if err := sc.Run(resp.Body); err != nil {
			return nil, fmt.Errorf("kiro stream parse: %w", err)
		}
		respJSON := agg.BuildResponse(messageID, areq.Model, sc.OutputTokens())
		c.JSON(http.StatusOK, respJSON)

		result := &ForwardResult{
			RequestID: messageID,
			Model:     areq.Model,
			Stream:    false,
			Duration:  time.Since(start),
			Usage: ClaudeUsage{
				InputTokens:  sc.InputTokens(),
				OutputTokens: sc.OutputTokens(),
			},
		}
		if modelID != areq.Model {
			result.UpstreamModel = modelID
		}
		return result, nil
	}

	flusher, _ := c.Writer.(http.Flusher)
	sw := &ginSSEWriter{c: c, flusher: flusher, startedAt: start}

	sc := kiro.NewStreamConverter(sw, modelID, messageID, thinking)
	sc.SetToolNameMap(toolNameMap)
	stopPing := startPingLoop(sw)
	defer stopPing()
	if err := sc.Run(resp.Body); err != nil {
		// If we already started writing, the stream is corrupted; just log.
		reqLog.Warn("kiro.stream_parse_error", zap.Error(err))
		if !sw.wroteHeader {
			return nil, fmt.Errorf("kiro stream parse: %w", err)
		}
	}

	result := &ForwardResult{
		RequestID: messageID,
		Model:     areq.Model,
		Stream:    true,
		Duration:  time.Since(start),
		Usage: ClaudeUsage{
			InputTokens:  sc.InputTokens(),
			OutputTokens: sc.OutputTokens(),
		},
	}
	if modelID != areq.Model {
		result.UpstreamModel = modelID
	}
	if sw.firstTokenAt != nil {
		ms := int(sw.firstTokenAt.Sub(start).Milliseconds())
		result.FirstTokenMs = &ms
	}

	// Maintenance log: request completion summary.
	ftMs := -1
	if result.FirstTokenMs != nil {
		ftMs = *result.FirstTokenMs
	}
	reqLog.Info("kiro.request_done",
		zap.Int64("account_id", account.ID),
		zap.String("model_id", modelID),
		zap.Int("first_token_ms", ftMs),
		zap.Int64("total_ms", time.Since(start).Milliseconds()),
		zap.Int("input_tokens", sc.InputTokens()),
		zap.Int("output_tokens", sc.OutputTokens()),
		zap.Bool("client_disconnect", result.ClientDisconnect),
	)

	return result, nil
}

// ---- Account connection test support ----

// testEventWriter adapts kiro gateway SSE events into the account-test
// TestEvent stream shape (type=content with text).
type testEventWriter struct {
	c       *gin.Context
	gotText bool
}

func (w *testEventWriter) emit(ev map[string]interface{}) {
	payload, _ := json.Marshal(ev)
	fmt.Fprintf(w.c.Writer, "data: %s\n\n", payload)
	if f, ok := w.c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *testEventWriter) WriteSSE(event string, data map[string]interface{}) error {
	if event == "content_block_delta" {
		if d, ok := data["delta"].(map[string]interface{}); ok {
			if t, ok := d["text"].(string); ok && t != "" {
				w.gotText = true
				w.emit(map[string]interface{}{"type": "content", "text": t})
			}
		}
	}
	return nil
}

// TestConnection runs a minimal request against the Kiro backend and streams
// account-test SSE events (test_start / content / test_complete / error).
func (s *KiroGatewayService) TestConnection(c *gin.Context, account *Account, modelID string) error {
	ctx := c.Request.Context()

	cred, err := credentialFromAccount(account)
	if err != nil {
		return s.sendTestError(c, "Invalid Kiro credential: "+err.Error(), false)
	}
	if modelID == "" {
		modelID = "claude-opus-4-7"
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()

	w := &testEventWriter{c: c}
	w.emit(map[string]interface{}{"type": "test_start", "model": modelID})

	if _, err := s.ensureToken(ctx, account, cred); err != nil {
		return s.sendTestError(c, "Token refresh failed: "+err.Error(), true)
	}

	reqBody := map[string]interface{}{
		"model":      modelID,
		"max_tokens": 32,
		"stream":     true,
		"messages":   []map[string]interface{}{{"role": "user", "content": "Hi"}},
	}
	bodyBytes, _ := json.Marshal(reqBody)

	var kreq kiro.AnthropicRequest
	if err := json.Unmarshal(bodyBytes, &kreq); err != nil {
		return s.sendTestError(c, "build request failed: "+err.Error(), true)
	}
	kiroBody, mappedModel, tcToolNameMap, err := kiro.ConvertRequestV2(&kreq)
	if err != nil {
		return s.sendTestError(c, "convert request failed: "+err.Error(), true)
	}
	if cred.ProfileArn != "" {
		var m map[string]interface{}
		if json.Unmarshal(kiroBody, &m) == nil {
			m["profileArn"] = cred.ProfileArn
			if b, e := kiro.MarshalNoEscape(m); e == nil {
				kiroBody = b
			}
		}
	}

	ep := kiro.NewEndpoint(s.kiroCfg)
	client, err := kiro.BuildHTTPClientExported(cred.ProxyURL, 120*time.Second)
	if err != nil {
		return s.sendTestError(c, "http client failed: "+err.Error(), true)
	}
	// Send with in-place transient retry (429/408/5xx), same as Forward, so a
	// transient upstream throttle does not surface as a test failure.
	var resp *http.Response
	for attempt := 0; attempt < kiro.MaxTransientRetries; attempt++ {
		httpReq, herr := http.NewRequestWithContext(ctx, http.MethodPost, ep.APIURL(cred), bytes.NewReader(kiroBody))
		if herr != nil {
			return s.sendTestError(c, herr.Error(), true)
		}
		ep.DecorateRequest(httpReq, cred, cred.AccessToken)
		var derr error
		resp, derr = client.Do(httpReq)
		if derr != nil {
			if attempt+1 < kiro.MaxTransientRetries {
				time.Sleep(kiro.RetryDelay(attempt))
				continue
			}
			return s.sendTestError(c, "upstream request failed: "+derr.Error(), true)
		}
		if !kiro.IsTransientStatus(resp.StatusCode) {
			break
		}
		if attempt+1 < kiro.MaxTransientRetries {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			time.Sleep(kiro.RetryDelay(attempt))
			continue
		}
		// last attempt kept transient status; fall through to error reporting
		break
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(io.LimitReader(resp.Body, 2048))
		return s.sendTestError(c, fmt.Sprintf("API returned %d: %s", resp.StatusCode, kiro.SanitizeError(buf.String())), true)
	}

	sc := kiro.NewStreamConverter(w, mappedModel, "msg_test", kiro.IsThinkingModel(modelID))
	sc.SetToolNameMap(tcToolNameMap)
	if err := sc.Run(resp.Body); err != nil {
		return s.sendTestError(c, "stream parse failed: "+err.Error(), true)
	}

	w.emit(map[string]interface{}{"type": "test_complete", "success": true})
	return nil
}

func (s *KiroGatewayService) sendTestError(c *gin.Context, msg string, headerWritten bool) error {
	if !headerWritten {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
	}
	payload, _ := json.Marshal(map[string]interface{}{"type": "error", "error": msg})
	fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
	return fmt.Errorf("%s", msg)
}


// forceRefresh unconditionally refreshes the credential's access token,
// bypassing the not-expired check (used when upstream rejects the token).
func (s *KiroGatewayService) forceRefresh(ctx context.Context, account *Account, cred *kiro.KiroCredential) error {
	accountID := account.ID
	mu := s.accountMutex(accountID)
	mu.Lock()
	defer mu.Unlock()
	// The current token was rejected by the upstream — drop it from the cache so
	// no concurrent request keeps reusing the bad token.
	s.invalidateCachedToken(accountID)
	res, err := kiro.RefreshToken(ctx, cred, s.kiroCfg)
	if err != nil {
		return err
	}
	kiro.ApplyTokenResult(cred, res)
	s.storeCachedToken(accountID, cred)
	// Persist the rotated token back to the DB (best-effort).
	s.persistRefreshedCredential(ctx, account, cred)
	return nil
}

// persistRefreshedCredential writes the freshly refreshed token fields back into
// the account's Credentials JSONB so they survive a restart and are shared
// across instances. Best-effort: a write failure is logged but never blocks the
// request (the in-memory token cache already carries the fresh values). Mirrors
// kam persist_account_refresh.
//
// Only the fields that a refresh can rotate are touched (accessToken,
// refreshToken, expiresAt, profileArn, machineId); all other credential keys
// (clientId/clientSecret/region/authMethod/proxyUrl/...) are preserved.
func (s *KiroGatewayService) persistRefreshedCredential(ctx context.Context, account *Account, cred *kiro.KiroCredential) {
	if s.accountRepo == nil || account == nil {
		return
	}
	creds := cloneCredentials(account.Credentials)
	if cred.AccessToken != "" {
		creds["accessToken"] = cred.AccessToken
	}
	if cred.RefreshToken != "" {
		creds["refreshToken"] = cred.RefreshToken
	}
	if cred.ExpiresAt != "" {
		creds["expiresAt"] = cred.ExpiresAt
	}
	if cred.ProfileArn != "" {
		creds["profileArn"] = cred.ProfileArn
	}
	if cred.MachineID != "" {
		creds["machineId"] = cred.MachineID
	}
	pctx, cancel := detachedRepoCtx(ctx)
	defer cancel()
	if err := persistAccountCredentials(pctx, s.accountRepo, account, creds); err != nil {
		logger.FromContext(ctx).Warn("kiro.persist_credential_failed",
			zap.Int64("account_id", account.ID), zap.Error(err))
		return
	}
	logger.FromContext(ctx).Debug("kiro.credential_persisted",
		zap.Int64("account_id", account.ID))
}

// classifyUpstreamError maps a Kiro upstream error into the right outcome.
//   - context/input too long: a client error that no account can satisfy, so we
//     write a friendly 400 directly (mirrors kiro.rs map_provider_error) and
//     return a non-failover error so the handler does NOT switch accounts.
//   - monthly quota exhausted: logged distinctly; surfaced as a failover error.
//   - everything else: a failover error preserving the upstream body.
func (s *KiroGatewayService) classifyUpstreamError(c *gin.Context, reqLog *zap.Logger, account *Account, status int, body []byte) error {
	// Maintenance log: surface the upstream error's reason/message (truncated, no
	// full body) so post-mortems can see e.g. SERVICE_REQUEST_RATE_EXCEEDED,
	// MONTHLY_REQUEST_COUNT, ThrottlingException without enabling debug bodies.
	reqLog.Warn("kiro.upstream_error_detail",
		zap.Int64("account_id", account.ID),
		zap.Int("status", status),
		zap.String("body_snippet", kiro.SanitizeError(truncateForLog(body, 300))),
	)
	if isCtxErr, friendly := kiro.IsContextLengthError(body); isCtxErr {
		reqLog.Warn("kiro.context_length_exceeded", zap.Int64("account_id", account.ID), zap.Int("status", status))
		if c != nil && !c.Writer.Written() {
			c.JSON(http.StatusBadRequest, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": friendly,
				},
			})
		}
		// Non-failover sentinel: response already written, do not switch accounts.
		return fmt.Errorf("kiro context length exceeded (handled): %s", friendly)
	}
	if kiro.IsMonthlyRequestLimit(body) {
		reqLog.Warn("kiro.monthly_request_limit", zap.Int64("account_id", account.ID), zap.Int("status", status))
		s.markMonthlyQuotaExhausted(c, account.ID)
	} else if kiro.IsAccountSuspended(status, body) {
		reqLog.Warn("kiro.account_suspended", zap.Int64("account_id", account.ID), zap.Int("status", status))
		s.markAccountSuspended(c, account.ID)
	} else if kiro.IsRateLimitError(status, body) {
		reqLog.Warn("kiro.account_rate_limited", zap.Int64("account_id", account.ID), zap.Int("status", status))
		s.markAccountRateLimited(c, account.ID)
	} else {
		reqLog.Warn("kiro.upstream_error", zap.Int("status", status), zap.Int64("account_id", account.ID))
	}
	return &UpstreamFailoverError{
		StatusCode:   status,
		ResponseBody: body,
	}
}

// --- Account health marking (mirrors kam load_balancer mark_rate_limited /
// mark_account_banned, but persisted to the DB so it survives restarts and is
// shared across instances). All are best-effort: a write failure is logged and
// the request still fails over.

// kiroRateLimitCooldown returns the cooldown for a transient 429/throttle.
// Default 5s: Kiro 429 is traffic protection that recovers in seconds, so a
// short cooldown lets the throttled credential return to the pool quickly while
// the request immediately fails over to another credential. env-overridable
// via KIRO_RATE_LIMIT_COOLDOWN_SEC.
func kiroRateLimitCooldown() time.Duration {
	return envDurationSeconds("KIRO_RATE_LIMIT_COOLDOWN_SEC", 5)
}

// kiroSuspendCooldown returns the cooldown for an account flagged suspended.
// Default 1h (kam uses 60s in-memory; we use longer since it persists and a
// suspended account won't recover within a minute), env-overridable.
func kiroSuspendCooldown() time.Duration {
	return envDurationSeconds("KIRO_SUSPEND_COOLDOWN_SEC", 3600)
}

// kiroMonthlyQuotaCooldown returns the cooldown for a monthly-quota-exhausted
// account. Default 6h (avoids hammering an account that won't reset until the
// billing cycle rolls over), env-overridable.
func kiroMonthlyQuotaCooldown() time.Duration {
	return envDurationSeconds("KIRO_MONTHLY_QUOTA_COOLDOWN_SEC", 6*3600)
}

func envDurationSeconds(key string, defSec int64) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return time.Duration(defSec) * time.Second
}

func (s *KiroGatewayService) markAccountRateLimited(c *gin.Context, accountID int64) {
	if s.accountRepo == nil {
		return
	}
	ctx, cancel := repoCtx(c)
	defer cancel()
	resetAt := time.Now().Add(kiroRateLimitCooldown())
	if err := s.accountRepo.SetRateLimited(ctx, accountID, resetAt); err != nil {
		logger.FromContext(ctx).Warn("kiro.mark_rate_limited_failed",
			zap.Int64("account_id", accountID), zap.Error(err))
	}
}

func (s *KiroGatewayService) markAccountSuspended(c *gin.Context, accountID int64) {
	if s.accountRepo == nil {
		return
	}
	ctx, cancel := repoCtx(c)
	defer cancel()
	until := time.Now().Add(kiroSuspendCooldown())
	reason := "kiro account suspended (AccessDenied/suspended; auto temp-unschedule)"
	if err := s.accountRepo.SetTempUnschedulable(ctx, accountID, until, reason); err != nil {
		logger.FromContext(ctx).Warn("kiro.mark_suspended_failed",
			zap.Int64("account_id", accountID), zap.Error(err))
	}
}

func (s *KiroGatewayService) markMonthlyQuotaExhausted(c *gin.Context, accountID int64) {
	if s.accountRepo == nil {
		return
	}
	ctx, cancel := repoCtx(c)
	defer cancel()
	until := time.Now().Add(kiroMonthlyQuotaCooldown())
	reason := "kiro monthly request quota exhausted (auto temp-unschedule)"
	if err := s.accountRepo.SetTempUnschedulable(ctx, accountID, until, reason); err != nil {
		logger.FromContext(ctx).Warn("kiro.mark_monthly_quota_failed",
			zap.Int64("account_id", accountID), zap.Error(err))
	}
}

// repoCtx derives a context for repo writes that must outlive the request: the
// gin request context may already be canceled (client disconnect) when we mark
// an account, so detach from cancelation while keeping a short deadline. The
// caller MUST defer the returned cancel func.
func repoCtx(c *gin.Context) (context.Context, context.CancelFunc) {
	var base context.Context = context.Background()
	if c != nil {
		base = c.Request.Context()
	}
	return detachedRepoCtx(base)
}

// detachedRepoCtx returns a fresh context that carries over the request logger
// but is detached from the request's cancelation, with a short write deadline.
func detachedRepoCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	base := logger.IntoContext(context.Background(), logger.FromContext(ctx))
	return context.WithTimeout(base, 5*time.Second)
}