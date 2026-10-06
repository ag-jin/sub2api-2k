package bigmodel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ResetStatusEndpoint is the read-only reset-card status endpoint of the ZCode
// relay. It is the only reset-card URL this package knows: the card-consuming
// execution endpoint is deliberately absent (R0: cards are a user asset, this
// system only observes them, never consumes them).
const ResetStatusEndpoint = "https://zcode.z.ai/api/v1/coding-plan/reset/status"

// resetTargetTypePersonal selects the personal plan, the only target type this
// system manages (TEAM is P4, design M4).
const resetTargetTypePersonal = "PERSONAL"

// Wire header names of the reset-card endpoint. The zcodejwt token travels in
// Authorization as-is (no "Bearer" scheme) and the bigmodel access token in its
// own header; sending either one in the other's place is rejected upstream.
const (
	headerBigmodelAuthorization = "X-Bigmodel-Authorization"
	headerBigmodelTargetType    = "Bigmodel-Target-Type"
)

// opResetStatus names this call in classified errors.
const opResetStatus = "reset_status"

// ResetCardType identifies which pool a reset card belongs to.
type ResetCardType string

const (
	// ResetCardTypeFiveHour is the 5-hour window pool ("available_five_hour_resets").
	ResetCardTypeFiveHour ResetCardType = "five_hour"
	// ResetCardTypeWeek is the weekly pool ("available_week_resets").
	ResetCardTypeWeek ResetCardType = "week"
)

// ResetCard is one read-only reset card. Execution fields of the upstream
// payload (card ids) are deliberately not parsed: R0 forbids consuming cards.
type ResetCard struct {
	Type ResetCardType
	// ExpireAt is the card expiry in UTC, zero when upstream reported none.
	ExpireAt time.Time
}

// ResetStatus is the parsed reset-card status of one account. Treat it as
// read-only: cache hits hand the same value to every caller.
type ResetStatus struct {
	FiveHourCards []ResetCard
	WeekCards     []ResetCard
	// HasUnreadHistory mirrors upstream "has_unread_history": a reset already
	// happened that the account owner has not seen yet.
	HasUnreadHistory bool
}

// ResetStatusReader reads reset-card status through the shared risk client.
// Results are cached per credential for cacheTTL (the ticket's
// reset_status_cache_minutes); a non-positive TTL disables caching. The reader
// has no cache-invalidation entry point because this system never changes a
// reset card: the upstream state can only move forward on its own (R0).
type ResetStatusReader struct {
	risk     *RiskClient
	cacheTTL time.Duration
	now      func() time.Time

	flight singleflight.Group

	// callMu serializes the upstream section of a read. The shared risk client
	// coalesces concurrent calls per endpoint key, and that key is endpoint-wide
	// while a reset-status read is credential-scoped: without this guard two
	// accounts reading at the same moment would be merged into a single upstream
	// request and one of them would receive the other account's cards. The risk
	// client's minimum call interval already limits this endpoint to one call at
	// a time, so serializing costs nothing in production.
	callMu sync.Mutex

	mu    sync.Mutex
	cache map[string]cachedResetStatus
}

// cachedResetStatus is one TTL cache entry.
type cachedResetStatus struct {
	status    *ResetStatus
	expiresAt time.Time
}

// NewResetStatusReader builds a reader over the shared risk client. risk is
// required: reset status is a zcode.z.ai call and must never bypass the risk
// gate (design M4).
func NewResetStatusReader(risk *RiskClient, cacheTTL time.Duration) *ResetStatusReader {
	return newResetStatusReader(risk, cacheTTL, time.Now)
}

// newResetStatusReader is the clock-injectable constructor used by tests.
func newResetStatusReader(risk *RiskClient, cacheTTL time.Duration, now func() time.Time) *ResetStatusReader {
	if now == nil {
		now = time.Now
	}
	return &ResetStatusReader{
		risk:     risk,
		cacheTTL: cacheTTL,
		now:      now,
		cache:    make(map[string]cachedResetStatus),
	}
}

// resetStatusCacheKey derives the cache key from the credential pair. The raw
// tokens are hashed so credentials never sit in the map as keys.
func resetStatusCacheKey(zcodeJWTToken, accessToken string) string {
	sum := sha256.Sum256([]byte(zcodeJWTToken + "\x00" + accessToken))
	return hex.EncodeToString(sum[:])
}

// Reset status wire shapes. History objects are accepted but not surfaced.
type resetStatusResponse struct {
	Code int              `json:"code"`
	Msg  string           `json:"msg"`
	Data *resetStatusData `json:"data"`
}

type resetStatusData struct {
	AvailableFiveHourResets []resetStatusCard `json:"available_five_hour_resets"`
	AvailableWeekResets     []resetStatusCard `json:"available_week_resets"`
	HasUnreadHistory        bool              `json:"has_unread_history"`
}

type resetStatusCard struct {
	// ExpireAtMS is epoch milliseconds. float64 tolerates the exponent form
	// some JSON encoders emit for large integers.
	ExpireAtMS float64 `json:"expire_at"`
}

// Fetch returns the account's reset-card status, served from the TTL cache when
// a fresh entry exists. It is read-only: the request is a GET and no other
// reset-card call exists in this package (R0).
func (r *ResetStatusReader) Fetch(ctx context.Context, zcodeJWTToken, accessToken string) (*ResetStatus, error) {
	if r == nil || r.risk == nil {
		return nil, configError(opResetStatus, errors.New("no risk client injected: zcode.z.ai calls must go through the shared risk client"))
	}
	if strings.TrimSpace(zcodeJWTToken) == "" {
		return nil, invalidInputError(opResetStatus, "zcode_jwt_token")
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, invalidInputError(opResetStatus, "access_token")
	}

	key := resetStatusCacheKey(zcodeJWTToken, accessToken)
	if status, ok := r.cachedStatus(key); ok {
		return status, nil
	}

	value, err, _ := r.flight.Do(key, func() (any, error) {
		if status, ok := r.cachedStatus(key); ok {
			return status, nil
		}
		status, err := r.fetchUncached(ctx, zcodeJWTToken, accessToken)
		if err != nil {
			// Failures are never cached: the next read retries upstream.
			return nil, err
		}
		r.storeStatus(key, status)
		return status, nil
	})
	if err != nil {
		return nil, err
	}
	status, _ := value.(*ResetStatus)
	if status == nil {
		return nil, configError(opResetStatus, errors.New("single-flight returned no reset status"))
	}
	return status, nil
}

// cachedStatus returns a fresh cache entry; an elapsed TTL drops it.
func (r *ResetStatusReader) cachedStatus(key string) (*ResetStatus, bool) {
	if r.cacheTTL <= 0 {
		return nil, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.cache[key]
	if !ok {
		return nil, false
	}
	if !r.now().Before(entry.expiresAt) {
		delete(r.cache, key)
		return nil, false
	}
	return entry.status, true
}

// storeStatus caches a successful read until the TTL elapses.
func (r *ResetStatusReader) storeStatus(key string, status *ResetStatus) {
	if r.cacheTTL <= 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.cache[key] = cachedResetStatus{status: status, expiresAt: r.now().Add(r.cacheTTL)}
}

// fetchUncached issues the GET through the risk client and parses the envelope.
func (r *ResetStatusReader) fetchUncached(ctx context.Context, zcodeJWTToken, accessToken string) (*ResetStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ResetStatusEndpoint, http.NoBody)
	if err != nil {
		return nil, &Error{Op: opResetStatus, Kind: ErrorKindInvalidInput, err: err}
	}
	req.Header.Set("Authorization", zcodeJWTToken)
	req.Header.Set(headerBigmodelAuthorization, accessToken)
	req.Header.Set(headerBigmodelTargetType, resetTargetTypePersonal)

	// One credential-scoped read at a time through the endpoint-wide risk client
	// (see callMu).
	r.callMu.Lock()
	defer r.callMu.Unlock()

	resp, err := r.risk.Do(ctx, RiskEndpointResetStatus, req)
	if err != nil {
		return nil, transportError(opResetStatus, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, transportError(opResetStatus, fmt.Errorf("read response: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Op: opResetStatus, Kind: ErrorKindHTTPStatus, Status: resp.StatusCode}
	}

	var parsed resetStatusResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, &Error{Op: opResetStatus, Kind: ErrorKindInvalidBody, Status: resp.StatusCode, err: err}
	}
	if parsed.Code != 0 {
		return nil, &Error{
			Op:      opResetStatus,
			Kind:    ErrorKindUpstreamCode,
			Status:  resp.StatusCode,
			Code:    parsed.Code,
			Message: truncateMessage(parsed.Msg),
		}
	}
	if parsed.Data == nil {
		return nil, &Error{Op: opResetStatus, Kind: ErrorKindMissingField, Status: resp.StatusCode, Field: "data"}
	}
	return buildResetStatus(parsed.Data), nil
}

// buildResetStatus maps the wire pools onto the typed status. A card without
// expire_at is kept with a zero expiry: the card exists, only its deadline is
// unknown, and dropping it would understate the account's balance. An empty
// pool stays nil so downstream JSON omits the field instead of emitting [].
func buildResetStatus(data *resetStatusData) *ResetStatus {
	status := &ResetStatus{HasUnreadHistory: data.HasUnreadHistory}
	for _, card := range data.AvailableFiveHourResets {
		status.FiveHourCards = append(status.FiveHourCards, newResetCard(ResetCardTypeFiveHour, card))
	}
	for _, card := range data.AvailableWeekResets {
		status.WeekCards = append(status.WeekCards, newResetCard(ResetCardTypeWeek, card))
	}
	return status
}

// newResetCard converts one wire card; expire_at is epoch milliseconds.
func newResetCard(cardType ResetCardType, card resetStatusCard) ResetCard {
	out := ResetCard{Type: cardType}
	if card.ExpireAtMS > 0 {
		out.ExpireAt = time.UnixMilli(int64(card.ExpireAtMS)).UTC()
	}
	return out
}
