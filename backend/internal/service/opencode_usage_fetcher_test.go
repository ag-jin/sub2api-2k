package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/opencode"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

type opencodeUsageTestUpstream struct {
	mu       sync.Mutex
	calls    int
	status   int
	body     string
	wait     bool
	lastAuth string
	lastURL  string
}

func (u *opencodeUsageTestUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls++
	u.lastAuth = req.Header.Get("Authorization")
	u.lastURL = req.URL.String()
	wait := u.wait
	status := u.status
	body := u.body
	u.mu.Unlock()
	if wait {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func (u *opencodeUsageTestUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func (u *opencodeUsageTestUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func newOpenCodeUsageTestService(upstream HTTPUpstream, account *Account) *AccountUsageService {
	return &AccountUsageService{
		accountRepo:     &stubOpenAIAccountRepo{accounts: []Account{*account}},
		cache:           NewUsageCache(),
		opencodeFetcher: NewOpenCodeUsageFetcher(upstream),
	}
}

func openCodeUsageTestAccount(id int64) *Account {
	return &Account{ID: id, Platform: PlatformOpenCode, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "sk-test-placeholder",
	}}
}

const openCodeUsageTestBody = `{"usage":{"rolling":{"status":"ok","percent":12,"resetsAt":"2030-01-01T00:00:00Z"},"weekly":{"status":"ok","percent":34,"resetsAt":"2030-01-02T00:00:00Z"},"monthly":{"status":"rate-limited","percent":80,"resetsAt":"2030-02-01T00:00:00Z"}}}`

func TestOpenCodeUsageFetcherValidatesURLBeforeAuthorization(t *testing.T) {
	upstream := &opencodeUsageTestUpstream{}
	fetcher := NewOpenCodeUsageFetcher(upstream)
	_, err := fetcher.FetchUsage(context.Background(), &OpenCodeUsageFetchOptions{
		APIKey:       "sk-test-placeholder",
		BaseURL:      "http://127.0.0.1:8080?credential=secret",
		HTTPUpstream: upstream,
	})
	if err == nil {
		t.Fatal("expected invalid base URL error")
	}
	if upstream.count() != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstream.count())
	}
}

func TestOpenCodeUsageForceHonorsNegativeCache(t *testing.T) {
	upstream := &opencodeUsageTestUpstream{status: http.StatusUnauthorized, body: `{"error":"secret upstream body"}`}
	account := openCodeUsageTestAccount(901)
	svc := newOpenCodeUsageTestService(upstream, account)

	first, err := svc.getOpenCodeUsage(context.Background(), account, false)
	if err != nil || first.Opencode == nil || first.Opencode.Error != "unauthenticated" {
		t.Fatalf("first usage = %#v, err = %v", first, err)
	}
	second, err := svc.getOpenCodeUsage(context.Background(), account, true)
	if err != nil || second.Opencode.Error != "unauthenticated" {
		t.Fatalf("forced usage = %#v, err = %v", second, err)
	}
	if got := upstream.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestOpenCodeUsageRefreshStoresNegativeAndRetainsStaleSuccess(t *testing.T) {
	upstream := &opencodeUsageTestUpstream{body: openCodeUsageTestBody}
	account := openCodeUsageTestAccount(902)
	svc := newOpenCodeUsageTestService(upstream, account)

	fresh, err := svc.getOpenCodeUsage(context.Background(), account, false)
	if err != nil || fresh.Opencode.Rolling.Percent != 12 {
		t.Fatalf("fresh usage = %#v, err = %v", fresh, err)
	}
	upstream.mu.Lock()
	upstream.status = http.StatusServiceUnavailable
	upstream.body = `{"message":"do not expose"}`
	upstream.mu.Unlock()

	stale, err := svc.getOpenCodeUsage(context.Background(), account, true)
	if err != nil || stale.Opencode == nil || !stale.Opencode.Stale || stale.Opencode.Status != "stale" {
		t.Fatalf("stale usage = %#v, err = %v", stale, err)
	}
	if stale.Opencode.Rolling.Percent != 12 || stale.Opencode.Error != "upstream_error" {
		t.Fatalf("stale snapshot = %#v", stale.Opencode)
	}
	cached, err := svc.getOpenCodeUsage(context.Background(), account, true)
	if err != nil || cached.Opencode.Error != "upstream_error" {
		t.Fatalf("negative cached usage = %#v, err = %v", cached, err)
	}
	if got := upstream.count(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
}

func TestOpenCodeUsagePreservesCallerCancellation(t *testing.T) {
	upstream := &opencodeUsageTestUpstream{wait: true}
	account := openCodeUsageTestAccount(903)
	svc := newOpenCodeUsageTestService(upstream, account)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	usage, err := svc.getOpenCodeUsage(ctx, account, true)
	if err != nil || usage == nil || usage.Opencode.Error != "network_error" {
		t.Fatalf("cancelled usage = %#v, err = %v", usage, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("caller cancellation was not preserved")
	}
}

func TestOpenCodeUsageWindowParser(t *testing.T) {
	snapshot, err := opencode.ParseUsageResponse([]byte(openCodeUsageTestBody))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Rolling.Percent != 12 || snapshot.Monthly.Status != opencode.UsageStatusRateLimited {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}
