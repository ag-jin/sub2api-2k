//go:build unit

package service

// 智谱登录态账号的 credit-usage 探针测试（票 08 / design M4）。
//
// 协议事实全部逐字对齐 zcode-150-research FINAL-REPORT §六 与实测响应快照
// （zcode-research/credit-usage-detail.json）：GET open.bigmodel.cn/api/monitor/
// credit-usage/usage-detail?type=1&startTime&endTime&usageType=MODEL，
// Authorization=<access_token>（无 Bearer），响应 code=200（注意不是 0）。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/bigmodel"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// zhipuCreditUsageSnapshotBody 是实测响应的逐字快照（截取 modelUsage 段，
// 保留 7 日数组与两模型；数值未做任何改动）。
const zhipuCreditUsageSnapshotBody = `{
  "code": 200,
  "msg": "操作成功",
  "data": {
    "granularity": "DAY",
    "timezone": "Asia/Shanghai",
    "modelUsage": {
      "totalUsage": {"totalTokens": 1072277951, "totalCredits": "103619.0861"},
      "xTime": ["2026-09-30","2026-10-01","2026-10-02","2026-10-03","2026-10-04","2026-10-05","2026-10-06"],
      "modelDataList": [
        {
          "modelCode": "glm-5.3",
          "modelName": "GLM-5.3",
          "sortOrder": 1,
          "uncachedInputTokensUsage": [0, 0, 0, 0, 3073651, 13109593, 24144636],
          "cachedInputTokensUsage": [0, 0, 0, 0, 147645184, 398128640, 484083584],
          "inputTokensUsage": [0, 0, 0, 0, 150718835, 411238233, 508228220],
          "outputTokensUsage": [0, 0, 0, 0, 424337, 916671, 747348],
          "totalTokensUsage": [0, 0, 0, 0, 151143172, 412154904, 508975568],
          "uncachedInputCreditsUsage": ["0.0000","0.0000","0.0000","0.0000","1060.4096","4407.2429","7995.8069"],
          "cachedInputCreditsUsage": ["0.0000","0.0000","0.0000","0.0000","12549.8406","33869.4074","41229.4173"],
          "inputCreditsUsage": ["0.0000","0.0000","0.0000","0.0000","13610.2502","38276.6502","49225.2242"],
          "outputCreditsUsage": ["0.0000","0.0000","0.0000","0.0000","509.2044","1100.0052","896.8176"],
          "totalCreditsUsage": ["0.0000","0.0000","0.0000","0.0000","14119.4546","39376.6554","50122.0418"]
        },
        {
          "modelCode": "glm-5.3-flash",
          "modelName": "GLM-5.3-Flash",
          "sortOrder": 2,
          "uncachedInputTokensUsage": [0, 0, 0, 0, 0, 0, 1125],
          "cachedInputTokensUsage": [0, 0, 0, 0, 0, 0, 0],
          "outputTokensUsage": [0, 0, 0, 0, 0, 0, 3182],
          "totalTokensUsage": [0, 0, 0, 0, 0, 0, 4307],
          "uncachedInputCreditsUsage": ["0.0000","0.0000","0.0000","0.0000","0.0000","0.0000","0.1255"],
          "cachedInputCreditsUsage": ["0.0000","0.0000","0.0000","0.0000","0.0000","0.0000","0.0000"],
          "outputCreditsUsage": ["0.0000","0.0000","0.0000","0.0000","0.0000","0.0000","0.8088"],
          "totalCreditsUsage": ["0.0000","0.0000","0.0000","0.0000","0.0000","0.0000","0.9343"]
        }
      ]
    }
  },
  "success": true
}`

// zhipuCreditUsageSnapshotWant 是上面快照的人工转录期望值（按 xTime 顺序，
// 两模型 × 7 日 = 14 行），不复用实现里的任何计算。
func zhipuCreditUsageSnapshotWant() []domain.MonitorQuotaModelCredit {
	dates := []string{"2026-09-30", "2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06"}
	rows := make([]domain.MonitorQuotaModelCredit, 0, 14)
	appendModel := func(model string, input, cached, output, credits []float64) {
		for i, date := range dates {
			rows = append(rows, domain.MonitorQuotaModelCredit{
				Model:        model,
				Date:         date,
				InputTokens:  input[i],
				CachedTokens: cached[i],
				OutputTokens: output[i],
				Credits:      credits[i],
			})
		}
	}
	appendModel("glm-5.3",
		[]float64{0, 0, 0, 0, 3073651, 13109593, 24144636},
		[]float64{0, 0, 0, 0, 147645184, 398128640, 484083584},
		[]float64{0, 0, 0, 0, 424337, 916671, 747348},
		[]float64{0, 0, 0, 0, 14119.4546, 39376.6554, 50122.0418},
	)
	appendModel("glm-5.3-flash",
		[]float64{0, 0, 0, 0, 0, 0, 1125},
		[]float64{0, 0, 0, 0, 0, 0, 0},
		[]float64{0, 0, 0, 0, 0, 0, 3182},
		[]float64{0, 0, 0, 0, 0, 0, 0.9343},
	)
	return rows
}

// --- 测试桩 ---

// zhipuMonitorUpstream 记录调用并把请求真正打到 httptest 服务器上。
type zhipuMonitorUpstream struct {
	mu        sync.Mutex
	calls     int
	methods   []string
	urls      []string
	authz     []string
	responder func(*http.Request) (*http.Response, error)
}

func (u *zhipuMonitorUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls++
	u.methods = append(u.methods, req.Method)
	u.urls = append(u.urls, req.URL.String())
	u.authz = append(u.authz, req.Header.Get("Authorization"))
	responder := u.responder
	u.mu.Unlock()
	if responder != nil {
		return responder(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (u *zhipuMonitorUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func (u *zhipuMonitorUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *zhipuMonitorUpstream) lastURL() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.urls) == 0 {
		return ""
	}
	return u.urls[len(u.urls)-1]
}

func (u *zhipuMonitorUpstream) lastAuth() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.authz) == 0 {
		return ""
	}
	return u.authz[len(u.authz)-1]
}

// zhipuMonitorFakeClock 让 TTL 断言不依赖真实等待。
type zhipuMonitorFakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newZhipuMonitorFakeClock() *zhipuMonitorFakeClock {
	return &zhipuMonitorFakeClock{now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
}

func (c *zhipuMonitorFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *zhipuMonitorFakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func zhipuMonitorManagedAccount(id int64, accessToken string) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformZhipu,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			zhipuCredentialAuthFlow:    ZhipuLoginAuthFlow,
			zhipuCredentialAccessToken: accessToken,
		},
	}
}

func newZhipuMonitorTestService(upstream HTTPUpstream, cfg *config.Config, clock *zhipuMonitorFakeClock) *ZhipuAccountMonitorService {
	svc := NewZhipuAccountMonitorService(nil, upstream, cfg)
	if clock != nil {
		svc.now = clock.Now
	}
	return svc
}

// --- 解析：实测快照逐日逐模型 ---

func TestZhipuAccountMonitorService_FetchUsageDetailForAccount_ParsesSnapshotPerModelPerDay(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(zhipuCreditUsageSnapshotBody))
	}))
	defer server.Close()
	upstream := &zhipuMonitorUpstream{responder: zhipuMonitorRespondVia(server.URL)}
	svc := newZhipuMonitorTestService(upstream, nil, nil)

	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 6, 23, 59, 59, 0, time.UTC)
	got, err := svc.FetchUsageDetailForAccount(context.Background(), zhipuMonitorManagedAccount(41, "at-secret"), start, end)

	require.NoError(t, err)
	require.Equal(t, zhipuCreditUsageSnapshotWant(), got)
	require.Equal(t, 1, upstream.callCount())
	require.Equal(t, "at-secret", upstream.lastAuth())
}

// zhipuMonitorRespondVia 把请求改写到 httptest 服务器上（探针写死生产域名，
// 测试只替换传输层目标，不改实现）。
func zhipuMonitorRespondVia(serverURL string) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		rewritten := req.Clone(req.Context())
		rewritten.URL.Scheme = "http"
		rewritten.URL.Host = serverURL[len("http://"):]
		return http.DefaultTransport.RoundTrip(rewritten)
	}
}

// --- 请求形状：端点、查询参数、鉴权头、+8 日历日窗口 ---

func TestZhipuAccountMonitorService_FetchUsageDetailForAccount_RequestShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		start, end time.Time
		wantStart  string
		wantEnd    string
	}{
		{
			name:      "UTC 午夜按 +8 日历日归一为当日 00:00:00",
			start:     time.Date(2026, 10, 5, 16, 0, 0, 0, time.UTC),
			end:       time.Date(2026, 10, 5, 16, 0, 0, 0, time.UTC),
			wantStart: "2026-10-06 00:00:00",
			wantEnd:   "2026-10-06 23:59:59",
		},
		{
			name:      "+8 当日窗口原样落到同一自然日",
			start:     time.Date(2026, 10, 6, 0, 0, 0, 0, time.FixedZone("CST", 8*3600)),
			end:       time.Date(2026, 10, 6, 23, 59, 59, 0, time.FixedZone("CST", 8*3600)),
			wantStart: "2026-10-06 00:00:00",
			wantEnd:   "2026-10-06 23:59:59",
		},
		{
			name:      "+8 前一日的最后一秒不越界到次日",
			start:     time.Date(2026, 10, 5, 15, 59, 59, 0, time.UTC),
			end:       time.Date(2026, 10, 5, 15, 59, 59, 0, time.UTC),
			wantStart: "2026-10-05 00:00:00",
			wantEnd:   "2026-10-05 23:59:59",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"code":200,"msg":"ok","data":{"modelUsage":{"xTime":["2026-10-06"],"modelDataList":[]}},"success":true}`))
			}))
			defer server.Close()
			upstream := &zhipuMonitorUpstream{responder: zhipuMonitorRespondVia(server.URL)}
			svc := newZhipuMonitorTestService(upstream, nil, nil)

			_, err := svc.FetchUsageDetailForAccount(context.Background(), zhipuMonitorManagedAccount(7, "at-abc.def"), tc.start, tc.end)
			require.NoError(t, err)

			require.Equal(t, 1, upstream.callCount())
			parsed, err := url.Parse(upstream.lastURL())
			require.NoError(t, err)
			require.Equal(t, "https", parsed.Scheme)
			require.Equal(t, "open.bigmodel.cn", parsed.Host)
			require.Equal(t, "/api/monitor/credit-usage/usage-detail", parsed.Path)

			query := parsed.Query()
			require.Equal(t, "1", query.Get("type"))
			require.Equal(t, "MODEL", query.Get("usageType"))
			require.Equal(t, tc.wantStart, query.Get("startTime"))
			require.Equal(t, tc.wantEnd, query.Get("endTime"))

			// 鉴权头必须是 access_token 原值：既不是 api_key，也不带 Bearer 前缀。
			require.Equal(t, "at-abc.def", upstream.lastAuth())
			require.NotContains(t, upstream.lastAuth(), "Bearer")
		})
	}
}

// --- 失败语义：401/403 可区分、非 2xx、业务 code != 200、非 JSON、缺字段 ---

func TestZhipuAccountMonitorService_FetchUsageDetailForAccount_ErrorSemantics(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		status     int
		body       string
		wantErr    error
		wantRows   bool
		wantErrMsg string
	}{
		{
			name:    "HTTP 401 归类为鉴权失败",
			status:  http.StatusUnauthorized,
			body:    `{"code":401,"msg":"invalid token"}`,
			wantErr: ErrZhipuCreditUsageUnauthorized,
		},
		{
			name:    "HTTP 403 归类为鉴权失败",
			status:  http.StatusForbidden,
			body:    `{"code":403,"msg":"forbidden"}`,
			wantErr: ErrZhipuCreditUsageUnauthorized,
		},
		{
			name:       "HTTP 500 归类为上游故障",
			status:     http.StatusInternalServerError,
			body:       `{"code":500,"msg":"internal error"}`,
			wantErr:    ErrZhipuCreditUsageUpstream,
			wantErrMsg: "500",
		},
		{
			name:       "HTTP 2xx 但业务 code 非 200",
			status:     http.StatusOK,
			body:       `{"code":1001,"msg":"参数错误","data":null,"success":false}`,
			wantErr:    ErrZhipuCreditUsageResponse,
			wantErrMsg: "参数错误",
		},
		{
			name:    "HTTP 2xx 但 success=false",
			status:  http.StatusOK,
			body:    `{"code":200,"msg":"ok","success":false,"data":{"modelUsage":{"xTime":["2026-10-06"],"modelDataList":[]}}}`,
			wantErr: ErrZhipuCreditUsageResponse,
		},
		{
			name:    "响应不是 JSON",
			status:  http.StatusOK,
			body:    `<html><body>gateway error</body></html>`,
			wantErr: ErrZhipuCreditUsageResponse,
		},
		{
			name:    "缺少 data.modelUsage.modelDataList",
			status:  http.StatusOK,
			body:    `{"code":200,"msg":"ok","data":{"modelUsage":{"xTime":["2026-10-06"]}},"success":true}`,
			wantErr: ErrZhipuCreditUsageResponse,
		},
		{
			name:    "有模型行但缺 xTime",
			status:  http.StatusOK,
			body:    `{"code":200,"msg":"ok","data":{"modelUsage":{"modelDataList":[{"modelCode":"glm-5.3","totalCreditsUsage":["1.0000"]}]}},"success":true}`,
			wantErr: ErrZhipuCreditUsageResponse,
		},
		{
			name:     "modelDataList 为空数组合法返回零行",
			status:   http.StatusOK,
			body:     `{"code":200,"msg":"ok","data":{"modelUsage":{"xTime":["2026-10-06"],"modelDataList":[]}},"success":true}`,
			wantRows: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			upstream := &zhipuMonitorUpstream{responder: zhipuMonitorRespondVia(server.URL)}
			svc := newZhipuMonitorTestService(upstream, nil, nil)

			got, err := svc.FetchUsageDetailForAccount(context.Background(), zhipuMonitorManagedAccount(9, "at-1"), time.Now(), time.Now())

			if tc.wantErr == nil {
				require.NoError(t, err)
				require.Empty(t, got)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			// 出错时不返回半截数据（keeper 判活只看错误分类）。
			require.Nil(t, got)
			if tc.wantErrMsg != "" {
				require.Contains(t, err.Error(), tc.wantErrMsg)
			}
		})
	}
}

func TestZhipuAccountMonitorService_FetchUsageDetailForAccount_ParsesPartialRows(t *testing.T) {
	t.Parallel()

	// 缺字段 + 数字以字符串返回：缺失的分项按 0 计，不报错也不丢行。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"msg":"ok","success":true,"data":{"modelUsage":{
			"xTime":["2026-10-05","2026-10-06"],
			"modelDataList":[{
				"modelCode":"glm-5.3",
				"uncachedInputTokensUsage":["1234",null],
				"cachedInputTokensUsage":[0],
				"uncachedInputCreditsUsage":["1.5000"],
				"outputCreditsUsage":["2.5000"],
				"cachedInputCreditsUsage":["0.2500"]
			}]
		}}}`))
	}))
	defer server.Close()
	upstream := &zhipuMonitorUpstream{responder: zhipuMonitorRespondVia(server.URL)}
	svc := newZhipuMonitorTestService(upstream, nil, nil)

	got, err := svc.FetchUsageDetailForAccount(context.Background(), zhipuMonitorManagedAccount(10, "at-2"), time.Now(), time.Now())

	require.NoError(t, err)
	require.Equal(t, []domain.MonitorQuotaModelCredit{
		// 无 totalCreditsUsage → credits = 1.5(未缓存输入) + 0.25(缓存输入) + 2.5(输出)
		{Model: "glm-5.3", Date: "2026-10-05", InputTokens: 1234, CachedTokens: 0, OutputTokens: 0, Credits: 4.25},
		{Model: "glm-5.3", Date: "2026-10-06", InputTokens: 0, CachedTokens: 0, OutputTokens: 0, Credits: 0},
	}, got)
}

func TestZhipuAccountMonitorService_FetchUsageDetailForAccount_NoCredentialSkipsUpstream(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		account *Account
	}{
		{
			name: "非登录托管账号",
			account: &Account{ID: 11, Platform: PlatformZhipu, Type: AccountTypeAPIKey, Status: StatusActive,
				Credentials: map[string]any{"api_key": "sk-manual"}},
		},
		{
			name:    "托管账号但没有 access_token",
			account: zhipuMonitorManagedAccount(12, ""),
		},
		{
			name:    "nil 账号",
			account: nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := &zhipuMonitorUpstream{responder: func(*http.Request) (*http.Response, error) {
				t.Fatal("must not issue upstream request without access_token")
				return nil, nil
			}}
			svc := newZhipuMonitorTestService(upstream, nil, nil)

			got, err := svc.FetchUsageDetailForAccount(context.Background(), tc.account, time.Now(), time.Now())

			require.ErrorIs(t, err, ErrZhipuCreditUsageNoCredential)
			require.Nil(t, got)
			require.Zero(t, upstream.callCount())
		})
	}
}

func TestZhipuAccountMonitorService_FetchUsageDetailForAccount_TransportAndDeadline(t *testing.T) {
	t.Parallel()

	t.Run("传输层失败归类为上游故障", func(t *testing.T) {
		t.Parallel()

		upstream := &zhipuMonitorUpstream{responder: func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp: connection refused")
		}}
		svc := newZhipuMonitorTestService(upstream, nil, nil)

		got, err := svc.FetchUsageDetailForAccount(context.Background(), zhipuMonitorManagedAccount(13, "at-3"), time.Now(), time.Now())

		require.ErrorIs(t, err, ErrZhipuCreditUsageUpstream)
		require.Nil(t, got)
	})

	t.Run("调用方超时不被上游拖住", func(t *testing.T) {
		t.Parallel()

		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer func() {
			close(release)
			server.Close()
		}()
		upstream := &zhipuMonitorUpstream{responder: zhipuMonitorRespondVia(server.URL)}
		svc := newZhipuMonitorTestService(upstream, nil, nil)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		got, err := svc.FetchUsageDetailForAccount(ctx, zhipuMonitorManagedAccount(14, "at-4"), start, start)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Nil(t, got)
		require.Less(t, time.Since(start), 5*time.Second)
	})
}

// --- singleflight / TTL 缓存：keeper（09）与监控（11）共用同一上游入口 ---

func newZhipuMonitorSnapshotServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(zhipuCreditUsageSnapshotBody))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestZhipuAccountMonitorService_FetchUsageDetailForAccount_MergesConcurrentCalls(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		<-release
		_, _ = w.Write([]byte(zhipuCreditUsageSnapshotBody))
	}))
	defer server.Close()
	upstream := &zhipuMonitorUpstream{responder: zhipuMonitorRespondVia(server.URL)}
	svc := newZhipuMonitorTestService(upstream, nil, newZhipuMonitorFakeClock())

	const callers = 8
	account := zhipuMonitorManagedAccount(21, "at-shared")
	window := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

	results := make([][]domain.MonitorQuotaModelCredit, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.FetchUsageDetailForAccount(context.Background(), account, window, window)
		}(i)
	}
	<-started
	time.Sleep(50 * time.Millisecond) // 让其余调用者进入 singleflight 等待
	close(release)
	wg.Wait()

	for i := 0; i < callers; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, zhipuCreditUsageSnapshotWant(), results[i])
	}
	require.Equal(t, 1, upstream.callCount(), "并发调用必须合并为一次上游请求")
}

func TestZhipuAccountMonitorService_FetchUsageDetailForAccount_CacheTTLWindows(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		cfg         *config.Config
		cachedAfter time.Duration
		staleAfter  time.Duration
	}{
		{
			name:        "默认 TTL 10 分钟（cfg 未配置）",
			cfg:         nil,
			cachedAfter: 9 * time.Minute,
			staleAfter:  11 * time.Minute,
		},
		{
			name: "按 reset_status_cache_minutes 覆盖",
			cfg:  &config.Config{Gateway: config.GatewayConfig{Zhipu: config.GatewayZhipuConfig{ResetStatusCacheMinutes: 1}}},
			// 配置为 1 分钟：59 秒内命中缓存，61 秒后必须重取。
			cachedAfter: 30 * time.Second,
			staleAfter:  61 * time.Second,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := newZhipuMonitorSnapshotServer(t)
			upstream := &zhipuMonitorUpstream{responder: zhipuMonitorRespondVia(server.URL)}
			clock := newZhipuMonitorFakeClock()
			svc := newZhipuMonitorTestService(upstream, tc.cfg, clock)

			account := zhipuMonitorManagedAccount(22, "at-ttl")
			window := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

			first, err := svc.FetchUsageDetailForAccount(context.Background(), account, window, window)
			require.NoError(t, err)
			require.Equal(t, zhipuCreditUsageSnapshotWant(), first)

			clock.Advance(tc.cachedAfter)
			second, err := svc.FetchUsageDetailForAccount(context.Background(), account, window, window)
			require.NoError(t, err)
			require.Equal(t, first, second)
			require.Equal(t, 1, upstream.callCount(), "TTL 内必须命中缓存")

			clock.Advance(tc.staleAfter - tc.cachedAfter)
			third, err := svc.FetchUsageDetailForAccount(context.Background(), account, window, window)
			require.NoError(t, err)
			require.Equal(t, first, third)
			require.Equal(t, 2, upstream.callCount(), "TTL 过期后必须重取")
		})
	}
}

func TestZhipuAccountMonitorService_FetchUsageDetailForAccount_CacheScopeIsAccountAndWindow(t *testing.T) {
	t.Parallel()

	server := newZhipuMonitorSnapshotServer(t)
	upstream := &zhipuMonitorUpstream{responder: zhipuMonitorRespondVia(server.URL)}
	svc := newZhipuMonitorTestService(upstream, nil, newZhipuMonitorFakeClock())

	window := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	_, err := svc.FetchUsageDetailForAccount(context.Background(), zhipuMonitorManagedAccount(23, "at-a"), window, window)
	require.NoError(t, err)

	// 同窗口不同账号：各自独立请求（缓存按账号隔离）。
	_, err = svc.FetchUsageDetailForAccount(context.Background(), zhipuMonitorManagedAccount(24, "at-b"), window, window)
	require.NoError(t, err)
	require.Equal(t, 2, upstream.callCount())

	// 同账号不同自然日窗口：不得串用缓存。
	nextWindow := window.Add(24 * time.Hour)
	_, err = svc.FetchUsageDetailForAccount(context.Background(), zhipuMonitorManagedAccount(23, "at-a"), nextWindow, nextWindow)
	require.NoError(t, err)
	require.Equal(t, 3, upstream.callCount())

	// 再次请求原窗口：仍在 TTL 内，命中缓存。
	_, err = svc.FetchUsageDetailForAccount(context.Background(), zhipuMonitorManagedAccount(23, "at-a"), window, window)
	require.NoError(t, err)
	require.Equal(t, 3, upstream.callCount())
}

func TestZhipuAccountMonitorService_FetchUsageDetailForAccount_DoesNotCacheErrors(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	status := http.StatusInternalServerError
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current := status
		mu.Unlock()
		w.WriteHeader(current)
		if current == http.StatusOK {
			_, _ = w.Write([]byte(zhipuCreditUsageSnapshotBody))
			return
		}
		_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
	}))
	defer server.Close()
	upstream := &zhipuMonitorUpstream{responder: zhipuMonitorRespondVia(server.URL)}
	svc := newZhipuMonitorTestService(upstream, nil, newZhipuMonitorFakeClock())

	account := zhipuMonitorManagedAccount(25, "at-err")
	window := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

	_, err := svc.FetchUsageDetailForAccount(context.Background(), account, window, window)
	require.ErrorIs(t, err, ErrZhipuCreditUsageUpstream)

	mu.Lock()
	status = http.StatusOK
	mu.Unlock()
	got, err := svc.FetchUsageDetailForAccount(context.Background(), account, window, window)
	require.NoError(t, err)
	require.Equal(t, zhipuCreditUsageSnapshotWant(), got)
	require.Equal(t, 2, upstream.callCount(), "失败结果不得写入缓存")
}

func TestZhipuAccountMonitorService_FetchUsageDetailForAccount_CachedRowsAreIsolatedFromCallers(t *testing.T) {
	t.Parallel()

	server := newZhipuMonitorSnapshotServer(t)
	upstream := &zhipuMonitorUpstream{responder: zhipuMonitorRespondVia(server.URL)}
	svc := newZhipuMonitorTestService(upstream, nil, newZhipuMonitorFakeClock())

	account := zhipuMonitorManagedAccount(26, "at-copy")
	window := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

	first, err := svc.FetchUsageDetailForAccount(context.Background(), account, window, window)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	first[0].Credits = -1
	first[0].Model = "tampered"

	second, err := svc.FetchUsageDetailForAccount(context.Background(), account, window, window)
	require.NoError(t, err)
	require.Equal(t, zhipuCreditUsageSnapshotWant(), second)
	require.Equal(t, 1, upstream.callCount())
}

// --- 跨凭据隔离：账号级缓存/合并不得串号（主会话通报 #12 的 RiskClient 端点级单飞坑） ---

// zhipuMonitorPayloadByToken 给每个 access_token 一份专属响应：串号会立刻现形。
var zhipuMonitorPayloadByToken = map[string]string{
	"at-account-a": `{"code":200,"msg":"ok","success":true,"data":{"modelUsage":{
		"xTime":["2026-10-06"],
		"modelDataList":[{"modelCode":"glm-5.3","uncachedInputTokensUsage":[11],"cachedInputTokensUsage":[22],
			"outputTokensUsage":[33],"totalCreditsUsage":["4.5000"]}]}}}`,
	"at-account-b": `{"code":200,"msg":"ok","success":true,"data":{"modelUsage":{
		"xTime":["2026-10-06"],
		"modelDataList":[{"modelCode":"glm-5.3-flash","uncachedInputTokensUsage":[111],"cachedInputTokensUsage":[222],
			"outputTokensUsage":[333],"totalCreditsUsage":["44.5000"]}]}}}`,
}

func TestZhipuAccountMonitorService_FetchUsageDetailForAccount_DoesNotBleedAcrossCredentials(t *testing.T) {
	t.Parallel()

	// 两个账号并发：不同凭据必须各自探测（2 次上游），且各自只看到自己的数据。
	var mu sync.Mutex
	seen := 0
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, ok := zhipuMonitorPayloadByToken[r.Header.Get("Authorization")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		seen++
		if seen == 2 {
			close(release)
		}
		mu.Unlock()
		select {
		case <-release:
		case <-time.After(500 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()
	upstream := &zhipuMonitorUpstream{responder: zhipuMonitorRespondVia(server.URL)}
	svc := newZhipuMonitorTestService(upstream, nil, newZhipuMonitorFakeClock())

	window := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	accountA := zhipuMonitorManagedAccount(31, "at-account-a")
	accountB := zhipuMonitorManagedAccount(32, "at-account-b")

	var wg sync.WaitGroup
	var gotA, gotB []domain.MonitorQuotaModelCredit
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		gotA, errA = svc.FetchUsageDetailForAccount(context.Background(), accountA, window, window)
	}()
	go func() {
		defer wg.Done()
		gotB, errB = svc.FetchUsageDetailForAccount(context.Background(), accountB, window, window)
	}()
	wg.Wait()

	require.NoError(t, errA)
	require.NoError(t, errB)
	require.Equal(t, []domain.MonitorQuotaModelCredit{
		{Model: "glm-5.3", Date: "2026-10-06", InputTokens: 11, CachedTokens: 22, OutputTokens: 33, Credits: 4.5},
	}, gotA)
	require.Equal(t, []domain.MonitorQuotaModelCredit{
		{Model: "glm-5.3-flash", Date: "2026-10-06", InputTokens: 111, CachedTokens: 222, OutputTokens: 333, Credits: 44.5},
	}, gotB)
	require.Equal(t, 2, upstream.callCount(), "不同凭据的并发探测不得被合并/串用")
}

// ---------------------------------------------------------------------------
// 重置卡只读读取（design M4 / 票 12 接线；R0：仅观测，永不使用）
// ---------------------------------------------------------------------------

// zhipuResetReaderFake 是重置卡读取器接缝的桩：记录凭据、返回预置状态，
// 让 service 层的映射/静默语义独立于 bigmodel 包的真实 HTTP 实现被测。
type zhipuResetReaderFake struct {
	status *bigmodel.ResetStatus
	err    error

	mu       sync.Mutex
	calls    int
	zcodes   []string
	accesses []string
}

func (f *zhipuResetReaderFake) Fetch(_ context.Context, zcodeJWTToken, accessToken string) (*bigmodel.ResetStatus, error) {
	f.mu.Lock()
	f.calls++
	f.zcodes = append(f.zcodes, zcodeJWTToken)
	f.accesses = append(f.accesses, accessToken)
	f.mu.Unlock()
	return f.status, f.err
}

func (f *zhipuResetReaderFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *zhipuResetReaderFake) lastCredentialPair() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.zcodes) == 0 {
		return "", ""
	}
	return f.zcodes[len(f.zcodes)-1], f.accesses[len(f.accesses)-1]
}

// zhipuMonitorManagedAccountWithJWT 构造同时带 zcodejwttoken 与 access_token 的
// 登录托管账号（重置卡读取需要两个头，缺一不发上游）。
func zhipuMonitorManagedAccountWithJWT(id int64, zcodeJWTToken, accessToken string) *Account {
	account := zhipuMonitorManagedAccount(id, accessToken)
	account.Credentials[zhipuCredentialZCodeJWT] = zcodeJWTToken
	return account
}

// zhipuResetReaderService 构造「只有读取器」的服务：HTTP 上游为 nil，
// 因此构造期不装配真实读取器，由用例显式注入桩。
func zhipuResetReaderService(reader zhipuResetStatusReader) *ZhipuAccountMonitorService {
	svc := NewZhipuAccountMonitorService(nil, nil, nil)
	svc.resetStatus = reader
	return svc
}

// 映射：两池 → 快照字段（five_hour 在前、week 在后，到期时间 RFC3339 UTC，
// 上游未给 expire_at 的卡保留但到期留空）。
func TestZhipuAccountMonitorService_FetchResetStatusForAccount_MapsReaderPools(t *testing.T) {
	t.Parallel()

	fiveHourA := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	weekA := time.Date(2026, 11, 5, 9, 20, 0, 0, time.UTC)
	// 上游可以给非 UTC 的到期时刻：快照字段一律归一为 UTC RFC3339。
	weekB := time.Date(2026, 11, 12, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))

	cases := []struct {
		name   string
		status *bigmodel.ResetStatus
		want   []domain.MonitorResetCard
	}{
		{
			name: "two pools",
			status: &bigmodel.ResetStatus{
				FiveHourCards: []bigmodel.ResetCard{
					{Type: bigmodel.ResetCardTypeFiveHour, ExpireAt: fiveHourA},
				},
				WeekCards: []bigmodel.ResetCard{
					{Type: bigmodel.ResetCardTypeWeek, ExpireAt: weekA},
					{Type: bigmodel.ResetCardTypeWeek, ExpireAt: weekB},
				},
				HasUnreadHistory: true,
			},
			want: []domain.MonitorResetCard{
				{Type: "five_hour", ExpireAt: "2026-10-06T12:00:00Z"},
				{Type: "week", ExpireAt: "2026-11-05T09:20:00Z"},
				{Type: "week", ExpireAt: "2026-11-11T16:00:00Z"},
			},
		},
		{
			name: "five hour pool only",
			status: &bigmodel.ResetStatus{
				FiveHourCards: []bigmodel.ResetCard{{Type: bigmodel.ResetCardTypeFiveHour, ExpireAt: fiveHourA}},
			},
			want: []domain.MonitorResetCard{{Type: "five_hour", ExpireAt: "2026-10-06T12:00:00Z"}},
		},
		{
			name: "week pool only",
			status: &bigmodel.ResetStatus{
				WeekCards: []bigmodel.ResetCard{{Type: bigmodel.ResetCardTypeWeek, ExpireAt: weekA}},
			},
			want: []domain.MonitorResetCard{{Type: "week", ExpireAt: "2026-11-05T09:20:00Z"}},
		},
		{
			// 卡片存在但上游没给到期时间：保留卡片（不能按 0 张处理），到期留空。
			name: "card without expiry keeps the card",
			status: &bigmodel.ResetStatus{
				FiveHourCards: []bigmodel.ResetCard{{Type: bigmodel.ResetCardTypeFiveHour}},
			},
			want: []domain.MonitorResetCard{{Type: "five_hour", ExpireAt: ""}},
		},
		{
			name:   "both pools empty stays absent",
			status: &bigmodel.ResetStatus{},
			want:   nil,
		},
		{
			name:   "nil status stays absent",
			status: nil,
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &zhipuResetReaderFake{status: tc.status}
			svc := zhipuResetReaderService(reader)

			got, err := svc.FetchResetStatusForAccount(
				context.Background(), zhipuMonitorManagedAccountWithJWT(61, "zc-jwt-61", "at-61"))

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			// 读取器拿到的必须是该账号的两个 token（Authorization 与 X-Bigmodel-Authorization
			// 不能互换，见 reset_status.go 的头部契约）。
			zcode, access := reader.lastCredentialPair()
			require.Equal(t, "zc-jwt-61", zcode)
			require.Equal(t, "at-61", access)
			require.Equal(t, 1, reader.callCount())
		})
	}
}

// 静默语义：非登录托管账号 / 缺任一 token / 读取器未接线 → (nil, nil) 且不触碰上游。
func TestZhipuAccountMonitorService_FetchResetStatusForAccount_SilentWithoutFullCredentials(t *testing.T) {
	t.Parallel()

	legacyAPIKey := &Account{
		ID: 62, Platform: PlatformZhipu, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			zhipuCredentialAccountMode: AccountModeCoding,
			zhipuCredentialAPIKey:      "sk-legacy",
			zhipuCredentialAccessToken: "at-legacy",
			zhipuCredentialZCodeJWT:    "zc-legacy",
		},
	}
	missingZCodeJWT := zhipuMonitorManagedAccount(63, "at-63")
	missingAccessToken := zhipuMonitorManagedAccount(64, "")
	missingAccessToken.Credentials[zhipuCredentialZCodeJWT] = "zc-64"
	blankTokens := zhipuMonitorManagedAccountWithJWT(65, "   ", "  ")

	cases := []struct {
		name    string
		account *Account
	}{
		{name: "managed account without zcode jwt", account: missingZCodeJWT},
		{name: "managed account without access token", account: missingAccessToken},
		{name: "managed account with blank tokens", account: blankTokens},
		{name: "legacy api key account", account: legacyAPIKey},
		{name: "nil account", account: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &zhipuResetReaderFake{status: &bigmodel.ResetStatus{
				WeekCards: []bigmodel.ResetCard{{Type: bigmodel.ResetCardTypeWeek}},
			}}
			svc := zhipuResetReaderService(reader)

			got, err := svc.FetchResetStatusForAccount(context.Background(), tc.account)

			require.NoError(t, err)
			require.Nil(t, got)
			require.Equal(t, 0, reader.callCount(), "缺凭据/非托管账号不得发起上游请求")
		})
	}
}

// 未接线（无 HTTP 上游 → 读取器为 nil）时整链静默跳过，不 panic。
func TestZhipuAccountMonitorService_FetchResetStatusForAccount_UnwiredReaderIsSilent(t *testing.T) {
	t.Parallel()

	svc := NewZhipuAccountMonitorService(nil, nil, nil)
	require.Nil(t, svc.resetStatus, "无 HTTP 上游时不得装配读取器")

	got, err := svc.FetchResetStatusForAccount(
		context.Background(), zhipuMonitorManagedAccountWithJWT(66, "zc-66", "at-66"))

	require.NoError(t, err)
	require.Nil(t, got)
}

// 上游失败（风控退避 / 401 / 传输错误）由调用方降级为「只缺字段」：本方法原样返回错误。
func TestZhipuAccountMonitorService_FetchResetStatusForAccount_PropagatesReaderFailure(t *testing.T) {
	t.Parallel()

	reader := &zhipuResetReaderFake{err: errors.New("bigmodel reset_status: upstream code 3012")}
	svc := zhipuResetReaderService(reader)

	got, err := svc.FetchResetStatusForAccount(
		context.Background(), zhipuMonitorManagedAccountWithJWT(67, "zc-67", "at-67"))

	require.Error(t, err)
	require.Contains(t, err.Error(), "3012")
	require.Nil(t, got)
	require.Equal(t, 1, reader.callCount())
}

// 接线集成：真实读取器经共享 RiskClient 发只读 GET，两个 token 各就各头。
// 这是「读取器接入监控链路」的端到端证据（服务内的装配 + 请求形状 + 映射）。
func TestZhipuAccountMonitorService_FetchResetStatusForAccount_ReadsThroughRiskClient(t *testing.T) {
	t.Parallel()

	const resetStatusBody = `{"code":0,"msg":"ok","data":{` +
		`"available_five_hour_resets":[{"expire_at":1791288000000}],` +
		`"available_week_resets":[],"has_unread_history":false}}`

	var mu sync.Mutex
	var paths []string
	var methods []string
	var bigmodelAuth []string
	var targetTypes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		methods = append(methods, r.Method)
		bigmodelAuth = append(bigmodelAuth, r.Header.Get("X-Bigmodel-Authorization"))
		targetTypes = append(targetTypes, r.Header.Get("Bigmodel-Target-Type"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resetStatusBody))
	}))
	defer server.Close()

	upstream := &zhipuMonitorUpstream{responder: zhipuMonitorRespondVia(server.URL)}
	svc := newZhipuMonitorTestService(upstream, nil, nil)

	got, err := svc.FetchResetStatusForAccount(
		context.Background(), zhipuMonitorManagedAccountWithJWT(68, "zc-jwt-68", "at-68"))

	require.NoError(t, err)
	// 1791288000000 ms = 2026-10-06T12:00:00Z（独立核对：date -u -r 1791288000）。
	require.Equal(t, []domain.MonitorResetCard{
		{Type: "five_hour", ExpireAt: "2026-10-06T12:00:00Z"},
	}, got)

	require.Equal(t, 1, upstream.callCount())
	require.Equal(t, []string{"/api/v1/coding-plan/reset/status"}, paths)
	require.Equal(t, []string{http.MethodGet}, methods, "重置卡读取必须是只读 GET（R0）")
	require.Equal(t, []string{"at-68"}, bigmodelAuth, "access_token 走 X-Bigmodel-Authorization")
	require.Equal(t, []string{"PERSONAL"}, targetTypes)
	require.Equal(t, "zc-jwt-68", upstream.lastAuth(), "zcodejwttoken 原值走 Authorization（无 Bearer）")
}
