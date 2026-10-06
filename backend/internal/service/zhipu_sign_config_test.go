//go:build unit

package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/zcodesign"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/hkdf"
)

// zhipuSignSettingRepoStub 是 SettingRepository 的测试替身：承载 system_settings 的
// 键值语义，并记录每次写入，供「非法值不落库」的断言使用。
type zhipuSignSettingRepoStub struct {
	values map[string]string
	writes []map[string]string
}

func newZhipuSignSettingRepoStub(values map[string]string) *zhipuSignSettingRepoStub {
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return &zhipuSignSettingRepoStub{values: copied}
}

func (s *zhipuSignSettingRepoStub) Get(_ context.Context, key string) (*Setting, error) {
	value, ok := s.values[key]
	if !ok {
		return nil, ErrSettingNotFound
	}
	return &Setting{Key: key, Value: value}, nil
}

func (s *zhipuSignSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	value, ok := s.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (s *zhipuSignSettingRepoStub) Set(_ context.Context, key, value string) error {
	s.values[key] = value
	return nil
}

func (s *zhipuSignSettingRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	found := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := s.values[key]; ok {
			found[key] = value
		}
	}
	return found, nil
}

func (s *zhipuSignSettingRepoStub) SetMultiple(_ context.Context, settings map[string]string) error {
	written := make(map[string]string, len(settings))
	for key, value := range settings {
		written[key] = value
		s.values[key] = value
	}
	s.writes = append(s.writes, written)
	return nil
}

func (s *zhipuSignSettingRepoStub) GetAll(_ context.Context) (map[string]string, error) {
	all := make(map[string]string, len(s.values))
	for key, value := range s.values {
		all[key] = value
	}
	return all, nil
}

func (s *zhipuSignSettingRepoStub) Delete(_ context.Context, key string) error {
	delete(s.values, key)
	return nil
}

// zhipuSignTestGatewayConfig 是 #01 定义并已由 viper 落默认值的 gateway.zhipu 配置
// （config.go 的 SetDefaults 逐字对应），测试用它当「配置默认值」事实源。
func zhipuSignTestGatewayConfig() *config.Config {
	return &config.Config{Gateway: config.GatewayConfig{Zhipu: config.GatewayZhipuConfig{
		SignV4Enabled:                    true,
		SignClientVersion:                "0.16.9",
		SignKeyTTLMinutes:                1440,
		CredentialCheckIntervalMinutes:   60,
		ZCodeMinCallIntervalSeconds:      30,
		ResetStatusCacheMinutes:          10,
		SignFailPolicy:                   "open",
		SignAlertEnabled:                 true,
		SignReconcileIntervalHours:       6,
		SignReconcileDeviationThreshold:  0.70,
		SignPowBits:                      8,
		SignHandshakeBackoffSeconds:      30,
		SignAccountCircuitBreakThreshold: 10,
	}}}
}

// 未写入任何覆盖键时，生效值必须是 gateway.zhipu 的配置值（票 28：配置化键的
// 事实源仍是 cfg.Gateway.Zhipu，system_settings 只是运行期覆盖层）。
func TestZhipuSignConfigEffectiveWithoutOverrides(t *testing.T) {
	t.Parallel()

	svc := NewZhipuSignConfigService(zhipuSignTestGatewayConfig(), newZhipuSignSettingRepoStub(nil), nil, nil)

	got := svc.Effective(context.Background())

	require.Equal(t, zhipuSignExpectedDefaults(), got)
	require.Empty(t, svc.ConfigView(context.Background()).OverriddenKeys,
		"没写过 system_settings 时不得有“已覆盖”键")
}

// zhipuSignExpectedDefaults 是 #01 配置默认值的独立事实源（viper SetDefaults 逐字对应）。
func zhipuSignExpectedDefaults() ZhipuSignConfig {
	return ZhipuSignConfig{
		SignV4Enabled:                    true,
		SignClientVersion:                "0.16.9",
		SignPowBits:                      8,
		SignKeyTTLMinutes:                1440,
		SignHandshakeBackoffSeconds:      30,
		SignAccountCircuitBreakThreshold: 10,
		SignFailPolicy:                   ZhipuSignFailPolicyOpen,
		SignAlertEnabled:                 true,
		SignReconcileIntervalHours:       6,
		SignReconcileDeviationThreshold:  0.70,
	}
}

// 运行层覆盖：system_settings 的 gateway.zhipu.sign_* 优先于 cfg.Gateway.Zhipu，
// 且全部 10 个键都可覆盖；fail 策略大小写归一为小写枚举。
func TestZhipuSignConfigEffectiveAppliesStoredOverrides(t *testing.T) {
	t.Parallel()

	repo := newZhipuSignSettingRepoStub(map[string]string{
		SettingKeyZhipuSignV4Enabled:                    "false",
		SettingKeyZhipuSignClientVersion:                "0.17.0",
		SettingKeyZhipuSignPowBits:                      "4",
		SettingKeyZhipuSignKeyTTLMinutes:                "60",
		SettingKeyZhipuSignHandshakeBackoffSeconds:      "5",
		SettingKeyZhipuSignAccountCircuitBreakThreshold: "3",
		SettingKeyZhipuSignFailPolicy:                   "CLOSED",
		SettingKeyZhipuSignAlertEnabled:                 "false",
		SettingKeyZhipuSignReconcileIntervalHours:       "12",
		SettingKeyZhipuSignReconcileDeviationThreshold:  "0.9",
	})
	svc := NewZhipuSignConfigService(zhipuSignTestGatewayConfig(), repo, nil, nil)

	view := svc.ConfigView(context.Background())

	require.Equal(t, ZhipuSignConfig{
		SignV4Enabled:                    false,
		SignClientVersion:                "0.17.0",
		SignPowBits:                      4,
		SignKeyTTLMinutes:                60,
		SignHandshakeBackoffSeconds:      5,
		SignAccountCircuitBreakThreshold: 3,
		SignFailPolicy:                   ZhipuSignFailPolicyClosed,
		SignAlertEnabled:                 false,
		SignReconcileIntervalHours:       12,
		SignReconcileDeviationThreshold:  0.9,
	}, view.ZhipuSignConfig)
	require.Equal(t, []string{
		"gateway.zhipu.sign_account_circuit_break_threshold",
		"gateway.zhipu.sign_alert_enabled",
		"gateway.zhipu.sign_client_version",
		"gateway.zhipu.sign_fail_policy",
		"gateway.zhipu.sign_handshake_backoff_seconds",
		"gateway.zhipu.sign_key_ttl_minutes",
		"gateway.zhipu.sign_pow_bits",
		"gateway.zhipu.sign_reconcile_deviation_threshold",
		"gateway.zhipu.sign_reconcile_interval_hours",
		"gateway.zhipu.sign_v4_enabled",
	}, view.OverriddenKeys, "被覆盖键必须排序稳定，前端按它显示“已修改”")
}

// 绕过管理端直接改库写进非法值时：该键按「未覆盖」处理（退回部署层默认），
// 绝不把非法值推给签名器；也不因为读到一个坏值就丢掉其它好值。
func TestZhipuSignConfigEffectiveIgnoresInvalidStoredValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		key   string
		value string
	}{
		{name: "pow bits above the protocol range", key: SettingKeyZhipuSignPowBits, value: "99"},
		{name: "pow bits below zero", key: SettingKeyZhipuSignPowBits, value: "-1"},
		{name: "blank client version", key: SettingKeyZhipuSignClientVersion, value: "   "},
		{name: "client version with url shape", key: SettingKeyZhipuSignClientVersion, value: "https://open.bigmodel.cn/x"},
		{name: "unknown fail policy", key: SettingKeyZhipuSignFailPolicy, value: "maybe"},
		{name: "zero key ttl", key: SettingKeyZhipuSignKeyTTLMinutes, value: "0"},
		{name: "negative handshake backoff", key: SettingKeyZhipuSignHandshakeBackoffSeconds, value: "-30"},
		{name: "non numeric circuit threshold", key: SettingKeyZhipuSignAccountCircuitBreakThreshold, value: "ten"},
		{name: "deviation above one", key: SettingKeyZhipuSignReconcileDeviationThreshold, value: "1.5"},
		{name: "unparsable bool", key: SettingKeyZhipuSignV4Enabled, value: "yes-please"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := newZhipuSignSettingRepoStub(map[string]string{tc.key: tc.value})
			svc := NewZhipuSignConfigService(zhipuSignTestGatewayConfig(), repo, nil, nil)

			view := svc.ConfigView(context.Background())

			require.Equal(t, zhipuSignExpectedDefaults(), view.ZhipuSignConfig)
			require.Empty(t, view.OverriddenKeys)
		})
	}
}

func zhipuSignPtr[T any](value T) *T { return &value }

// 强校验（design M3.1(e)）：非法值必须拒绝写入、错误可读（指明键名与边界），
// 且磁盘上的旧值原样保持——非法配置会直接破坏全量账号的签名，绝不能落库。
func TestZhipuSignConfigUpdateRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		update  ZhipuSignConfigUpdate
		wantKey string
	}{
		{
			name:    "pow bits above the protocol range",
			update:  ZhipuSignConfigUpdate{SignPowBits: zhipuSignPtr(17)},
			wantKey: "sign_pow_bits",
		},
		{
			name:    "negative pow bits",
			update:  ZhipuSignConfigUpdate{SignPowBits: zhipuSignPtr(-1)},
			wantKey: "sign_pow_bits",
		},
		{
			name:    "empty client version",
			update:  ZhipuSignConfigUpdate{SignClientVersion: zhipuSignPtr("   ")},
			wantKey: "sign_client_version",
		},
		{
			name:    "client version that is not semver",
			update:  ZhipuSignConfigUpdate{SignClientVersion: zhipuSignPtr("v0.16.9 rc1")},
			wantKey: "sign_client_version",
		},
		{
			name:    "unknown fail policy",
			update:  ZhipuSignConfigUpdate{SignFailPolicy: zhipuSignPtr("fail-open")},
			wantKey: "sign_fail_policy",
		},
		{
			name:    "blank fail policy",
			update:  ZhipuSignConfigUpdate{SignFailPolicy: zhipuSignPtr("")},
			wantKey: "sign_fail_policy",
		},
		{
			name:    "zero key ttl",
			update:  ZhipuSignConfigUpdate{SignKeyTTLMinutes: zhipuSignPtr(0)},
			wantKey: "sign_key_ttl_minutes",
		},
		{
			name:    "negative handshake backoff",
			update:  ZhipuSignConfigUpdate{SignHandshakeBackoffSeconds: zhipuSignPtr(-30)},
			wantKey: "sign_handshake_backoff_seconds",
		},
		{
			name:    "backoff above the safety bound",
			update:  ZhipuSignConfigUpdate{SignHandshakeBackoffSeconds: zhipuSignPtr(3601)},
			wantKey: "sign_handshake_backoff_seconds",
		},
		{
			name:    "zero circuit break threshold",
			update:  ZhipuSignConfigUpdate{SignAccountCircuitBreakThreshold: zhipuSignPtr(0)},
			wantKey: "sign_account_circuit_break_threshold",
		},
		{
			name:    "reconcile interval out of range",
			update:  ZhipuSignConfigUpdate{SignReconcileIntervalHours: zhipuSignPtr(169)},
			wantKey: "sign_reconcile_interval_hours",
		},
		{
			name:    "reconcile deviation above one",
			update:  ZhipuSignConfigUpdate{SignReconcileDeviationThreshold: zhipuSignPtr(1.5)},
			wantKey: "sign_reconcile_deviation_threshold",
		},
		{
			name:    "reconcile deviation at zero",
			update:  ZhipuSignConfigUpdate{SignReconcileDeviationThreshold: zhipuSignPtr(0.0)},
			wantKey: "sign_reconcile_deviation_threshold",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := newZhipuSignSettingRepoStub(map[string]string{SettingKeyZhipuSignPowBits: "4"})
			svc := NewZhipuSignConfigService(zhipuSignTestGatewayConfig(), repo, nil, nil)

			_, err := svc.Update(context.Background(), tc.update)

			require.Error(t, err)
			require.Equal(t, "ZHIPU_SIGN_CONFIG_INVALID", infraerrors.Reason(err))
			require.Equal(t, 400, infraerrors.Code(err))
			require.Contains(t, infraerrors.Message(err), tc.wantKey, "错误必须指明具体键，管理端才可读")
			require.Empty(t, repo.writes, "非法值绝不落库")
			require.Equal(t, "4", repo.values[SettingKeyZhipuSignPowBits], "旧值必须原样保持")
		})
	}
}

// 没有提供任何键的更新请求不算一次变更：明确报错，而不是静默成功或全量覆写。
func TestZhipuSignConfigUpdateRejectsEmptyRequest(t *testing.T) {
	t.Parallel()

	repo := newZhipuSignSettingRepoStub(nil)
	svc := NewZhipuSignConfigService(zhipuSignTestGatewayConfig(), repo, nil, nil)

	_, err := svc.Update(context.Background(), ZhipuSignConfigUpdate{})

	require.Error(t, err)
	require.Equal(t, "ZHIPU_SIGN_CONFIG_EMPTY", infraerrors.Reason(err))
	require.Empty(t, repo.writes)
}

// 部分更新：只写传入的键（其余键在库里的值与生效值都不动），返回更新后的生效视图，
// 覆盖键列表同步刷新（管理端一次调用即可回显，不需要再发一次 GET）。
func TestZhipuSignConfigUpdatePersistsOnlyProvidedKeys(t *testing.T) {
	t.Parallel()

	repo := newZhipuSignSettingRepoStub(map[string]string{SettingKeyZhipuSignKeyTTLMinutes: "60"})
	svc := NewZhipuSignConfigService(zhipuSignTestGatewayConfig(), repo, nil, nil)

	view, err := svc.Update(context.Background(), ZhipuSignConfigUpdate{
		SignFailPolicy: zhipuSignPtr("CLOSED"),
		SignPowBits:    zhipuSignPtr(2),
	})

	require.NoError(t, err)
	require.Len(t, repo.writes, 1)
	require.Equal(t, map[string]string{
		SettingKeyZhipuSignFailPolicy: ZhipuSignFailPolicyClosed,
		SettingKeyZhipuSignPowBits:    "2",
	}, repo.writes[0], "只写传入的键，且 fail 策略归一为小写枚举")
	require.Equal(t, ZhipuSignFailPolicyClosed, view.SignFailPolicy)
	require.Equal(t, 2, view.SignPowBits)
	require.Equal(t, 60, view.SignKeyTTLMinutes, "未传的键保持库中旧值")
	require.Equal(t, "0.16.9", view.SignClientVersion, "未传的键保持部署层默认")
	require.Equal(t, []string{
		SettingKeyZhipuSignFailPolicy,
		SettingKeyZhipuSignKeyTTLMinutes,
		SettingKeyZhipuSignPowBits,
	}, view.OverriddenKeys)
}

// zhipuSignConfigTestAPIKey 是测试凭据：形态与真实 api key 一致（<apiKeyId>.<secret with dots>）。
const zhipuSignConfigTestAPIKey = "2f8c1d7a4b6e9031.s3cr3t-part.with.dot"

const zhipuSignConfigTestAPIKeyID = "2f8c1d7a4b6e9031"

// zhipuSignTestHandshakeUpstream 是握手端点的测试替身（只实现 zcodesign.HTTPDoer）：
// 为每个 apiKey 现生成一对 Ed25519 密钥，按协议口径封装成 privateCipher 返回，
// 并保留公钥供测试独立验签。它不复用生产代码，是「上游行为」的独立事实源。
type zhipuSignTestHandshakeUpstream struct {
	mu         sync.Mutex
	calls      int
	publicKeys map[string]ed25519.PublicKey
}

func newZhipuSignTestHandshakeUpstream() *zhipuSignTestHandshakeUpstream {
	return &zhipuSignTestHandshakeUpstream{publicKeys: make(map[string]ed25519.PublicKey)}
}

func (u *zhipuSignTestHandshakeUpstream) Do(req *http.Request) (*http.Response, error) {
	apiKey := req.Header.Get("Authorization")
	apiKeyID, secret, found := strings.Cut(apiKey, ".")
	if !found || apiKeyID == "" || secret == "" {
		return nil, errors.New("test handshake upstream: malformed api key")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	privateCipher, err := zhipuSignTestSealPrivateKey(secret, apiKeyID, privateKey)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.calls++
	u.publicKeys[apiKeyID] = publicKey
	u.mu.Unlock()

	body := fmt.Sprintf(`{"code":200,"data":{"privateCipher":%q}}`, privateCipher)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (u *zhipuSignTestHandshakeUpstream) handshakeCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *zhipuSignTestHandshakeUpstream) publicKey(apiKeyID string) ed25519.PublicKey {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.publicKeys[apiKeyID]
}

// zhipuSignTestSealPrivateKey 按 zcode 协议口径封装私钥：
// HKDF-SHA256(secret, salt=WD_CLIENT_SIGN_KDF_SALT, info=ed25519_priv) 作 AES-256-GCM 密钥，
// aad=apiKeyId，明文是 PKCS#8 私钥的 base64，载荷是 base64(iv || ciphertext||tag)。
func zhipuSignTestSealPrivateKey(secret, apiKeyID string, privateKey ed25519.PrivateKey) (string, error) {
	derived := make([]byte, 32)
	reader := hkdf.New(sha256.New, []byte(secret), []byte("WD_CLIENT_SIGN_KDF_SALT"), []byte("ed25519_priv"))
	if _, err := io.ReadFull(reader, derived); err != nil {
		return "", err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	plaintext := base64.StdEncoding.EncodeToString(pkcs8)
	sealed := gcm.Seal(nil, iv, []byte(plaintext), []byte(apiKeyID))
	return base64.StdEncoding.EncodeToString(append(iv, sealed...)), nil
}

// 热更新验收（design M3.1(e)）：管理端改 sign_client_version / sign_pow_bits 后，
// **下一次 Sign** 就用新值签名，且进程未重启、已握手的私钥不被作废。
// 断言落在签名串本身（X-Client-Version 头 + PoW 候选）并对签名做独立 Ed25519 验签。
func TestZhipuSignConfigUpdateAppliesToSignerOnTheNextSign(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	upstream := newZhipuSignTestHandshakeUpstream()
	signer := zcodesign.NewSigner("https://open.bigmodel.cn", "0.16.9", time.Hour, upstream)
	svc, hotSigner := ProvideZhipuSignRuntime(zhipuSignTestGatewayConfig(), newZhipuSignSettingRepoStub(nil), nil, signer)

	before := http.Header{}
	require.NoError(t, hotSigner.Sign(ctx, zhipuSignConfigTestAPIKey, "session-before", before))
	require.Equal(t, "0.16.9", before.Get(zcodesign.HeaderClientVersion))
	require.Equal(t, 1, upstream.handshakeCount())

	view, err := svc.Update(ctx, ZhipuSignConfigUpdate{
		SignClientVersion: zhipuSignPtr("0.17.0"),
		SignPowBits:       zhipuSignPtr(0),
	})
	require.NoError(t, err)
	require.Equal(t, "0.17.0", view.SignClientVersion)
	require.Equal(t, 0, view.SignPowBits)

	// 同一个进程、同一个 Signer 实例：没有重启。
	after := http.Header{}
	require.NoError(t, hotSigner.Sign(ctx, zhipuSignConfigTestAPIKey, "session-after", after))

	require.Equal(t, "0.17.0", after.Get(zcodesign.HeaderClientVersion),
		"sign_client_version 必须在下一个请求生效")
	require.Equal(t, after.Get(zcodesign.HeaderClientNonce)+"00000000", after.Get(zcodesign.HeaderClientPow),
		"sign_pow_bits=0 必须在下一个请求生效（首个 PoW 候选即命中）")
	require.Equal(t, 1, upstream.handshakeCount(), "改协议参数不得作废已握手的私钥")

	publicKey := upstream.publicKey(zhipuSignConfigTestAPIKeyID)
	require.NotNil(t, publicKey)
	signature, err := base64.StdEncoding.DecodeString(after.Get(zcodesign.HeaderClientSig))
	require.NoError(t, err)
	message := strings.Join([]string{
		zhipuSignConfigTestAPIKeyID,
		after.Get(zcodesign.HeaderClientTs),
		after.Get(zcodesign.HeaderClientVersion),
		after.Get(zcodesign.HeaderSessionID),
		after.Get(zcodesign.HeaderClientNonce),
	}, "\n")
	require.True(t, ed25519.Verify(publicKey, []byte(message), signature),
		"签名必须覆盖新版本（版本是签名消息的一部分）")
}

// zhipuSignRuntimeStub 是签名器接缝的测试替身：记录推送的选项并返回预设缓存状态。
type zhipuSignRuntimeStub struct {
	mu       sync.Mutex
	applied  []zcodesign.SignerOptions
	statuses map[string]zcodesign.KeyStatus
}

func (s *zhipuSignRuntimeStub) SetOptions(options zcodesign.SignerOptions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied = append(s.applied, options)
}

func (s *zhipuSignRuntimeStub) KeyStatus(apiKeyID string) zcodesign.KeyStatus {
	return s.statuses[apiKeyID]
}

func (s *zhipuSignRuntimeStub) appliedOptions() []zcodesign.SignerOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]zcodesign.SignerOptions(nil), s.applied...)
}

// zhipuSignAccountListerStub 是账号来源替身：只实现 ListByPlatform 一个方法。
type zhipuSignAccountListerStub struct {
	accounts []Account
	err      error
}

func (s *zhipuSignAccountListerStub) ListByPlatform(_ context.Context, _ string) ([]Account, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.accounts, nil
}

// zhipuSignConfigTestAccount 构造一个「登录托管 + 已灰度签名」的智谱账号。
func zhipuSignConfigTestAccount(id int64, name, apiKey string, sign string) Account {
	credentials := map[string]any{
		"auth_flow":     "bigmodel_oauth",
		"api_key":       apiKey,
		"access_token":  "access-token-value",
		"zcodejwttoken": "zcodejwt-value",
	}
	if sign != "" {
		credentials["zcode_client_sign"] = sign
	}
	return Account{
		ID:          id,
		Name:        name,
		Platform:    PlatformZhipu,
		Type:        AccountTypeAPIKey,
		Credentials: credentials,
	}
}

// 状态读取面（design M6 第一条）：全局开关 + fail 策略的生效值，以及每个「启用账号」
// 的私钥缓存状态；未握手账号显示未缓存，非签名账号不出现；按 account_id 升序。
func TestZhipuSignConfigStatusProjectsTheSignerKeyState(t *testing.T) {
	t.Parallel()

	handshakedAt := time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)
	runtime := &zhipuSignRuntimeStub{statuses: map[string]zcodesign.KeyStatus{
		zhipuSignConfigTestAPIKeyID: {
			Cached:          true,
			LastHandshakeAt: handshakedAt,
			ExpiresAt:       handshakedAt.Add(24 * time.Hour),
		},
		"deadbeefdeadbeef": {Cached: false, ConsecutiveFailures: 3},
	}}
	accounts := []Account{
		zhipuSignConfigTestAccount(2, "zhipu-signed-b", "deadbeefdeadbeef.secret-b", "v4"),
		zhipuSignConfigTestAccount(1, "zhipu-signed-a", zhipuSignConfigTestAPIKey, "v4"),
		zhipuSignConfigTestAccount(3, "zhipu-not-graduated", "ccccccccccccccc3.secret-c", ""),
		{
			ID: 4, Name: "zhipu-manual-api-key", Platform: PlatformZhipu, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "ddddddddddddddd4.secret-d", "zcode_client_sign": "v4"},
		},
	}
	svc := NewZhipuSignConfigService(
		zhipuSignTestGatewayConfig(),
		newZhipuSignSettingRepoStub(nil),
		&zhipuSignAccountListerStub{accounts: accounts},
		runtime,
	)

	status, err := svc.Status(context.Background())

	require.NoError(t, err)
	require.True(t, status.SignV4Enabled)
	require.Equal(t, ZhipuSignFailPolicyOpen, status.SignFailPolicy)
	require.Len(t, status.Accounts, 2, "只有登录托管且已灰度的账号进入状态列表")

	require.Equal(t, int64(1), status.Accounts[0].AccountID, "账号按 id 升序")
	require.Equal(t, "zhipu-signed-a", status.Accounts[0].AccountName)
	require.True(t, status.Accounts[0].KeyCached)
	require.NotNil(t, status.Accounts[0].LastHandshakeAt)
	require.Equal(t, handshakedAt, *status.Accounts[0].LastHandshakeAt)
	require.NotNil(t, status.Accounts[0].KeyExpiresAt)
	require.Zero(t, status.Accounts[0].ConsecutiveFailures)

	require.Equal(t, int64(2), status.Accounts[1].AccountID)
	require.False(t, status.Accounts[1].KeyCached, "未握手账号显示未缓存")
	require.Nil(t, status.Accounts[1].LastHandshakeAt, "未握手账号没有上次握手时间")
	require.Nil(t, status.Accounts[1].KeyExpiresAt)
	require.Equal(t, 3, status.Accounts[1].ConsecutiveFailures)

	// TODO(#24)：熔断字段结构齐备但标记为「未接线」，前端不得据此显示「未熔断」。
	require.False(t, status.Accounts[0].CircuitBreakTripped)
	require.Empty(t, status.Accounts[0].CircuitBreakReason)
	require.False(t, status.Accounts[0].RuntimeStateAvailable)
}

// 负向断言：状态响应里不得出现任何凭据或签名材料（apiKey 全串、secret、access_token、
// zcode jwt，乃至 apiKeyId 片段），也不得出现私钥缓存状态以外的账号字段。
func TestZhipuSignConfigStatusNeverLeaksCredentials(t *testing.T) {
	t.Parallel()

	runtime := &zhipuSignRuntimeStub{statuses: map[string]zcodesign.KeyStatus{
		zhipuSignConfigTestAPIKeyID: {Cached: true, LastHandshakeAt: time.Unix(0, 0).UTC()},
	}}
	svc := NewZhipuSignConfigService(
		zhipuSignTestGatewayConfig(),
		newZhipuSignSettingRepoStub(nil),
		&zhipuSignAccountListerStub{accounts: []Account{
			zhipuSignConfigTestAccount(1, "zhipu-signed-a", zhipuSignConfigTestAPIKey, "v4"),
		}},
		runtime,
	)

	status, err := svc.Status(context.Background())
	require.NoError(t, err)
	payload, err := json.Marshal(status)
	require.NoError(t, err)

	for _, secret := range []string{
		zhipuSignConfigTestAPIKey,
		"s3cr3t-part",
		zhipuSignConfigTestAPIKeyID,
		"access-token-value",
		"zcodejwt-value",
		"privateCipher",
	} {
		require.NotContains(t, string(payload), secret, "状态响应不得包含凭据/签名材料")
	}
}

// 账号查询失败必须显式报错：「状态未知」与「没有启用账号」是两种不同的运营含义。
func TestZhipuSignConfigStatusSurfacesAccountQueryFailure(t *testing.T) {
	t.Parallel()

	svc := NewZhipuSignConfigService(
		zhipuSignTestGatewayConfig(),
		newZhipuSignSettingRepoStub(nil),
		&zhipuSignAccountListerStub{err: errors.New("db down")},
		&zhipuSignRuntimeStub{},
	)

	_, err := svc.Status(context.Background())

	require.Error(t, err)
	require.Contains(t, err.Error(), "db down")
}

// ApplyToSigner 只在生效值变化时推送：同一配置下重复调用不得反复 SetOptions（热路径
// 每请求都会调它），配置变化后必须推送一次带新值的选项。
func TestZhipuSignConfigApplyToSignerPushesOnlyOnChange(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime := &zhipuSignRuntimeStub{}
	repo := newZhipuSignSettingRepoStub(nil)
	svc := NewZhipuSignConfigService(zhipuSignTestGatewayConfig(), repo, nil, runtime)

	svc.ApplyToSigner(ctx)
	svc.ApplyToSigner(ctx)
	require.Len(t, runtime.appliedOptions(), 1, "生效值未变时不得重复 SetOptions")

	first := runtime.appliedOptions()[0]
	require.Equal(t, "0.16.9", first.ClientVersion)
	require.Equal(t, 8, first.PowBits)
	require.Equal(t, 1440*time.Minute, first.KeyTTL)
	require.Equal(t, 30*time.Second, first.HandshakeBackoff)

	_, err := svc.Update(ctx, ZhipuSignConfigUpdate{SignClientVersion: zhipuSignPtr("0.17.1")})
	require.NoError(t, err)

	applied := runtime.appliedOptions()
	require.Len(t, applied, 2)
	require.Equal(t, "0.17.1", applied[1].ClientVersion)
	require.Equal(t, 8, applied[1].PowBits)
}
