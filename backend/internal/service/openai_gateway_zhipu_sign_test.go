//go:build unit

package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/zcodesign"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/hkdf"
)

// 智谱签名注入（design M3 / 票 22）的表驱动用例：
//
//   - 双开关 AND 门控矩阵（全局 × 账号级标记 × 登录托管网账号判定）；
//   - 两个挂点（CC 直转 / anthropic 原生直通）与「签名最后应用」顺序；
//   - fail-open：签名失败仍发无签名请求、无半写头、降级回调恰好一次；
//   - sessionID 确定性种子；
//   - 与票 20 真实 Signer 的联调（httptest 握手端点 + httptest 上游，7 个签名头）。
//
// 除联调用例外一律用 stub Signer，不触发真实握手。

// zhipuSignTestAPIKey 是 <apiKeyId>.<secret> 形态的凭据：握手协议按第一个点拆分，
// 缺任一半即 fail-open，所以测试账号必须用该形态。
const zhipuSignTestAPIKey = "1234567890abcdef.test-secret"

// zhipuSignStubSigner 是签名接缝的可编程 stub：记录调用参数与「调用时刻」的头快照，
// 让顺序断言（ApplyHeaderOverrides 之后才签名）无需真实握手即可成立。
type zhipuSignStubSigner struct {
	calls         int
	apiKeys       []string
	sessions      []string
	headersAtSign []http.Header

	// err 非 nil 时模拟签名失败（fail-open 路径）：先写入 partial（模拟半写），再返回错误。
	err error
	// partial 是失败前写入的头，用于验证 fail-open 的兜底剥离。
	partial map[string]string
	// values 覆盖默认的签名头值，用于「签名值不得被后续覆写拆散」的断言。
	values map[string]string
}

// TestApplyZhipuClientSignFailOpenStripsPartialHeaders 覆盖 fail 路径：签名失败时
// 不得留下任何签名头（含半写头），且失败回调接缝恰好触发一次（票 24 的 L1 计数点）。
func TestApplyZhipuClientSignFailOpenStripsPartialHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"}],"stream":false}`)

	hookErr := errors.New("handshake: business code 3012")
	var hookedIDs []int64
	var hookedErrs []error
	stub := &zhipuSignStubSigner{
		err: hookErr,
		partial: map[string]string{
			zcodesign.HeaderClientTs: "half-written-ts",
			zcodesign.HeaderAppID:    "zcode",
		},
	}
	svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(true), zhipuSigner: stub}
	svc.zhipuSignFailureHook = func(accountID int64, err error) {
		hookedIDs = append(hookedIDs, accountID)
		hookedErrs = append(hookedErrs, err)
	}
	account := zhipuSignTestAccount(t, map[string]any{})
	header := http.Header{"Content-Type": []string{"application/json"}}

	svc.applyZhipuClientSign(context.Background(), zhipuSignTestContext(body), account, body, header)

	require.Equal(t, 1, stub.calls)
	require.Equal(t, []int64{account.ID}, hookedIDs, "失败回调必须恰好一次并带上账号 ID（票 24 的计数维度）")
	require.Equal(t, []error{hookErr}, hookedErrs)
	zhipuSignAssertUpstreamHeaders(t, header, false)
	require.Equal(t, []string{"application/json"}, header.Values("Content-Type"), "未签名的头不受影响")
}

// TestForwardAsRawChatCompletionsFailOpenKeepsRequestSent 覆盖挂点 1 的 fail-open：
// 签名失败仍然发无签名请求（可用性优先），且失败回调恰好一次。
func TestForwardAsRawChatCompletionsFailOpenKeepsRequestSent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"}],"stream":false}`)

	hooked := 0
	stub := &zhipuSignStubSigner{err: errors.New("handshake failed")}
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(true), httpUpstream: upstream, zhipuSigner: stub}
	svc.zhipuSignFailureHook = func(int64, error) { hooked++ }
	account := zhipuSignTestAccount(t, map[string]any{
		"header_override_enabled": true,
		"header_overrides":        map[string]any{"x-request-source": "override-present"},
	})

	_, err := svc.forwardAsRawChatCompletions(context.Background(), zhipuSignTestContext(body), account, body, "")
	require.Error(t, err)
	require.NotNil(t, upstream.lastReq, "签名失败必须 fail-open：请求照常发往上游")
	require.Equal(t, 1, stub.calls)
	require.Equal(t, 1, hooked, "fail-open 降级必须计入一次失败回调")

	zhipuSignAssertUpstreamHeaders(t, upstream.lastReq.Header, false)
	values, _ := zhipuTestHeaderValues(upstream.lastReq.Header, "x-request-source")
	require.Equal(t, []string{"override-present"}, values)
}

// TestZhipuSignSessionIDIsStablePerAccountAndConversation 覆盖 sessionID 规则：
// 复用既有确定性会话种子（同 opencode x-opencode-session 先例），同一 API Key +
// 同一账号 + 同一对话（首条 user 消息不变）跨轮稳定，跨 API Key / 账号 / 对话互不相同。
// 黄金值由文档化的种子独立算出（sha256 前 16 字节 + UUIDv4 版本/变体位）。
func TestZhipuSignSessionIDIsStablePerAccountAndConversation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	otherBody := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"another question"}],"stream":false}`)

	stub := &zhipuSignStubSigner{}
	svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(true), zhipuSigner: stub}
	account := zhipuSignTestAccount(t, map[string]any{})
	other := zhipuSignTestAccount(t, map[string]any{})
	other.ID = account.ID + 1

	ctxWithKey := func(apiKeyID int64, body []byte) *gin.Context {
		c := zhipuSignTestContext(body)
		c.Set("api_key", &APIKey{ID: apiKeyID})
		return c
	}
	sign := func(c *gin.Context, acc *Account, sendBody []byte) string {
		svc.applyZhipuClientSign(context.Background(), c, acc, sendBody, http.Header{})
		session := stub.sessions[len(stub.sessions)-1]
		require.NotEmpty(t, session)
		_, err := uuid.Parse(session)
		require.NoError(t, err, "sessionID 必须是合法 UUID")
		return session
	}

	session := sign(ctxWithKey(7, body), account, body)
	require.Equal(t, "17006cfb-66e6-4e8b-9c23-8533da2885a9", session,
		"sessionID = UUIDv4(sha256(\"sub2api:zhipu-sign:u<apiKeyID>:a<accountID>:<首条 user 文本>\"))")

	require.Equal(t, session, sign(ctxWithKey(7, body), account, body), "同账号同对话跨轮必须稳定")
	require.NotEqual(t, session, sign(ctxWithKey(7, otherBody), account, otherBody), "不同对话必须不同")
	otherAccount := other
	require.NotEqual(t, session, sign(ctxWithKey(7, body), otherAccount, body), "不同账号必须不同")
	require.NotEqual(t, session, sign(ctxWithKey(8, body), account, body), "不同 API Key 必须不同")
}

// zhipuSignSealPrivateKey 按协议把 PKCS#8 私钥封装成上游回包里的 privateCipher：
// base64(iv || AES-256-GCM(base64(pkcs8), aad = apiKeyId))，AES 密钥由
// HKDF-SHA256(secret, salt="WD_CLIENT_SIGN_KDF_SALT", info="ed25519_priv") 派生。
// 独立实现，不复用被测代码。
func zhipuSignSealPrivateKey(t *testing.T, secret, apiKeyID string, iv, pkcs8DER []byte) string {
	t.Helper()
	reader := hkdf.New(sha256.New, []byte(secret), []byte("WD_CLIENT_SIGN_KDF_SALT"), []byte("ed25519_priv"))
	key := make([]byte, 32)
	_, err := io.ReadFull(reader, key)
	require.NoError(t, err)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	sealed := gcm.Seal(nil, iv, []byte(base64.StdEncoding.EncodeToString(pkcs8DER)), []byte(apiKeyID))
	return base64.StdEncoding.EncodeToString(append(append([]byte{}, iv...), sealed...))
}

// zhipuSignReferenceHandshakeSignature 独立复算握手自证签名（RFC 5869 HKDF-SHA256 +
// RFC 2104 HMAC-SHA256，消息 "get_sign_key\n<id>\n<ts>\n<nonce>"）。
func zhipuSignReferenceHandshakeSignature(t *testing.T, secret, apiKeyID, ts, nonce string) string {
	t.Helper()
	reader := hkdf.New(sha256.New, []byte(secret), []byte("WD_CLIENT_SIGN_KDF_SALT"), []byte("getSignKey_hmac"))
	key := make([]byte, 32)
	_, err := io.ReadFull(reader, key)
	require.NoError(t, err)
	mac := hmac.New(sha256.New, key)
	_, err = mac.Write([]byte("get_sign_key\n" + apiKeyID + "\n" + ts + "\n" + nonce))
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// zhipuSignPowSatisfied 独立复算 PoW：challenge = hex(sha256("<id>\nzcode\n<session>\n<ts>"))[:32]，
// 要求 sha256("<challenge>\n<pow>") 的前 8 位为零。
func zhipuSignPowSatisfied(apiKeyID, sessionID, ts, pow string) bool {
	sum := sha256.Sum256([]byte(apiKeyID + "\n" + "zcode" + "\n" + sessionID + "\n" + ts))
	challenge := hex.EncodeToString(sum[:16])
	digest := sha256.Sum256([]byte(challenge + "\n" + pow))
	return digest[0] == 0
}

// TestZhipuSignIntegrationWithRealSigner 与票 20 的真实 Signer 联调：httptest 握手端点
// 返回私钥，httptest 上游（记录器）收到 7 个签名头；握手请求与签名头都用协议文档
// 独立复算校验，避免「实现自证」。
func TestZhipuSignIntegrationWithRealSigner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"}],"stream":false}`)

	apiKeyID, secret, found := strings.Cut(zhipuSignTestAPIKey, ".")
	require.True(t, found)

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	privateCipher := zhipuSignSealPrivateKey(t, secret, apiKeyID, []byte("0123456789ab"), pkcs8)

	var mu sync.Mutex
	var handshakeBodies []map[string]string
	handshakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		var request map[string]string
		_ = json.Unmarshal(payload, &request)
		request["authorization"] = r.Header.Get("Authorization")
		request["path"] = r.URL.Path
		mu.Lock()
		handshakeBodies = append(handshakeBodies, request)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200,"data":{"privateCipher":"` + privateCipher + `"}}`))
	}))
	defer handshakeServer.Close()

	signer := zcodesign.NewSigner(handshakeServer.URL, "0.16.9", time.Hour, http.DefaultClient)
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(true), httpUpstream: upstream, zhipuSigner: signer}
	account := zhipuSignTestAccount(t, map[string]any{})

	_, err = svc.forwardAsRawChatCompletions(context.Background(), zhipuSignTestContext(body), account, body, "")
	require.Error(t, err)
	_, err = svc.forwardAsRawChatCompletions(context.Background(), zhipuSignTestContext(body), account, body, "")
	require.Error(t, err)

	require.Len(t, upstream.requests, 2)
	first := upstream.requests[0].Header
	second := upstream.requests[1].Header

	// 7 个头齐全，且值符合协议形态。
	for _, name := range zhipuSignHeaderNames() {
		values, variants := zhipuTestHeaderValues(first, name)
		require.Equal(t, 1, variants, "签名头 %s 在 wire 上只能有一个来源", name)
		require.NotEmpty(t, values)
	}
	require.Equal(t, "zcode", first.Get(zcodesign.HeaderAppID))
	require.Equal(t, "0.16.9", first.Get(zcodesign.HeaderClientVersion))
	require.Equal(t, "a597383c-a310-4b22-984a-ca097c2780fe", first.Get(zcodesign.HeaderSessionID),
		"同一账号同一对话的会话种子必须稳定")
	ts, err := strconv.ParseInt(first.Get(zcodesign.HeaderClientTs), 10, 64)
	require.NoError(t, err)
	require.Greater(t, ts, int64(1_700_000_000_000), "ts 必须是毫秒时间戳")

	nonce := first.Get(zcodesign.HeaderClientNonce)
	require.Len(t, nonce, 32)
	_, err = hex.DecodeString(nonce)
	require.NoError(t, err)
	require.NotEqual(t, nonce, second.Get(zcodesign.HeaderClientNonce), "nonce 必须逐请求新鲜")

	pow := first.Get(zcodesign.HeaderClientPow)
	require.Len(t, pow, 40, "X-Client-Pow = nonce + 8 位十六进制计数器")
	require.True(t, strings.HasPrefix(pow, nonce))
	require.True(t, zhipuSignPowSatisfied(apiKeyID, first.Get(zcodesign.HeaderSessionID), first.Get(zcodesign.HeaderClientTs), pow),
		"PoW 必须满足 8 位前导零")

	signature, err := base64.StdEncoding.DecodeString(first.Get(zcodesign.HeaderClientSig))
	require.NoError(t, err)
	require.True(t, ed25519.Verify(publicKey, []byte(strings.Join([]string{
		apiKeyID,
		first.Get(zcodesign.HeaderClientTs),
		first.Get(zcodesign.HeaderClientVersion),
		first.Get(zcodesign.HeaderSessionID),
		nonce,
	}, "\n")), signature),
		"签名必须是用握手私钥对 <id>\\n<ts>\\n<version>\\n<session>\\n<nonce> 的 Ed25519 签名")

	// 握手：只发生一次（私钥按 TTL 缓存），且请求自证签名可独立复算。
	mu.Lock()
	bodies := append([]map[string]string(nil), handshakeBodies...)
	mu.Unlock()
	require.Len(t, bodies, 1, "私钥必须缓存：两次请求只握手一次")
	require.Equal(t, zhipuSignTestAPIKey, bodies[0]["authorization"])
	require.Equal(t, "/api/paas/c1f3a7e2/v2/client", bodies[0]["path"])
	require.Equal(t, zhipuSignTestAPIKey, bodies[0]["apiKey"])
	require.Equal(t, zhipuSignReferenceHandshakeSignature(t, secret, apiKeyID, bodies[0]["ts"], bodies[0]["nonce"]), bodies[0]["sig"])
}

// TestZhipuSignMountsAreExactlyTwo 是结构性回归守卫（design M3 登记的风险：挂点遗漏）：
// zhipu 数据面只允许 sendCCUpstreamRequest（CC 直转）与 buildNativeAnthropicUpstreamRequest
// （anthropic 原生直通）两处调用签名注入。count_tokens（Anthropic 网关 ForwardCountTokens）
// 与模型列表同步（AccountTestService）不在本服务的出站构建器内，故对它们天然零签名。
// 新增第三个调用点必须同步票 31 的验证清单与灰度策略。
func TestZhipuSignMountsAreExactlyTwo(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fset := token.NewFileSet()
	callSites := map[string]int{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "applyZhipuClientSign" {
				return true
			}
			callSites[name]++
			return true
		})
	}

	require.Equal(t, map[string]int{
		"openai_gateway_cc_pipeline.go":               1,
		"openai_gateway_messages_anthropic_native.go": 1,
	}, callSites)
}

// zhipuSignAssertUpstreamHeaders 断言上游请求头：启用时 7 个签名头各只有一个变体，
// 且无签名头被拆散；未启用时一个签名头都不能出现。
func zhipuSignAssertUpstreamHeaders(t *testing.T, sent http.Header, enabled bool) {
	t.Helper()
	for _, name := range zhipuSignHeaderNames() {
		values, variants := zhipuTestHeaderValues(sent, name)
		if !enabled {
			require.Empty(t, values, "未启用签名的账号不得出现签名头 %s", name)
			require.Zero(t, variants, "未启用签名的账号不得出现签名头 %s", name)
			continue
		}
		require.Equal(t, 1, variants, "签名头 %s 在 wire 上只能有一个来源", name)
		require.NotEmpty(t, values, "签名头 %s 必须出现在上游请求", name)
	}
}

// TestForwardAsAnthropicInjectsZhipuSign 覆盖挂点 2（anthropic 原生直通，与票 23
// 的重放路径共用的构建器）与同一套「签名最后应用」约定。
func TestForwardAsAnthropicInjectsZhipuSign(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","max_tokens":32,"stream":false,"messages":[{"role":"user","content":"hello"}]}`)

	cases := []struct {
		name      string
		enabled   bool
		overrides map[string]any
		wantCalls int
	}{
		{
			name:    "全局开 + 账号标记 v4 → 签名",
			enabled: true,
			overrides: map[string]any{
				// 同名覆写：签名最后应用，必须只有一个来源。
				"x-client-sig":     "override-must-lose",
				"x-request-source": "override-present",
			},
			wantCalls: 1,
		},
		{
			name:       "全局关 → 逐字节等价于现状",
			enabled:    false,
			overrides:  map[string]any{"x-request-source": "override-present"},
			wantCalls:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &zhipuSignStubSigner{values: map[string]string{zcodesign.HeaderClientSig: "signed-sig"}}
			upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
			svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(tc.enabled), httpUpstream: upstream, zhipuSigner: stub}
			account := zhipuSignTestAccount(t, map[string]any{
				"api_protocol":            APIProtocolAnthropic,
				"header_override_enabled": true,
				"header_overrides":        tc.overrides,
			})

			_, err := svc.ForwardAsAnthropic(context.Background(),
				adaptiveProtocolTestContext("/v1/messages", body), account, body, "", "")
			require.Error(t, err)
			require.Equal(t, "http://upstream.example/v1/messages", upstream.lastReq.URL.String())
			require.Equal(t, tc.wantCalls, stub.calls)

			zhipuSignAssertUpstreamHeaders(t, upstream.lastReq.Header, tc.enabled)
			values, _ := zhipuTestHeaderValues(upstream.lastReq.Header, "x-request-source")
			require.Equal(t, []string{"override-present"}, values)

			if tc.wantCalls == 1 {
				atSign := stub.headersAtSign[0]
				values, _ := zhipuTestHeaderValues(atSign, "x-request-source")
				require.Equal(t, []string{"override-present"}, values,
					"签名必须在账号级 header 覆写之后调用")
				values, variants := zhipuTestHeaderValues(upstream.lastReq.Header, zcodesign.HeaderClientSig)
				require.Equal(t, []string{"signed-sig"}, values, "签名头不得被覆写拆散")
				require.Equal(t, 1, variants)
			}
		})
	}
}


// zhipuTestHeaderValues 收集 header 中与 name 大小写不敏感匹配的全部值并返回变体数。
// 账号级覆写会以非 canonical key（如 "x-session-id"）写入，http.Header.Values 取不到；
// 变体数用于断言「签名头在 wire 上只有一个来源」。
func zhipuTestHeaderValues(h http.Header, name string) (values []string, variants int) {
	for existing, current := range h {
		if strings.EqualFold(existing, name) {
			values = append(values, current...)
			variants++
		}
	}
	return values, variants
}

// TestForwardAsRawChatCompletionsInjectsZhipuSignLast 覆盖挂点 1（CC 直转）与
// 「签名最后应用」：签名发生在账号级 header 覆写之后，且最终上线的头以签名值为准
// （同名覆写不得拆散 ts/nonce/sig/session 的同源一致性）。
func TestForwardAsRawChatCompletionsInjectsZhipuSignLast(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"}],"stream":false}`)

	stub := &zhipuSignStubSigner{values: map[string]string{
		zcodesign.HeaderClientVersion: "signed-0.16.9",
		zcodesign.HeaderSessionID:     "signed-session",
	}}
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(true), httpUpstream: upstream, zhipuSigner: stub}
	account := zhipuSignTestAccount(t, map[string]any{
		"header_override_enabled": true,
		"header_overrides": map[string]any{
			// 与签名头同名的覆写：只有「签名最后应用」才不会被它改写。
			"x-client-version": "override-must-lose",
			"x-session-id":      "override-must-lose",
			"x-request-source":  "override-present",
		},
	})

	_, err := svc.forwardAsRawChatCompletions(context.Background(), zhipuSignTestContext(body), account, body, "")
	require.Error(t, err) // 记录器主动中断，只为截获发往上游的请求
	require.Equal(t, "http://upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Equal(t, 1, stub.calls)

	atSign := stub.headersAtSign[0]
	values, variants := zhipuTestHeaderValues(atSign, "x-request-source")
	require.Equal(t, []string{"override-present"}, values, "签名必须在 ApplyHeaderOverrides 之后调用")
	require.Equal(t, 1, variants)

	sent := upstream.lastReq.Header
	for _, name := range zhipuSignHeaderNames() {
		values, variants := zhipuTestHeaderValues(sent, name)
		require.Equal(t, 1, variants, "签名头 %s 在 wire 上只能有一个来源", name)
		require.NotEmpty(t, values, "签名头 %s 必须出现在上游请求", name)
	}
	values, _ = zhipuTestHeaderValues(sent, zcodesign.HeaderClientVersion)
	require.Equal(t, []string{"signed-0.16.9"}, values, "签名头不得被覆写拆散")
	values, _ = zhipuTestHeaderValues(sent, zcodesign.HeaderSessionID)
	require.Equal(t, []string{"signed-session"}, values, "签名头不得被覆写拆散")
	values, _ = zhipuTestHeaderValues(sent, "x-request-source")
	require.Equal(t, []string{"override-present"}, values, "无关覆写照旧生效")
}

// TestForwardAsRawChatCompletionsKeepsUnsignedPathByteIdentical 覆盖非启用账号：
// 同一路径上签名头一个都不能出现，且账号级覆写仍照常生效。
func TestForwardAsRawChatCompletionsKeepsUnsignedPathByteIdentical(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"}],"stream":false}`)

	stub := &zhipuSignStubSigner{}
	upstream := &httpUpstreamRecorder{err: errors.New("stop after capture")}
	svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(false), httpUpstream: upstream, zhipuSigner: stub}
	account := zhipuSignTestAccount(t, map[string]any{
		"header_override_enabled": true,
		"header_overrides":        map[string]any{"x-request-source": "override-present"},
	})

	_, err := svc.forwardAsRawChatCompletions(context.Background(), zhipuSignTestContext(body), account, body, "")
	require.Error(t, err)
	require.Equal(t, 0, stub.calls)

	sent := upstream.lastReq.Header
	for _, name := range zhipuSignHeaderNames() {
		values, variants := zhipuTestHeaderValues(sent, name)
		require.Empty(t, values, "未启用签名的账号不得出现签名头 %s", name)
		require.Zero(t, variants, "未启用签名的账号不得出现签名头 %s", name)
	}
	values, _ := zhipuTestHeaderValues(sent, "x-request-source")
	require.Equal(t, []string{"override-present"}, values)
}


func (s *zhipuSignStubSigner) Sign(_ context.Context, apiKey, sessionID string, h http.Header) error {
	s.calls++
	s.apiKeys = append(s.apiKeys, apiKey)
	s.sessions = append(s.sessions, sessionID)
	snapshot := http.Header{}
	for name, values := range h {
		snapshot[name] = append([]string(nil), values...)
	}
	s.headersAtSign = append(s.headersAtSign, snapshot)
	if s.err != nil {
		for name, value := range s.partial {
			h.Set(name, value)
		}
		return s.err
	}
	for _, name := range zhipuSignHeaderNames() {
		value := "stub-" + name
		if override, ok := s.values[name]; ok {
			value = override
		}
		h.Set(name, value)
	}
	return nil
}

// zhipuSignTestAccount 构造登录托管的智谱账号：platform=zhipu、type=apikey、
// credentials[auth_flow]=bigmodel_oauth（票 03 的托管标记）+ 票 22 的账号级签名标记。
func zhipuSignTestAccount(t *testing.T, credentials map[string]any) *Account {
	t.Helper()
	base := map[string]any{
		"api_key":           zhipuSignTestAPIKey,
		"auth_flow":         "bigmodel_oauth",
		"zcode_client_sign": "v4",
		"api_protocol":      APIProtocolChatCompletions,
		"account_mode":      AccountModePayG,
		"base_url":          "http://upstream.example",
	}
	for key, value := range credentials {
		base[key] = value
	}
	return &Account{
		ID:          2201,
		Name:        "zhipu-login-managed",
		Platform:    PlatformZhipu,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: base,
	}
}

// zhipuSignTestConfig 返回带全局签名开关的网关配置（默认关，测试显式打开）。
func zhipuSignTestConfig(signV4Enabled bool) *config.Config {
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.Zhipu.SignV4Enabled = signV4Enabled
	return cfg
}

func zhipuSignTestContext(body []byte) *gin.Context {
	return adaptiveProtocolTestContext("/v1/chat/completions", body)
}

// TestApplyZhipuClientSignGatingMatrix 覆盖验收标准的生效条件：账号是智谱登录托管
// 且 credentials["zcode_client_sign"]=="v4" 且 cfg.Gateway.Zhipu.SignV4Enabled，
// 三者同时成立才注入签名（AND）；任一不成立时请求头必须保持调用前状态。
func TestApplyZhipuClientSignGatingMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"}],"stream":false}`)

	cases := []struct {
		name          string
		globalEnabled bool
		platform      string
		accountType   string
		marker        string
		authFlow      string
		nilSigner     bool
		wantCalls     int
	}{
		{
			name:          "全局开 + 账号标记 v4 + 登录托管 → 签名",
			globalEnabled: true,
			platform:      PlatformZhipu,
			accountType:   AccountTypeAPIKey,
			marker:        "v4",
			authFlow:      "bigmodel_oauth",
			wantCalls:     1,
		},
		{
			name:          "全局关（默认）→ 不签名",
			globalEnabled: false,
			platform:      PlatformZhipu,
			accountType:   AccountTypeAPIKey,
			marker:        "v4",
			authFlow:      "bigmodel_oauth",
			wantCalls:     0,
		},
		{
			name:          "账号级标记关 → 不签名（全局开也不签）",
			globalEnabled: true,
			platform:      PlatformZhipu,
			accountType:   AccountTypeAPIKey,
			marker:        "",
			authFlow:      "bigmodel_oauth",
			wantCalls:     0,
		},
		{
			name:          "账号级标记非 v4 → 不签名",
			globalEnabled: true,
			platform:      PlatformZhipu,
			accountType:   AccountTypeAPIKey,
			marker:        "v3",
			authFlow:      "bigmodel_oauth",
			wantCalls:     0,
		},
		{
			name:          "智谱手填 api key（非登录托管）→ 不签名",
			globalEnabled: true,
			platform:      PlatformZhipu,
			accountType:   AccountTypeAPIKey,
			marker:        "v4",
			authFlow:      "",
			wantCalls:     0,
		},
		{
			name:          "非 zhipu 平台 → 不签名",
			globalEnabled: true,
			platform:      PlatformKimi,
			accountType:   AccountTypeAPIKey,
			marker:        "v4",
			authFlow:      "bigmodel_oauth",
			wantCalls:     0,
		},
		{
			name:          "未注入 Signer（wire 未接）→ 不签名且不 panic",
			globalEnabled: true,
			platform:      PlatformZhipu,
			accountType:   AccountTypeAPIKey,
			marker:        "v4",
			authFlow:      "bigmodel_oauth",
			nilSigner:     true,
			wantCalls:     0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &zhipuSignStubSigner{}
			svc := &OpenAIGatewayService{cfg: zhipuSignTestConfig(tc.globalEnabled)}
			if !tc.nilSigner {
				svc.zhipuSigner = stub
			}
			account := zhipuSignTestAccount(t, map[string]any{})
			account.Platform = tc.platform
			account.Type = tc.accountType
			if tc.marker == "" {
				delete(account.Credentials, "zcode_client_sign")
			} else {
				account.Credentials["zcode_client_sign"] = tc.marker
			}
			if tc.authFlow == "" {
				delete(account.Credentials, "auth_flow")
			} else {
				account.Credentials["auth_flow"] = tc.authFlow
			}

			header := http.Header{"Content-Type": []string{"application/json"}}
			before := header.Clone()

			svc.applyZhipuClientSign(context.Background(), zhipuSignTestContext(body), account, body, header)

			require.Equal(t, tc.wantCalls, stub.calls)
			if tc.wantCalls == 1 {
				require.Equal(t, zhipuSignTestAPIKey, stub.apiKeys[0])
				for _, name := range zhipuSignHeaderNames() {
					require.Equal(t, []string{"stub-" + name}, header.Values(name), "签名头 %s 必须写入", name)
				}
				return
			}
			// 未生效时请求头逐字节等价于调用前（非 zhipu / 未启用 = 零行为变化）。
			require.Equal(t, before, header)
		})
	}
}
