package service

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// 智谱登录态账号的低频凭据健康探针（design M2 / 决策 B2）。
//
// 语义边界（刻意区别于 token_refresh_service.go:986-1048 的 SetError 路径）：
//   - 数据面健康与登录态无关：凭据失效只写 extra["zhipu_needs_relogin"] 并通知管理员，
//     绝不 SetError / SetTempUnschedulable / 改 schedulable（账号继续可调度，转发零影响）；
//   - 不自动重登（无凭据可用，B2 明确不做），人工凭 M1 重登后由 ZhipuOAuthService 清标记；
//   - 不进入 TokenRefreshService 候选集（B1）：本 keeper 只按 credentials.auth_flow 取号。
//
// 探针是票 08 的 ZhipuAccountMonitorService（共享 singleflight/TTL 缓存），
// 本 keeper 不自己打上游；探针未接线时不标记（宁可不判，不可误判）。
//
// 未接线项：wire 构造与 Start 由主会话统一添加（票 04 在制），本文件只提供
// 构造器、生命周期与标记语义。
const (
	// zhipuCredentialDefaultCheckIntervalMinutes 是探针周期兜底值，与票 01 的
	// gateway.zhipu.credential_check_interval_minutes 默认值同源（60）。
	zhipuCredentialDefaultCheckIntervalMinutes = 60

	// zhipuCredentialMaxJitter 是抖动上限口径（间隔的 1/5，封顶 5 分钟），
	// 与 upstream_billing_probe.go:1083-1088 同源；探针低频化以规避上游风控。
	zhipuCredentialMaxJitter = 5 * time.Minute

	// zhipuCredentialUsageStaleAfter 判定 CNProviderQuotaService 的额度快照「长期停更」。
	// 该快照由 api_key 探针每 10 分钟刷新；鉴权失败时上游不落快照，故长期停更
	// 即 api_key 探针持续失败的证据之一。
	zhipuCredentialUsageStaleAfter = 6 * time.Hour
)

// zhipuCredentialUnauthorizedPattern 匹配持久化错误文本里的 401 状态码
// （独立数字，避免 "1401"/"4021 ms" 之类的误命中）。
var zhipuCredentialUnauthorizedPattern = regexp.MustCompile(`(^|[^0-9])401([^0-9]|$)`)

// zhipuCredentialProber 是票 08 ZhipuAccountMonitorService 的探针接缝：
// keeper 与渠道监控共用同一 singleflight/TTL 入口，永不重复打上游。
// 实现方以 errors.Is(err, ErrZhipuCreditUsageUnauthorized) 表达 401/403 判活结论。
type zhipuCredentialProber interface {
	FetchUsageDetailForAccount(ctx context.Context, account *Account, start, end time.Time) ([]domain.MonitorQuotaModelCredit, error)
}

// zhipuCredentialNotifier 是管理员通知接缝，默认实现见 ZhipuCredentialAlertNotifier。
type zhipuCredentialNotifier interface {
	NotifyZhipuCredentialInvalid(ctx context.Context, account *Account, detail string)
}

// zhipuCredentialAccountRepo 是 keeper 依赖的仓储接缝：既有 AccountRepository
// 加上本票新增的按凭据流取号。新增方法只声明在这条窄接缝上而不并入 AccountRepository：
// 后者有大量测试 fake 逐方法实现，为一个只服务 keeper 的查询扩散改动没有收益；
// 具体实现仍在 repository 层（*accountRepository.ListAccountsByCredentialFlow）。
type zhipuCredentialAccountRepo interface {
	AccountRepository
	ListAccountsByCredentialFlow(ctx context.Context, flow string) ([]Account, error)
}

// ZhipuCredentialKeeper 周期探测登录托管智谱账号的凭据健康并维护 needs_relogin 标记。
type ZhipuCredentialKeeper struct {
	accountRepo zhipuCredentialAccountRepo
	probe       zhipuCredentialProber
	notifier    zhipuCredentialNotifier

	interval time.Duration

	// usageStaleAfter 是兜底分支的停更阈值；零值取 zhipuCredentialUsageStaleAfter。
	usageStaleAfter time.Duration
	// now / randInt64n 为测试注入口（时钟、抖动随机源）。
	now        func() time.Time
	randInt64n func(n int64) int64

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewZhipuCredentialKeeper 构造 keeper。interval 取自
// gateway.zhipu.credential_check_interval_minutes，配置缺失时回落 60 分钟。
// interval <= 0 时 Start() 直接返回（不启动），便于回滚到无 keeper 状态。
//
// accountRepo 以满足 zhipuCredentialAccountRepo（即 *repository.accountRepository）
// 的实现为准；不满足时 keeper 退化为空转并告警（对齐 grok 刷新条件仓储的 fail-closed 先例）。
func NewZhipuCredentialKeeper(
	accountRepo AccountRepository,
	probe zhipuCredentialProber,
	notifier zhipuCredentialNotifier,
	cfg *config.Config,
) *ZhipuCredentialKeeper {
	interval := time.Duration(zhipuCredentialDefaultCheckIntervalMinutes) * time.Minute
	if cfg != nil && cfg.Gateway.Zhipu.CredentialCheckIntervalMinutes > 0 {
		interval = time.Duration(cfg.Gateway.Zhipu.CredentialCheckIntervalMinutes) * time.Minute
	}
	k := &ZhipuCredentialKeeper{
		probe:           probe,
		notifier:        notifier,
		interval:        interval,
		usageStaleAfter: zhipuCredentialUsageStaleAfter,
		now:             time.Now,
		randInt64n:      rand.Int64N,
		stopCh:          make(chan struct{}),
	}
	if repo, ok := accountRepo.(zhipuCredentialAccountRepo); ok {
		k.accountRepo = repo
	} else if accountRepo != nil {
		slog.Warn("zhipu_credential_keeper.repository_missing_credential_flow_listing")
	}
	return k
}

// Start 启动后台周期探测（生命周期骨架对齐 cn_provider_balance_check_service.go）：
// 启动后先等一个周期再首次探测，避免与进程启动峰重叠；每轮等待时长 = 间隔 ± 抖动，
// 故用 timer 而非 ticker 重新随机化。
func (k *ZhipuCredentialKeeper) Start() {
	if k == nil || k.accountRepo == nil || k.probe == nil {
		return
	}
	if k.interval <= 0 {
		return
	}
	log.Printf("[ZhipuKeeper] started (interval=%s stale_after=%s)", k.interval, k.usageStaleAfterDuration())
	k.wg.Add(1)
	go k.loop()
}

// Stop 停止后台探测；可重复调用，Start 之前调用也安全。
func (k *ZhipuCredentialKeeper) Stop() {
	if k == nil {
		return
	}
	k.stopOnce.Do(func() { close(k.stopCh) })
	k.wg.Wait()
}

func (k *ZhipuCredentialKeeper) loop() {
	defer k.wg.Done()
	timer := time.NewTimer(k.nextInterval())
	defer timer.Stop()
	for {
		select {
		case <-k.stopCh:
			return
		case <-timer.C:
			k.runOnce(context.Background())
			timer.Reset(k.nextInterval())
		}
	}
}

// runOnce 遍历全部登录托管账号各探测一次；列表失败只记日志，不影响下一轮。
func (k *ZhipuCredentialKeeper) runOnce(ctx context.Context) {
	if k == nil || k.accountRepo == nil {
		return
	}
	accounts, err := k.accountRepo.ListAccountsByCredentialFlow(ctx, ZhipuLoginAuthFlow)
	if err != nil {
		slog.Warn("zhipu_credential_keeper.list_accounts_failed", "error", err)
		return
	}
	for i := range accounts {
		if ctx.Err() != nil {
			return
		}
		k.CheckAccount(ctx, &accounts[i])
	}
}

// nextInterval 返回本轮之后的等待时长：间隔 ± 抖动（随机源可注入，测试不 sleep）。
func (k *ZhipuCredentialKeeper) nextInterval() time.Duration {
	if k == nil || k.interval <= 0 {
		return 0
	}
	jitter := k.interval / 5
	if jitter > zhipuCredentialMaxJitter {
		jitter = zhipuCredentialMaxJitter
	}
	if jitter <= 0 {
		return k.interval
	}
	draw := k.randInt64n
	if draw == nil {
		draw = rand.Int64N
	}
	// 均匀分布 [0, 2*jitter]：与 upstream_billing_probe 同口径。
	return k.interval - jitter + time.Duration(draw(int64(2*jitter)+1))
}

// CheckAccount 对单个账号执行一次凭据健康判定：
//
//  1. 探针 401/403 → extra["zhipu_needs_relogin"] = true + 管理员通知；
//  2. 探针 2xx → 清除该标记；
//  3. 探针无结论（网络/瞬时故障/未接线）→ 不清标记，改判兜底证据：
//     CNProviderQuotaService 的 zhipu_usage_updated_at 长期停更 + 数据面错误含 401
//     → 同样标记（api_key 疑似失效）。
//
// 任何分支都不触碰调度状态（SetError / SetTempUnschedulable / schedulable）。
func (k *ZhipuCredentialKeeper) CheckAccount(ctx context.Context, account *Account) {
	if k == nil || k.probe == nil || account == nil {
		return
	}
	// 只处理 platform=zhipu 且 type=apikey 且 auth_flow=bigmodel_oauth 的登录托管账号（B1）。
	if !account.IsZhipuLoginManaged() {
		return
	}
	// 无 access_token 不发请求：登录态凭据不存在，探针只会返回 NoCredential。
	if strings.TrimSpace(account.GetZhipuAccessToken()) == "" {
		return
	}

	now := k.nowTime()
	_, err := k.probe.FetchUsageDetailForAccount(ctx, account, now, now)
	switch {
	case err == nil:
		k.clearNeedsRelogin(ctx, account)
		return
	case errors.Is(err, ErrZhipuCreditUsageUnauthorized):
		k.markNeedsRelogin(ctx, account, "credit-usage 探针判定登录态失效："+err.Error())
		return
	}

	// 探针未给出结论：保留既有标记（健康时不误清），仅在有 api_key 失效的兜底证据时标记。
	if detail, ok := k.staleUsageSnapshotWith401(account); ok {
		k.markNeedsRelogin(ctx, account, detail)
		return
	}
	slog.Warn("zhipu_credential_keeper.probe_inconclusive",
		"account_id", account.ID, "platform", account.Platform, "error", err)
}

// staleUsageSnapshotWith401 是 api_key 疑似失效的兜底证据链（数据面事件兜底）：
// 额度快照长期停更（api_key 探针持续失败，401/403 时上游不落快照）
// 且账号持久化错误含 401（ratelimit_service.handleAuthError 写入的 error_message）。
func (k *ZhipuCredentialKeeper) staleUsageSnapshotWith401(account *Account) (string, bool) {
	if account == nil {
		return "", false
	}
	staleFor, ok := k.usageSnapshotStaleFor(account)
	if !ok || staleFor < k.usageStaleAfterDuration() {
		return "", false
	}
	if !zhipuCredentialUnauthorizedPattern.MatchString(account.ErrorMessage) {
		return "", false
	}
	return fmt.Sprintf("api_key 疑似失效：额度快照 zhipu_usage_updated_at 已停更 %s 且数据面错误含 401",
		staleFor.Truncate(time.Minute)), true
}

// usageSnapshotStaleFor 返回 CNProviderQuotaService 额度快照的停更时长；
// 无快照或格式不符时 ok=false（此时不做任何判断）。
func (k *ZhipuCredentialKeeper) usageSnapshotStaleFor(account *Account) (time.Duration, bool) {
	if account == nil || len(account.Extra) == 0 {
		return 0, false
	}
	raw, ok := account.Extra[cnExtraKey(PlatformZhipu, cnExtraSuffixUsageUpdated)]
	if !ok {
		return 0, false
	}
	text, ok := raw.(string)
	if !ok {
		return 0, false
	}
	updatedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(text))
	if err != nil {
		return 0, false
	}
	return k.nowTime().Sub(updatedAt), true
}

// markNeedsRelogin 写标记并通知。首跨（false → true）才写库与通知，
// 避免每轮重复告警（口径同余额通知的首跨阈值语义）；标记已存在时直接返回。
func (k *ZhipuCredentialKeeper) markNeedsRelogin(ctx context.Context, account *Account, detail string) {
	if account == nil || account.ID <= 0 || k.accountRepo == nil {
		return
	}
	if accountNeedsRelogin(account) {
		return
	}
	if err := k.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{ZhipuNeedsReloginExtraKey: true}); err != nil {
		slog.Warn("zhipu_credential_keeper.mark_needs_relogin_failed", "account_id", account.ID, "error", err)
		return
	}
	setAccountExtraValue(account, ZhipuNeedsReloginExtraKey, true)
	if k.notifier != nil {
		k.notifier.NotifyZhipuCredentialInvalid(ctx, account, detail)
	}
}

// clearNeedsRelogin 清除标记；只在确有标记时写库（避免无意义的 extra 写入）。
func (k *ZhipuCredentialKeeper) clearNeedsRelogin(ctx context.Context, account *Account) {
	if account == nil || account.ID <= 0 || k.accountRepo == nil {
		return
	}
	if !accountNeedsRelogin(account) {
		return
	}
	if err := k.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{ZhipuNeedsReloginExtraKey: false}); err != nil {
		slog.Warn("zhipu_credential_keeper.clear_needs_relogin_failed", "account_id", account.ID, "error", err)
		return
	}
	setAccountExtraValue(account, ZhipuNeedsReloginExtraKey, false)
}

func (k *ZhipuCredentialKeeper) usageStaleAfterDuration() time.Duration {
	if k != nil && k.usageStaleAfter > 0 {
		return k.usageStaleAfter
	}
	return zhipuCredentialUsageStaleAfter
}

func (k *ZhipuCredentialKeeper) nowTime() time.Time {
	if k != nil && k.now != nil {
		return k.now()
	}
	return time.Now()
}

// accountNeedsRelogin 读取 extra 标记（非 bool 或缺失一律视为未标记）。
func accountNeedsRelogin(account *Account) bool {
	if account == nil {
		return false
	}
	marked, ok := account.Extra[ZhipuNeedsReloginExtraKey].(bool)
	return ok && marked
}

func setAccountExtraValue(account *Account, key string, value any) {
	if account == nil {
		return
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any, 1)
	}
	account.Extra[key] = value
}

// ---------------------------------------------------------------------------
// 通知接缝的默认实现
// ---------------------------------------------------------------------------

// zhipuCredentialAlertMailer 是邮件发送的最小接缝（*EmailService 满足）。
type zhipuCredentialAlertMailer interface {
	SendEmail(ctx context.Context, to, subject, body string) error
}

// ZhipuCredentialAlertNotifier 复用账号运维通知通道发送「需要重新登录」告警：
// 收件人来源与过滤规则同 BalanceNotifyService.getAccountQuotaNotifyEmails
// （SettingKeyAccountQuotaNotifyEmails + ParseNotifyEmails + filterVerifiedEmails），
// 开关同 SettingKeyAccountQuotaNotifyEnabled，发送走 EmailService，不新造渠道。
type ZhipuCredentialAlertNotifier struct {
	mailer      zhipuCredentialAlertMailer
	settingRepo SettingRepository
}

// NewZhipuCredentialAlertNotifier 构造默认通知实现。
func NewZhipuCredentialAlertNotifier(mailer zhipuCredentialAlertMailer, settingRepo SettingRepository) *ZhipuCredentialAlertNotifier {
	return &ZhipuCredentialAlertNotifier{mailer: mailer, settingRepo: settingRepo}
}

// NotifyZhipuCredentialInvalid 向管理员通知某账号需要重新登录。依赖缺失、开关关闭
// 或无有效收件人时静默跳过（与余额通知同口径，不影响 keeper 主流程）。
func (n *ZhipuCredentialAlertNotifier) NotifyZhipuCredentialInvalid(ctx context.Context, account *Account, detail string) {
	if n == nil || n.mailer == nil || n.settingRepo == nil || account == nil {
		return
	}
	if !n.accountOpsNotifyEnabled(ctx) {
		return
	}
	recipients := n.accountOpsNotifyRecipients(ctx)
	if len(recipients) == 0 {
		return
	}

	siteName := n.siteName(ctx)
	subject := fmt.Sprintf("[%s] 智谱登录账号需要重新登录：%s", siteName, account.Name)
	body := n.buildBody(account, detail, siteName)
	for _, to := range recipients {
		// 与 BalanceNotifyService.sendEmails 同口径：发送上下文脱离调用方，独立超时。
		sendCtx, cancel := context.WithTimeout(context.Background(), emailSendTimeout)
		if err := n.mailer.SendEmail(sendCtx, to, subject, body); err != nil {
			slog.Error("zhipu_credential_alert_send_failed", "account_id", account.ID, "to", to, "error", err)
		} else {
			slog.Info("zhipu_credential_alert_sent", "account_id", account.ID, "to", to)
		}
		cancel()
	}
}

func (n *ZhipuCredentialAlertNotifier) accountOpsNotifyEnabled(ctx context.Context) bool {
	val, err := n.settingRepo.GetValue(ctx, SettingKeyAccountQuotaNotifyEnabled)
	if err != nil {
		return false
	}
	return val == "true"
}

func (n *ZhipuCredentialAlertNotifier) accountOpsNotifyRecipients(ctx context.Context) []string {
	raw, err := n.settingRepo.GetValue(ctx, SettingKeyAccountQuotaNotifyEmails)
	if err != nil || strings.TrimSpace(raw) == "" || raw == "[]" {
		return nil
	}
	return filterVerifiedEmails(ParseNotifyEmails(raw))
}

func (n *ZhipuCredentialAlertNotifier) siteName(ctx context.Context) string {
	name, err := n.settingRepo.GetValue(ctx, SettingKeySiteName)
	if err != nil || name == "" {
		return defaultSiteName
	}
	return name
}

func (n *ZhipuCredentialAlertNotifier) buildBody(account *Account, detail, siteName string) string {
	return fmt.Sprintf(
		`<p>%s 的智谱登录账号 <strong>%s</strong>（ID %d）登录态已失效，需要管理员重新登录。</p>`+
			`<p>判定依据：%s</p>`+
			`<p>账号仍可继续转发（本告警不影响调度），请前往账号管理页执行「重新登录」。</p>`,
		html.EscapeString(siteName),
		html.EscapeString(account.Name),
		account.ID,
		html.EscapeString(detail),
	)
}
