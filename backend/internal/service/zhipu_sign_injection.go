package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/zcodesign"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// 智谱客户端签名 V4 注入（design M3 / 票 22）。
//
// 本文件是「签名最后应用」约定的唯一实现点：签名必须在账号级请求头覆写
// （Account.ApplyHeaderOverrides）之后写入，否则 ts/nonce/sig 会被同源覆写拆散。
//
// **zhipu 数据面挂点仅两处**（票 22 冻结的调用位置约定；新增第三条路径必须同步
// 票 31 的验证清单）：
//
//  1. sendCCUpstreamRequest（openai_gateway_cc_pipeline.go）— CC 直转；
//  2. buildNativeAnthropicUpstreamRequest（openai_gateway_messages_anthropic_native.go）
//     — anthropic 原生直通。
//
// zhipu 的 Responses 协议本就被拒（account.go）、adaptive 强制 CC（recon §3/§4），
// 故这两处覆盖 zhipu 全部数据面路径；count_tokens（Anthropic 网关 ForwardCountTokens）
// 与模型列表同步（AccountTestService）不经这两处，天然零签名。

// 账号级签名开关。键名与值是与 #26 调度成本因子共用的冻结契约
// （openai_account_scheduler.go:zhipuSchedulingSignCredentialKey，同键同值）。
const (
	zhipuSignCredentialKey = "zcode_client_sign"
	zhipuSignCredentialV4  = "v4"
)

// zhipuSignOrigin 是握手 origin：智谱数据面域名（design M3：取账号 base_url 的
// origin，默认账号均为 https://open.bigmodel.cn/...）。海外站（z.ai）不在本期范围。
const zhipuSignOrigin = "https://open.bigmodel.cn"

// zhipuClientSigner 是签名注入的窄接缝：生产实现是票 20 的 *zcodesign.Signer，
// 测试注入 stub 断言门控与调用次数而不触发真实握手。Sign 必须保证「全或无」：
// 任何失败都不写入部分签名头（由票 20 保证，本文件只做防御性剥离）。
//
// Invalidate 是自愈状态机（票 23）的第一步：作废 apiKeyID 的缓存私钥，使下一次
// Sign 必然重新握手（singleflight 合并 + 每 key 退避由票 20 的 Signer 负责）。
type zhipuClientSigner interface {
	Sign(ctx context.Context, apiKey string, sessionID string, h http.Header) error
	Invalidate(apiKeyID string)
}

// zhipuSignHTTPDoer 把网关共享的 HTTP 上游适配成票 20 的握手传输接缝。握手请求
// 不带账号代理（origin 固定为数据面默认域），代理只作用于数据面请求本身。
type zhipuSignHTTPDoer struct {
	upstream HTTPUpstream
}

func (d zhipuSignHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	if d.upstream == nil {
		return nil, errors.New("zhipu sign: no upstream transport configured")
	}
	return d.upstream.Do(req, "", 0, 0)
}

// zhipuSignHeaderNames 返回协议固定的 7 个签名头（票 20 的导出常量是单一事实源）。
func zhipuSignHeaderNames() []string {
	return []string{
		zcodesign.HeaderClientTs,
		zcodesign.HeaderClientVersion,
		zcodesign.HeaderClientSig,
		zcodesign.HeaderSessionID,
		zcodesign.HeaderClientNonce,
		zcodesign.HeaderAppID,
		zcodesign.HeaderClientPow,
	}
}

// zhipuClientSignEnabled 是双开关 AND 门控：
//
//	账号是智谱登录托管（type=apikey + credentials[auth_flow]=bigmodel_oauth）
//	∧ credentials[zcode_client_sign]=="v4"（账号级灰度）
//	∧ cfg.Gateway.Zhipu.SignV4Enabled（全局一键关停）
//
// 运行时熔断（票 24 的账号级摘除）与配置热更新（票 28）在各自的票内收口，
// 本函数只读当前生效值。
func (s *OpenAIGatewayService) zhipuClientSignEnabled(account *Account) bool {
	if s == nil || s.zhipuSigner == nil || account == nil {
		return false
	}
	if s.cfg == nil || !s.cfg.Gateway.Zhipu.SignV4Enabled {
		return false
	}
	if !zhipuSignLoginManagedAccount(account) {
		return false
	}
	return account.GetCredential(zhipuSignCredentialKey) == zhipuSignCredentialV4
}

// zhipuSignLoginManagedAccount 判定账号是否为「登录托管」的智谱账号：
// platform=zhipu ∧ type=apikey ∧ credentials[auth_flow]=bigmodel_oauth。
//
// TODO(#03): 票 03 的 Account.IsZhipuLoginManaged() 合入 HEAD 后，本函数改为
// 一行委托该 getter 并删除（判定语义逐字一致）。此处按 #26 的内联桩先例实现，
// 避免本票卡在并行票的编译依赖上，也不与 account.go 产生冲突。
func zhipuSignLoginManagedAccount(account *Account) bool {
	return account != nil &&
		account.Platform == PlatformZhipu &&
		account.Type == AccountTypeAPIKey &&
		account.GetCredential("auth_flow") == "bigmodel_oauth"
}

// zhipuSignSessionID 生成签名用的 X-Session-Id：复用既有确定性会话种子模式
// （CC 路径 x-opencode-session 先例），同一账号 + 同一对话（首条 user 消息不变）
// 跨轮稳定，跨 API Key / 账号 / 对话互不相同。
func zhipuSignSessionID(c *gin.Context, account *Account, body []byte) string {
	return generateSessionUUID(fmt.Sprintf(
		"sub2api:zhipu-sign:u%d:a%d:%s",
		getAPIKeyIDFromContext(c), account.ID, extractFirstUserText(body),
	))
}

// applyZhipuClientSign 在出站请求头定稿后注入签名头。关闭、账号不适用或签名失败
// 时不做任何阻塞：失败一律 fail-open（剥离签名头，发无签名请求并回调计数接缝）。
//
// 返回值 signed 表示「本次请求确实带着签名发出」：自愈状态机（票 23）只对
// signed=true 的响应做 VERIFY_* 判定 —— 没签名就没有私钥可作废，也没有可重放的
// 签名请求（fail-open 的降级请求不进入自愈路径）。
func (s *OpenAIGatewayService) applyZhipuClientSign(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	header http.Header,
) bool {
	if header == nil || !s.zhipuClientSignEnabled(account) {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	// 先清掉同名旧值：账号级覆写可能以非 canonical key（如 "x-client-version"）
	// 写入，直接 Set 会与它并存成两个同名头，把 ts/nonce/sig 拆散。
	deleteZhipuClientSignHeaderVariants(header)
	if err := s.zhipuSigner.Sign(ctx, apiKey, zhipuSignSessionID(c, account, body), header); err != nil {
		// fail-open（design M3.1(c) 的 "open" 分支）：票 20 的 Sign 已保证全或无，
		// 这里再剥离一次纯属防御，确保任何失败路径都不会把半写头发出去。
		deleteZhipuClientSignHeaderVariants(header)
		s.reportZhipuSignFailure(account, err)
		return false
	}
	return true
}

// zhipuSignAPIKeyID 取出凭据里的 apiKeyID（握手协议：第一个点之前的部分，
// 与票 20 splitAPIKey 同规则）。形态非法时返回空串，调用方不得据此调用 Invalidate。
func zhipuSignAPIKeyID(apiKey string) string {
	apiKeyID, secret, found := strings.Cut(strings.TrimSpace(apiKey), ".")
	if !found || apiKeyID == "" || secret == "" {
		return ""
	}
	return apiKeyID
}

// zhipuSignSelfHealOutcome 是自愈状态机（design M3.1(a)）的编排结果。
type zhipuSignSelfHealOutcome struct {
	// Resp 是最终响应：Replayed=false 时等于首次响应，Replayed=true 时为重放响应。
	Resp *http.Response
	// ReplayErr 是重放本身的传输层失败（只有 Replayed=true 时可能非 nil）。
	ReplayErr error
	// Replayed 表示命中 VERIFY_* 并已执行「作废私钥 → 重签 → 重放恰好一次」。
	Replayed bool
}

// zhipuSignSelfHealVerifyFailure 是自愈状态机的唯一编排点（两条数据面路径共用）。
//
// 语义（官方 CLI makeReplayableSigningRequest / readRefreshableClientSignatureReason
// 1:1 照搬）：首次响应错误体命中 VERIFY_SIGNATURE_INVALID / VERIFY_APIKEY_EXPIRED 时，
// 作废该 apiKeyID 的缓存私钥（下一次 Sign 重新握手；singleflight 与每 key 退避由
// 票 20 的 Signer 保证），然后用同一请求体重放一次（resend 内部会重新签名）。
//
// 单次重放的保证是结构性的：resend 由调用方传入「只发送一次」的闭包，本函数绝不
// 递归调用自己，也不循环 —— 重放再次失败时直接把重放响应交回既有错误链。
//
// 调用方约定：firstResp 的 body 已由 readOpenAIUpstreamError 读取并回卷（本函数
// 不再读它也绝不消费它）；resend 返回的重放响应 body 未读。
//
// TODO(票 24): 重放仍失败（ReplayErr != nil 或 Resp 仍 >=400）的分支是 fail 策略
// （design M3.1(c)：open = 剥签重发 / closed = 可 failover 错误）与 failopen 计数的
// 唯一接入点；本票的默认行为 = 交回既有错误链（等价 closed 语义，绝不静默放行）。
func (s *OpenAIGatewayService) zhipuSignSelfHealVerifyFailure(
	account *Account,
	signed bool,
	firstResp *http.Response,
	firstErrorBody []byte,
	resend func() (*http.Response, error),
) zhipuSignSelfHealOutcome {
	outcome := zhipuSignSelfHealOutcome{Resp: firstResp}
	if !signed || firstResp == nil || resend == nil || s.zhipuSigner == nil || account == nil {
		return outcome
	}
	reason := zcodesign.ReadZhipuSignatureVerifyReason(firstErrorBody)
	if reason == "" {
		return outcome
	}
	outcome.Replayed = true

	apiKeyID := zhipuSignAPIKeyID(account.GetOpenAIProtocolAPIKey())
	if apiKeyID != "" {
		s.zhipuSigner.Invalidate(apiKeyID)
	}
	logger.L().Warn("zhipu client sign: VERIFY_* rejected upstream, invalidating key and replaying once",
		zap.Int64("account_id", account.ID),
		zap.String("reason", reason),
		zap.String("api_key_id", apiKeyID),
	)

	replayResp, replayErr := resend()
	if replayErr != nil {
		outcome.ReplayErr = replayErr
		return outcome
	}
	outcome.Resp = replayResp
	if replayResp != nil && replayResp.StatusCode >= 400 {
		// 读出重放错误体并回卷：调用方仍会照常重读（接缝只读不消费），
		// 供票 24 的判定与既有错误链使用。
		_, _ = s.readOpenAIUpstreamError(replayResp)
	}
	return outcome
}

// deleteZhipuClientSignHeaderVariants 删除 7 个签名头的全部变体（大小写不敏感）。
// 启用路径在写入前调用（签名必须是同名的唯一来源），fail-open 路径在失败后调用
// （不得残留半写头）。
func deleteZhipuClientSignHeaderVariants(header http.Header) {
	if header == nil {
		return
	}
	for _, name := range zhipuSignHeaderNames() {
		for existing := range header {
			if strings.EqualFold(existing, name) {
				delete(header, existing)
			}
		}
	}
}

// reportZhipuSignFailure 记录并上报一次签名失败降级。热路径只做一次 Warn 日志与
// 一次回调，不做任何 I/O。
func (s *OpenAIGatewayService) reportZhipuSignFailure(account *Account, err error) {
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	logger.L().Warn("zhipu client sign fail-open: sending unsigned request",
		zap.Int64("account_id", accountID),
		zap.Error(err),
	)
	if s.zhipuSignFailureHook != nil {
		s.zhipuSignFailureHook(accountID, err)
	}
}
