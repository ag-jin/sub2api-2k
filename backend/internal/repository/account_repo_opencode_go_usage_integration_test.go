//go:build integration

package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// openCodeGoManagedExtra 构造同时携带探测 / Ollama / OpenCode 受管键的 extra，
// 用于验证各身份清理分支只清自己该清的键、不跨身份误清。
func openCodeGoManagedExtra(now time.Time) map[string]any {
	return map[string]any{
		service.UpstreamBillingProbeExtraKey:        map[string]any{"status": "ok"},
		service.OllamaCloudUsageSessionExtraKey:     "cipher:wos-session=fixture",
		service.OllamaCloudUsageAutoRefreshExtraKey: true,
		service.OllamaCloudUsageSnapshotExtraKey: map[string]any{
			"status": service.OllamaCloudUsageStatusOK, "last_attempt_at": now, "next_refresh_at": now.Add(time.Hour),
		},
		service.OpenCodeGoUsageAutoRefreshExtraKey: true,
		service.OpenCodeGoUsageSnapshotExtraKey: map[string]any{
			"status": service.OpenCodeGoUsageStatusOK, "last_attempt_at": now, "next_refresh_at": now.Add(time.Hour),
		},
	}
}

// F1：混合 OpenCode + openai Ollama + anthropic Ollama 行的批量代理变化与
// 代理+key 变化，验证 OpenCode/Ollama CASE 分支互斥、各自只清自己该清的键。
// 回归点：OpenCode eligible 判定若不含旧行 opencode base URL，OpenAI+Ollama 行
// 在代理变化时会先命中 OpenCode 分支而遮蔽 Ollama 快照清理。
func TestBulkUpdateOpenCodeGoMixedPlatformIdentityCleanup(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Now().UTC()

	create := func(name, platform, baseURL string, extra map[string]any) *service.Account {
		t.Helper()
		return mustCreateAccount(t, tx.Client(), &service.Account{
			Name: name, Platform: platform, Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "shared-key", "base_url": baseURL},
			Extra:       extra,
		})
	}
	ollamaExtra := func() map[string]any {
		return map[string]any{
			service.OllamaCloudUsageSessionExtraKey:     "cipher:wos-session=fixture",
			service.OllamaCloudUsageAutoRefreshExtraKey: true,
			service.OllamaCloudUsageSnapshotExtraKey: map[string]any{
				"status": service.OllamaCloudUsageStatusOK, "last_attempt_at": now, "next_refresh_at": now.Add(time.Hour),
			},
		}
	}
	opencode := create("mixed-opencode", service.PlatformOpenAI, "https://opencode.ai/zen/go/v1", map[string]any{
		service.OpenCodeGoUsageAutoRefreshExtraKey: true,
		service.OpenCodeGoUsageSnapshotExtraKey: map[string]any{
			"status": service.OpenCodeGoUsageStatusOK, "last_attempt_at": now, "next_refresh_at": now.Add(time.Hour),
		},
	})
	openaiOllama := create("mixed-openai-ollama", service.PlatformOpenAI, "https://ollama.com", ollamaExtra())
	anthropicOllama := create("mixed-anthropic-ollama", service.PlatformAnthropic, "https://ollama.com", ollamaExtra())
	proxy := mustCreateProxy(t, tx.Client(), &service.Proxy{
		Name: "mixed-proxy", Protocol: "http", Host: "127.0.0.1", Port: 3128,
		Username: "user", Password: "pass", Status: service.StatusActive,
	})
	proxy2 := mustCreateProxy(t, tx.Client(), &service.Proxy{
		Name: "mixed-proxy-2", Protocol: "http", Host: "127.0.0.2", Port: 3128,
		Username: "user", Password: "pass", Status: service.StatusActive,
	})

	// 只改 proxy：OpenCode 行清快照留开关；Ollama 行清快照留 session/开关。
	rows, err := repo.BulkUpdate(ctx, []int64{opencode.ID, openaiOllama.ID, anthropicOllama.ID}, service.AccountBulkUpdate{
		ProxyID: &proxy.ID,
	})
	require.NoError(t, err)
	require.Equal(t, int64(3), rows)

	opencodeLoaded, err := repo.GetByID(ctx, opencode.ID)
	require.NoError(t, err)
	require.Equal(t, true, opencodeLoaded.Extra[service.OpenCodeGoUsageAutoRefreshExtraKey])
	require.NotContains(t, opencodeLoaded.Extra, service.OpenCodeGoUsageSnapshotExtraKey)

	for _, id := range []int64{openaiOllama.ID, anthropicOllama.ID} {
		loaded, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		require.Equal(t, "cipher:wos-session=fixture", loaded.Extra[service.OllamaCloudUsageSessionExtraKey])
		require.Equal(t, true, loaded.Extra[service.OllamaCloudUsageAutoRefreshExtraKey])
		require.NotContains(t, loaded.Extra, service.OllamaCloudUsageSnapshotExtraKey,
			"OpenAI+Ollama 行的快照必须由 Ollama 分支清理，不能被 OpenCode 分支遮蔽")
	}

	// 同时改 proxy + api_key：OpenCode 行清开关+快照；Ollama 行清 session/开关/快照。
	rows, err = repo.BulkUpdate(ctx, []int64{opencode.ID, openaiOllama.ID, anthropicOllama.ID}, service.AccountBulkUpdate{
		ProxyID:     &proxy2.ID,
		Credentials: map[string]any{"api_key": "rotated-key"},
	})
	require.NoError(t, err)
	require.Equal(t, int64(3), rows)

	opencodeLoaded, err = repo.GetByID(ctx, opencode.ID)
	require.NoError(t, err)
	require.NotContains(t, opencodeLoaded.Extra, service.OpenCodeGoUsageAutoRefreshExtraKey)
	require.NotContains(t, opencodeLoaded.Extra, service.OpenCodeGoUsageSnapshotExtraKey)

	for _, id := range []int64{openaiOllama.ID, anthropicOllama.ID} {
		loaded, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		require.NotContains(t, loaded.Extra, service.OllamaCloudUsageSessionExtraKey)
		require.NotContains(t, loaded.Extra, service.OllamaCloudUsageAutoRefreshExtraKey)
		require.NotContains(t, loaded.Extra, service.OllamaCloudUsageSnapshotExtraKey)
	}
}

// F2：UpdateCredentials 的 OpenCode 分支必须与通用身份变化不变量一致——
// 清 probe、OpenCode 两键以及不应跨身份残留的 Ollama session/auto/snapshot。
func TestUpdateCredentialsOpenCodeGoClearsAllManagedStateOnIdentityChange(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Now().UTC()

	account := mustCreateAccount(t, tx.Client(), &service.Account{
		Name: "opencode-credentials", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "old-key", "base_url": "https://opencode.ai/zen/go/v1"},
		Extra:       openCodeGoManagedExtra(now),
	})

	require.NoError(t, repo.UpdateCredentials(ctx, account.ID, map[string]any{
		"api_key": "new-key", "base_url": "https://opencode.ai/zen/go/v1",
	}))
	loaded, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.NotContains(t, loaded.Extra, service.UpstreamBillingProbeExtraKey)
	require.NotContains(t, loaded.Extra, service.OpenCodeGoUsageAutoRefreshExtraKey)
	require.NotContains(t, loaded.Extra, service.OpenCodeGoUsageSnapshotExtraKey)
	require.NotContains(t, loaded.Extra, service.OllamaCloudUsageSessionExtraKey)
	require.NotContains(t, loaded.Extra, service.OllamaCloudUsageAutoRefreshExtraKey)
	require.NotContains(t, loaded.Extra, service.OllamaCloudUsageSnapshotExtraKey)
}

// F3：新 credentials 缺 base_url（NULL）时 OpenCode 分支必须仍命中（NULL-safe），
// 否则 OpenCode 受管状态会残留。api_key 保持不变，专门压 NOT regex(NULL) 路径。
func TestUpdateCredentialsOpenCodeGoMissingBaseURLClearsManagedState(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Now().UTC()

	account := mustCreateAccount(t, tx.Client(), &service.Account{
		Name: "opencode-missing-base-url", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "old-key", "base_url": "https://opencode.ai/zen/go/v1"},
		Extra: map[string]any{
			service.OpenCodeGoUsageAutoRefreshExtraKey: true,
			service.OpenCodeGoUsageSnapshotExtraKey: map[string]any{
				"status": service.OpenCodeGoUsageStatusOK, "last_attempt_at": now, "next_refresh_at": now.Add(time.Hour),
			},
		},
	})

	require.NoError(t, repo.UpdateCredentials(ctx, account.ID, map[string]any{
		"api_key": "old-key",
	}))
	loaded, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.NotContains(t, loaded.Extra, service.OpenCodeGoUsageAutoRefreshExtraKey)
	require.NotContains(t, loaded.Extra, service.OpenCodeGoUsageSnapshotExtraKey)
}

// F3 延伸：BulkUpdate 移除 base_url（null）时 opencode 受管键必须清除，
// 否则 NOT regex(NULL) 为 NULL 会令 WHEN 不命中而残留。
func TestBulkUpdateOpenCodeGoRemovedBaseURLClearsManagedState(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Now().UTC()

	account := mustCreateAccount(t, tx.Client(), &service.Account{
		Name: "opencode-bulk-remove-base-url", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "old-key", "base_url": "https://opencode.ai/zen/go/v1"},
		Extra: map[string]any{
			service.OpenCodeGoUsageAutoRefreshExtraKey: true,
			service.OpenCodeGoUsageSnapshotExtraKey: map[string]any{
				"status": service.OpenCodeGoUsageStatusOK, "last_attempt_at": now, "next_refresh_at": now.Add(time.Hour),
			},
		},
	})

	rows, err := repo.BulkUpdate(ctx, []int64{account.ID}, service.AccountBulkUpdate{
		Credentials: map[string]any{"base_url": nil},
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)
	loaded, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.NotContains(t, loaded.Extra, service.OpenCodeGoUsageAutoRefreshExtraKey)
	require.NotContains(t, loaded.Extra, service.OpenCodeGoUsageSnapshotExtraKey)
}

// F6：SQL eligible 判定与 Go 侧对齐，接受显式默认端口 :443。
func TestOpenCodeGoUsageEligibleSQLAcceptsDefaultPort443(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

	account := mustCreateAccount(t, tx.Client(), &service.Account{
		Name: "opencode-port-443", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "port-key", "base_url": "https://opencode.ai:443/zen/go/v1"},
		Extra:       map[string]any{},
	})

	// ListOpenCodeGoUsageGroupAccounts 走 opencodeGoUsageEligibleSQL，能解析到
	// :443 行即证明 SQL 正则与 Go 判定一致。
	groups, err := repo.ListOpenCodeGoUsageGroupAccounts(ctx, []*service.Account{account})
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, account.ID, groups[0].ID)
}

// 资格泛化回归：opencode_go 平台 Go 订阅账号（不依赖 base_url，未设置
// account_mode 默认 Go）与 Anthropic 基址（/zen/go）挂载行都必须命中
// opencodeGoUsageEligibleSQL；同 key 的 Zen 模式行必须被 SQL 排除。
func TestOpenCodeGoUsageEligibleSQLOpencodePlatformAndAnthropicBaseURL(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

	platformGo := mustCreateAccount(t, tx.Client(), &service.Account{
		Name: "opencode-platform-go", Platform: service.PlatformOpenCodeGo, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "platform-key"}, // 无 base_url、无 account_mode（默认 Go）
		Extra:       map[string]any{},
	})
	// 同 key 的 Zen 行：SQL 必须排除（account_mode = zen）。
	platformZen := mustCreateAccount(t, tx.Client(), &service.Account{
		Name: "opencode-platform-zen", Platform: service.PlatformOpenCodeGo, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "platform-key", "account_mode": service.AccountModeZen},
		Extra:       map[string]any{},
	})
	anthropicMount := mustCreateAccount(t, tx.Client(), &service.Account{
		Name: "opencode-anthropic-mount", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "mount-key", "base_url": "https://opencode.ai/zen/go"},
		Extra:       map[string]any{},
	})

	groups, err := repo.ListOpenCodeGoUsageGroupAccounts(ctx, []*service.Account{platformGo, anthropicMount})
	require.NoError(t, err)
	require.Len(t, groups, 2)
	ids := make(map[int64]struct{}, len(groups))
	for _, account := range groups {
		ids[account.ID] = struct{}{}
	}
	require.Contains(t, ids, platformGo.ID)
	require.Contains(t, ids, anthropicMount.ID)
	require.NotContains(t, ids, platformZen.ID, "同 key 的 Zen 模式行必须被 SQL 谓词排除")
}

// 资格泛化回归：UpdateCredentials 的 OpenCode 清理分支里，IS NOT TRUE 只能作用于
// 新凭证的检查本身，平台条件必须在作用域外——否则挂载行 (FALSE AND …) IS NOT TRUE
// 恒为 TRUE，仅添加无关凭证键也会误清受管键。两类行在 api_key/身份不变、凭证仅
// 无关变化时都必须保留 OpenCode 受管键。
func TestUpdateCredentialsOpenCodeGoUnrelatedCredentialChangeKeepsManagedState(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Now().UTC()

	create := func(name, platform string, credentials map[string]any) *service.Account {
		return mustCreateAccount(t, tx.Client(), &service.Account{
			Name: name, Platform: platform, Type: service.AccountTypeAPIKey,
			Credentials: credentials,
			Extra: map[string]any{
				service.OpenCodeGoUsageAutoRefreshExtraKey: true,
				service.OpenCodeGoUsageSnapshotExtraKey: map[string]any{
					"status": service.OpenCodeGoUsageStatusOK, "last_attempt_at": now, "next_refresh_at": now.Add(time.Hour),
				},
			},
		})
	}
	anthropicMount := create("opencode-mount-unrelated", service.PlatformAnthropic, map[string]any{
		"api_key": "mount-key", "base_url": "https://opencode.ai/zen/go",
	})
	platformGo := create("opencode-platform-unrelated", service.PlatformOpenCodeGo, map[string]any{
		"api_key": "platform-key", "account_mode": service.AccountModeGo,
	})

	// 挂载行：api_key 与官方基址都未变，仅新增无关凭证键。
	require.NoError(t, repo.UpdateCredentials(ctx, anthropicMount.ID, map[string]any{
		"api_key": "mount-key", "base_url": "https://opencode.ai/zen/go", "remark": "unrelated",
	}))
	// opencode_go 行：api_key 未变、模式仍为 Go，仅新增无关凭证键。
	require.NoError(t, repo.UpdateCredentials(ctx, platformGo.ID, map[string]any{
		"api_key": "platform-key", "account_mode": service.AccountModeGo, "remark": "unrelated",
	}))

	for _, id := range []int64{anthropicMount.ID, platformGo.ID} {
		loaded, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		require.Equal(t, true, loaded.Extra[service.OpenCodeGoUsageAutoRefreshExtraKey])
		snapshot, ok := loaded.Extra[service.OpenCodeGoUsageSnapshotExtraKey].(map[string]any)
		require.True(t, ok)
		require.Equal(t, service.OpenCodeGoUsageStatusOK, snapshot["status"])
	}
}

// F7：生产故障路径——前端脱敏普通编辑（incoming Extra 不含 OpenCode 受管键）经真实
// repo.Update 走 lockAndMergeAccountProbeExtra 的 SELECT/Scan/UPDATE 后，custom 生效
// 且两个受管键从锁定 DB 行回填；incoming Extra 伪造受管键时以锁定 DB 值为准。
// 旧代码（无 opencode 三列与回填分支）下：脱敏编辑会直接抹掉 DB 里的开关与快照，
// 伪造编辑会把伪造值原样写入，两条断言都会失败。
func TestUpdateOpenCodeGoManagedStateSurvivesDesensitizedEdit(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	now := time.Now().UTC()

	proxy := mustCreateProxy(t, tx.Client(), &service.Proxy{
		Name: "opencode-edit-proxy", Protocol: "http", Host: "127.0.0.1", Port: 3128,
		Username: "user", Password: "pass", Status: service.StatusActive,
	})
	account := mustCreateAccount(t, tx.Client(), &service.Account{
		Name: "opencode-desensitized-edit", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "edit-key", "base_url": "https://opencode.ai/zen/go/v1"},
		ProxyID:     &proxy.ID,
		Extra: map[string]any{
			service.OpenCodeGoUsageAutoRefreshExtraKey: true,
			service.OpenCodeGoUsageSnapshotExtraKey: map[string]any{
				"status": service.OpenCodeGoUsageStatusOK, "last_attempt_at": now, "next_refresh_at": now.Add(time.Hour),
			},
			"custom": "original",
		},
	})

	// 前端脱敏普通编辑：incoming Extra 不含两个受管键，只改 custom；base_url 用
	// :443 + 尾斜杠变体证明 normalized 身份判定（同 ID/platform/type/api_key/proxy）。
	edit, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	edit.Credentials = map[string]any{"api_key": "edit-key", "base_url": "https://opencode.ai:443/zen/go/v1/"}
	edit.Extra = map[string]any{"custom": "edited"}
	require.NoError(t, repo.Update(ctx, edit))

	loaded, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, "edited", loaded.Extra["custom"])
	require.Equal(t, true, loaded.Extra[service.OpenCodeGoUsageAutoRefreshExtraKey],
		"脱敏编辑不得抹掉 auto_refresh")
	snapshot, ok := loaded.Extra[service.OpenCodeGoUsageSnapshotExtraKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, service.OpenCodeGoUsageStatusOK, snapshot["status"], "脱敏编辑不得抹掉快照")

	// incoming Extra 伪造受管键：锁定 DB 值必须胜出。
	forged, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	forged.Extra = map[string]any{
		service.OpenCodeGoUsageAutoRefreshExtraKey: false,
		service.OpenCodeGoUsageSnapshotExtraKey:    map[string]any{"status": "forged"},
		"custom":                                   "edited-again",
	}
	require.NoError(t, repo.Update(ctx, forged))

	loaded, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, "edited-again", loaded.Extra["custom"])
	require.Equal(t, true, loaded.Extra[service.OpenCodeGoUsageAutoRefreshExtraKey],
		"伪造 auto_refresh 必须以锁定 DB 值为准")
	snapshot, ok = loaded.Extra[service.OpenCodeGoUsageSnapshotExtraKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, service.OpenCodeGoUsageStatusOK, snapshot["status"], "伪造快照必须以锁定 DB 值为准")
}

// openCodeGoUsagePolicyFallbackConfig 返回与生产 524 同形的合法额度保护配置。
// enabled=true 时必须同时提供 model_mapping，否则 service.ParseOpenCodeQuotaFallback
// 会把配置判成非法并让 GetOpenCodeQuotaFallback().Enabled 返回 false。
func openCodeGoUsagePolicyFallbackConfig() map[string]any {
	return map[string]any{
		"enabled":           true,
		"threshold_percent": 98,
		"model_mapping":     map[string]any{"deepseek-v4.1-flash": "step-5-preview"},
	}
}

// assertOpenCodeGoUsageIntegrationDatabase 证明当前连接真的是 integration harness
// 启动的 PostgreSQL，而不是被静默 skip 后的零值环境。专项 workflow 会断言该日志存在。
func assertOpenCodeGoUsageIntegrationDatabase(t *testing.T, q sqlQueryer) {
	t.Helper()
	var dbName, versionNumRaw string
	err := scanSingleRow(context.Background(), q,
		`SELECT current_database(), current_setting('server_version_num')`,
		nil, &dbName, &versionNumRaw)
	require.NoError(t, err, "integration harness guard query")
	require.Equal(t, "sub2api_test", dbName,
		"integration tests must run against the testcontainers database")
	versionNum, err := strconv.Atoi(versionNumRaw)
	require.NoError(t, err, "server_version_num must be numeric: %q", versionNumRaw)
	require.GreaterOrEqual(t, versionNum, 140000, "PostgreSQL >= 14 is required")
	t.Logf("integration harness: database=%s server_version_num=%s", dbName, versionNumRaw)
}

// requireOpenCodeGoUsageListDueNoError 把 *pq.Error 的 SQLSTATE 显式打进失败信息。
// lib/pq 的 Error() 只返回 "pq: <message>"，不含 SQLSTATE；不展开 pqErr.Code 时
// 专项 gate 无法区分 42703 回归红与连接/setup 失败。
func requireOpenCodeGoUsageListDueNoError(t *testing.T, err error, policyOnly bool) {
	t.Helper()
	if err == nil {
		return
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		t.Fatalf("ListDueOpenCodeGoUsageAccounts(policyAccountsOnly=%t) failed: SQLSTATE=%s message=%s err=%v",
			policyOnly, pqErr.Code, pqErr.Message, err)
	}
	t.Fatalf("ListDueOpenCodeGoUsageAccounts(policyAccountsOnly=%t) failed with non-SQL error: %v",
		policyOnly, err)
}

type openCodeGoUsagePolicyFixtures struct {
	accounts map[string]*service.Account
	keys     map[string]string
	dedupKey string
}

// expectedCounts 按 verification-plan §3.5 的矩阵返回每个 api_key 的期望行数。
// 同一 kDedup key 的两条 fixture（K/L）合并统计。
func (f *openCodeGoUsagePolicyFixtures) expectedCounts(policyOnly bool) map[string]int {
	keys := f.keys
	if policyOnly {
		return map[string]int{
			keys["A"]: 1, keys["B"]: 0, keys["C"]: 0, keys["D"]: 0, keys["E"]: 0,
			keys["F"]: 1, keys["G"]: 0, keys["H"]: 0, keys["I"]: 0, keys["J"]: 0,
			f.dedupKey: 1, keys["M"]: 1,
		}
	}
	return map[string]int{
		keys["A"]: 1, keys["B"]: 1, keys["C"]: 0, keys["D"]: 0, keys["E"]: 0,
		keys["F"]: 1, keys["G"]: 0, keys["H"]: 0, keys["I"]: 1, keys["J"]: 0,
		f.dedupKey: 1, keys["M"]: 1,
	}
}

// createOpenCodeGoUsagePolicyContractFixtures 在调用方自己的 testEntTx 事务内创建
// A–M 全套 fixture。每条 fixture 都显式设置 Type=apikey/Status=active 并在创建后断言，
// 因为 mustCreateAccount 对空 Type 的默认值是 AccountTypeOAuth。
func createOpenCodeGoUsagePolicyContractFixtures(t *testing.T, client *dbent.Client, now time.Time) *openCodeGoUsagePolicyFixtures {
	t.Helper()
	runID := fmt.Sprintf("ocq219-%d", time.Now().UnixNano())
	apiKey := func(label string) string { return "sk-" + runID + "-" + label }
	at := func(offset time.Duration) *time.Time {
		ts := now.Add(offset)
		return &ts
	}
	plainCredentials := func(key string) map[string]any {
		return map[string]any{"api_key": key}
	}
	policyCredentials := func(key string) map[string]any {
		return map[string]any{
			"api_key": key,
			service.OpenCodeQuotaFallbackCredentialKey: openCodeGoUsagePolicyFallbackConfig(),
		}
	}
	extraWithSnapshot := func(autoRefresh bool, fetchedAt, lastAttemptAt, nextRefreshAt time.Time) map[string]any {
		extra := map[string]any{
			service.OpenCodeGoUsageSnapshotExtraKey: map[string]any{
				"status":          service.OpenCodeGoUsageStatusOK,
				"fetched_at":      fetchedAt,
				"last_attempt_at": lastAttemptAt,
				"next_refresh_at": nextRefreshAt,
			},
		}
		if autoRefresh {
			extra[service.OpenCodeGoUsageAutoRefreshExtraKey] = true
		}
		return extra
	}
	autoRefreshExtra := func() map[string]any {
		return map[string]any{service.OpenCodeGoUsageAutoRefreshExtraKey: true}
	}

	fixtures := &openCodeGoUsagePolicyFixtures{
		accounts: make(map[string]*service.Account, 13),
		keys:     make(map[string]string, 13),
	}
	create := func(label, platform string, credentials, extra map[string]any, lastUsedAt *time.Time) *service.Account {
		t.Helper()
		account := mustCreateAccount(t, client, &service.Account{
			Name:        runID + "-" + label,
			Platform:    platform,
			Type:        service.AccountTypeAPIKey,
			Status:      service.StatusActive,
			Credentials: credentials,
			Extra:       extra,
			LastUsedAt:  lastUsedAt,
		})
		require.Equal(t, service.AccountTypeAPIKey, account.Type, "fixture %s must be apikey", label)
		require.Equal(t, service.StatusActive, account.Status, "fixture %s must be active", label)
		fixtures.accounts[label] = account
		fixtures.keys[label] = credentials["api_key"].(string)
		return account
	}

	// A: legacy opencode + account_mode 缺省 + auto_refresh 缺省 + policy enabled，
	// 无 snapshot / 无 last_used → fail-open 必入选。
	create("A", service.PlatformOpenCode, policyCredentials(apiKey("A")), map[string]any{}, nil)
	// B: opencode_go + account_mode 缺省 + 仅 auto_refresh，无 snapshot。
	create("B", service.PlatformOpenCodeGo, plainCredentials(apiKey("B")), autoRefreshExtra(), at(-10*time.Minute))
	// C: 非策略且无 auto_refresh → 两种模式都排除。
	create("C", service.PlatformOpenCodeGo, plainCredentials(apiKey("C")), map[string]any{}, nil)
	// D/E: Zen 模式（opencode_go / legacy）必须排除。
	zenGo := policyCredentials(apiKey("D"))
	zenGo["account_mode"] = service.AccountModeZen
	create("D", service.PlatformOpenCodeGo, zenGo, autoRefreshExtra(), nil)
	zenLegacy := policyCredentials(apiKey("E"))
	zenLegacy["account_mode"] = service.AccountModeZen
	create("E", service.PlatformOpenCode, zenLegacy, autoRefreshExtra(), nil)
	// F: 策略账号，ok 快照空闲且 fetched+maxWait 已到期 → 必入选。
	create("F", service.PlatformOpenCodeGo, policyCredentials(apiKey("F")),
		extraWithSnapshot(false, now.Add(-20*time.Minute), now.Add(-20*time.Minute), now.Add(-5*time.Minute)),
		at(-30*time.Minute))
	// G: 策略账号，ok 快照空闲但 fetched+maxWait 未到期 → 不入选。
	create("G", service.PlatformOpenCodeGo, policyCredentials(apiKey("G")),
		extraWithSnapshot(false, now.Add(-5*time.Minute), now.Add(-5*time.Minute), now.Add(10*time.Minute)),
		at(-20*time.Minute))
	// H: 非策略账号的 idle ok 快照不得走 policy 分支 → 两种模式都排除。
	create("H", service.PlatformOpenCodeGo, plainCredentials(apiKey("H")),
		extraWithSnapshot(true, now.Add(-20*time.Minute), now.Add(-20*time.Minute), now.Add(-5*time.Minute)),
		at(-30*time.Minute))
	// I: 非策略活动驱动到期（last_used 晚于 fetched）→ false 入选 / true 排除。
	create("I", service.PlatformOpenCodeGo, plainCredentials(apiKey("I")),
		extraWithSnapshot(true, now.Add(-10*time.Minute), now.Add(-10*time.Minute), now.Add(5*time.Minute)),
		at(-2*time.Minute))
	// J: age 未到 debounce（last_used+1m 恒晚于同一 now）→ 不入选。
	create("J", service.PlatformOpenCodeGo, plainCredentials(apiKey("J")),
		extraWithSnapshot(true, now.Add(-10*time.Minute), now.Add(-10*time.Minute), now.Add(5*time.Minute)),
		at(-10*time.Second))
	// K/L: 同一 api_key 的两条 eligible 行。K 必须先创建，保证 K.ID < L.ID。
	fixtures.dedupKey = apiKey("K")
	create("K", service.PlatformOpenCodeGo, policyCredentials(fixtures.dedupKey), map[string]any{}, at(-10*time.Minute))
	create("L", service.PlatformOpenCode, policyCredentials(fixtures.dedupKey), map[string]any{}, at(-time.Minute))
	// M: 显式 go 模式的正向控制。
	goMode := policyCredentials(apiKey("M"))
	goMode["account_mode"] = service.AccountModeGo
	create("M", service.PlatformOpenCodeGo, goMode, map[string]any{}, nil)
	return fixtures
}

// TestListDueOpenCodeGoUsageAccountsPolicyContract 是本修复的决定性真实 PostgreSQL
// 回归：直接执行 repository 方法，让 PostgreSQL 解析 SQL。旧 v0.1.218 代码在 timed
// 作用域重新展开 quota_fallback 表达式，引用不存在的 credentials 列，报 SQLSTATE
// 42703；本测试的两个子测试各自使用独立 testEntTx，因此旧代码下必须各自独立出现
// 42703，而不是第二个子测试退化为 25P02。
func TestListDueOpenCodeGoUsageAccountsPolicyContract(t *testing.T) {
	for _, policyOnly := range []bool{false, true} {
		policyOnly := policyOnly
		t.Run(fmt.Sprintf("policyAccountsOnly=%t", policyOnly), func(t *testing.T) {
			ctx := context.Background()
			tx := testEntTx(t)
			assertOpenCodeGoUsageIntegrationDatabase(t, tx)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			// 每个子测试只捕获一次 now；fixture 与 ListDue 都使用这同一个值。
			now := time.Now().UTC()
			fixtures := createOpenCodeGoUsagePolicyContractFixtures(t, tx.Client(), now)

			accounts, err := repo.ListDueOpenCodeGoUsageAccounts(
				ctx, now, time.Minute, 15*time.Minute, 10000, policyOnly)
			requireOpenCodeGoUsageListDueNoError(t, err, policyOnly)

			expected := fixtures.expectedCounts(policyOnly)
			got := make(map[string][]service.Account, len(expected))
			for _, account := range accounts {
				key, _ := account.Credentials["api_key"].(string)
				if _, tracked := expected[key]; !tracked {
					continue // 共享测试库中其它测试留下的已提交账号
				}
				got[key] = append(got[key], account)
			}
			for key, want := range expected {
				bucket := got[key]
				ids := make([]int64, 0, len(bucket))
				for _, account := range bucket {
					ids = append(ids, account.ID)
				}
				require.Len(t, bucket, want, "policyAccountsOnly=%t key=%s ids=%v", policyOnly, key, ids)
				for _, account := range bucket {
					require.Equal(t, service.AccountTypeAPIKey, account.Type, "returned account %d type", account.ID)
					require.Equal(t, service.StatusActive, account.Status, "returned account %d status", account.ID)
					require.Equal(t, key, account.Credentials["api_key"], "returned account %d key", account.ID)
				}
			}

			dedup := got[fixtures.dedupKey]
			require.Len(t, dedup, 1, "same api_key must yield at most one due candidate")
			require.Equal(t, fixtures.accounts["K"].ID, dedup[0].ID,
				"same due_class/due_at must pick the lowest id (K created before L)")
			require.NotNil(t, dedup[0].LastUsedAt)
			require.WithinDuration(t, now.Add(-time.Minute), *dedup[0].LastUsedAt, time.Second,
				"returned LastUsedAt must be the group MAX(last_used_at)")

			require.Nil(t, got[fixtures.keys["A"]][0].LastUsedAt, "fixture A has no activity")

			for _, label := range []string{"A", "F", "K", "M"} {
				require.True(t, fixtures.accounts[label].GetOpenCodeQuotaFallback().Enabled,
					"fixture %s must be policy-enabled", label)
			}
			for _, label := range []string{"B", "I"} {
				require.False(t, fixtures.accounts[label].GetOpenCodeQuotaFallback().Enabled,
					"fixture %s must not be policy-enabled", label)
			}
		})
	}
}

const openCodeGoUsageRealRepositoryFixture = `{"usage":{"rolling":{"status":"ok","percent":6,"resetsAt":"2026-08-13T18:26:39.281Z"},"weekly":{"status":"ok","percent":2,"resetsAt":"2026-08-17T00:00:00.281Z"},"monthly":{"status":"ok","percent":1,"resetsAt":"2026-09-13T13:24:47.281Z"}}}`

type openCodeGoUsageControlledUpstream struct {
	body  []byte
	calls atomic.Int64
}

func (u *openCodeGoUsageControlledUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(u.body)),
		Request:    req,
	}, nil
}

func (u *openCodeGoUsageControlledUpstream) DoWithTLS(
	req *http.Request, proxyURL string, accountID int64, accountConcurrency int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

// TestOpenCodeGoUsageRefreshWritesSnapshotThroughRealRepository 覆盖运行时链路：
// service.runner 经受控 transport 拿到 200 响应后，必须通过**真实 repository**
// （事务内 ent client）把解码后的快照写回 accounts.extra，而不是只在内存里更新。
//
// 这里使用 service.Refresh（内部就是 RunDue 调用的 refreshAccount）而不是 RunDue
// 本身：RunDue 的 ListDue 固定 limit=20，共享测试库中其它已提交候选会令“本轮 fixture
// 是否进入前 20”不确定；ListDue 的选择语义（含 policyOnly 两种模式）由上面的契约
// 回归在 limit=10000 下确定性覆盖。
func TestOpenCodeGoUsageRefreshWritesSnapshotThroughRealRepository(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	assertOpenCodeGoUsageIntegrationDatabase(t, tx)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)

	runID := fmt.Sprintf("ocq219-runtime-%d", time.Now().UnixNano())
	account := mustCreateAccount(t, tx.Client(), &service.Account{
		Name:     runID,
		Platform: service.PlatformOpenCodeGo,
		Type:     service.AccountTypeAPIKey,
		Status:   service.StatusActive,
		Credentials: map[string]any{
			"api_key": "sk-" + runID,
			service.OpenCodeQuotaFallbackCredentialKey: openCodeGoUsagePolicyFallbackConfig(),
		},
		Extra: map[string]any{},
	})
	require.Equal(t, service.AccountTypeAPIKey, account.Type)
	require.Equal(t, service.StatusActive, account.Status)

	upstream := &openCodeGoUsageControlledUpstream{body: []byte(openCodeGoUsageRealRepositoryFixture)}
	settingRepo := NewSettingRepository(tx.Client())
	svc := service.NewOpenCodeGoUsageService(repo, upstream, service.NewSettingService(settingRepo, nil))

	state, err := svc.Refresh(ctx, account.ID)
	require.NoError(t, err, "service runner refresh")
	require.NotNil(t, state.Snapshot)
	require.Equal(t, service.OpenCodeGoUsageStatusOK, state.Snapshot.Status)
	require.Equal(t, int64(1), upstream.calls.Load(), "controlled transport must be called exactly once")

	// 直接从数据库读回 accounts.extra，证明快照真的落库（而不是只在内存 state 上）。
	var extraJSON string
	err = scanSingleRow(ctx, tx, `SELECT extra::text FROM accounts WHERE id = $1`, []any{account.ID}, &extraJSON)
	require.NoError(t, err)
	var stored map[string]any
	require.NoError(t, json.Unmarshal([]byte(extraJSON), &stored))
	rawSnapshot, ok := stored[service.OpenCodeGoUsageSnapshotExtraKey].(map[string]any)
	require.True(t, ok, "opencode_go_usage_snapshot must be persisted in accounts.extra: %s", extraJSON)
	require.Equal(t, service.OpenCodeGoUsageStatusOK, rawSnapshot["status"])
	require.NotEmpty(t, rawSnapshot["last_attempt_at"])
	require.NotEmpty(t, rawSnapshot["next_refresh_at"])
	rawData, ok := rawSnapshot["data"].(map[string]any)
	require.True(t, ok, "ok snapshot must persist a non-empty data object: %s", extraJSON)
	for _, window := range []string{"rolling", "weekly", "monthly"} {
		_, present := rawData[window]
		require.True(t, present, "snapshot data must contain %s window: %s", window, extraJSON)
	}
}
