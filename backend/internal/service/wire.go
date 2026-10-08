package service

import (
	"context"
	"database/sql"
	"os"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/zcodesign"
	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func ProvideGrokOAuthService(proxyRepo ProxyRepository, oauthClient GrokOAuthClient, cfg *config.Config, redisClient *redis.Client) *GrokOAuthService {
	svc := NewGrokOAuthService(proxyRepo, oauthClient, cfg)
	// wire.go is depguard-exempt for redis; construct the Redis session store here.
	if redisClient != nil {
		svc = svc.WithSessionStore(xai.NewRedisSessionStore(redisClient))
	}
	return svc
}

// ProvideCodeBuddyAdminService 构造管理面 CodeBuddy 专属服务（扫码纳管 / 签到）。
// wire.go is depguard-exempt for redis；Redis 未配置时扫码流程不可用（start
// 返回 store unavailable），签到不受影响。
func ProvideCodeBuddyAdminService(
	adminSvc AdminService,
	accountRepo AccountRepository,
	redisClient *redis.Client,
	httpUpstream HTTPUpstream,
) *CodeBuddyAdminService {
	var store codebuddy.Store
	if redisClient != nil {
		store = codebuddy.NewRedisStore(redisClient)
	}
	svc := NewCodeBuddyAdminService(adminSvc, accountRepo, store)
	// 解冻闭环（4.5）需要查实时积分；复用 A2 已有的 fetcher 实现。
	svc.SetCreditsFetcher(NewCodeBuddyCreditsFetcher(httpUpstream))
	return svc
}

// ProvideCodeBuddyCheckinScheduler 构造并启动签到调度器（4.1/4.2）。
//
// 每分钟 tick + 窗口内一次（形态说明见 codebuddy_checkin_scheduler.go）。
// 注册表里的功能默认关闭，所以调度器起来后在显式开启前不会发任何上游请求。
//
// 顺带把 CodeBuddyAdminService 注入网关的**业务码冷却执行者**（4.6）：
// NewOpenAIGatewayService 的形参里没有 CodeBuddyAdminService（网关先于 AdminService
// 构造，而 CodeBuddyAdminService 依赖 AdminService——直接加形参会成环），所以
// 必须在两边都构造完之后后置注入。这里两个依赖同时在场，是天然的锚点。
func ProvideCodeBuddyCheckinScheduler(
	codeBuddyAdminService *CodeBuddyAdminService,
	settingService *SettingService,
	openAIGatewayService *OpenAIGatewayService,
) *CodeBuddyCheckinScheduler {
	// 不注入的话 codeBuddyCooldownApplier 恒为 nil，
	// handleOpenAIAccountUpstreamError 里整段业务码冷却分支永不执行
	// ——4.6 会静默变成死代码。
	if openAIGatewayService != nil {
		openAIGatewayService.SetCodeBuddyCooldownApplier(codeBuddyAdminService)
	}
	scheduler := NewCodeBuddyCheckinScheduler(codeBuddyAdminService, settingService)
	scheduler.Start()

	// 设置页"立即执行"：走与自动签到**同一条**批量实现（CheckinAll），
	// 差别只在触发方式。不读开关/窗口——人明确要求现在就跑。
	RegisterPlatformFeatureImmediateRunner(
		PlatformCodeBuddy, CodeBuddyCheckinFeatureKey,
		func(ctx context.Context) (*PlatformFeatureRunResult, error) {
			summary, err := codeBuddyAdminService.CheckinAll(ctx)
			if err != nil {
				return nil, err
			}
			result := &PlatformFeatureRunResult{Detail: summary}
			if summary != nil {
				result.Summary = codeBuddyCheckinSummaryText(summary)
			}
			return result, nil
		},
	)
	return scheduler
}

// ProvideCodeBuddyActivityScheduler 构造并**启动**活跃上报执行器。
//
// # 授权沿革（2026-09-29 恢复自动排程）
//
// 用户 2026-09-22 的裁定曾把活跃上报归为 `full` 级 = 仅手动，本 provider
// 当时**刻意不调 Start()**。2026-09-29 用户重新裁定「开活跃上报+领养/夜猫/开学季」，
// 授权它进自动排程——所以这里恢复 `Start()`。
//
// 性质没有变：它仍在复刻官方客户端 `chat_request_send` 事件形状以过 `chat_5` 门槛。
// 变的是**政策**，且这是一个可追溯的显式决定（不是"顺手加回去的"）。
//
// 自动执行仍受三道门约束，其中"默认关闭"意味着**开启前一个请求都不发**：
//   - 平台功能开关（`codebuddy` / `activity`，默认 false）→ 需管理员在面板显式开启；
//   - 窗口（默认 10:00–11:00 CST）；
//   - 当日去重（进程内 + 账号级台账双重，重启不会重复上报）。
//
// accountRepo 用于按候选 ID 取回完整账号——候选列表只带 ID/名字，而上报需要
// 凭据（access_token / uid）与账号级台账（Account.Extra）。
func ProvideCodeBuddyActivityScheduler(
	codeBuddyAdminService *CodeBuddyAdminService,
	accountRepo AccountRepository,
	settingService *SettingService,
) *CodeBuddyActivityScheduler {
	scheduler := NewCodeBuddyActivityScheduler(codeBuddyAdminService, accountRepo, settingService)
	scheduler.Start()

	// 设置页"立即执行"：人明确要求现在上报。
	// 注意与**签到**的差别：这里走 `RunActivityNow` 而非自动路径，
	// 它按设计**绕开**窗口与账号级当日去重（人点了就该真发出去）。
	RegisterPlatformFeatureImmediateRunner(
		PlatformCodeBuddy, CodeBuddyActivityFeatureKey,
		func(ctx context.Context) (*PlatformFeatureRunResult, error) {
			summary := scheduler.RunActivityNow(ctx)
			return &PlatformFeatureRunResult{
				Summary: codeBuddyActivitySummaryText(summary),
				Detail:  summary,
			}, nil
		},
	)
	return scheduler
}

// ProvideCodeBuddyGrowthScheduler 构造并启动成长链调度器（A6 批 P5）。
//
// # 它能自动跑哪些通道：由**性质 + 授权**共同决定
//
// 成长链的通道集合不是靠"开发者记得别加"，而是靠调度器取通道列表的
// **唯一入口**：`codebuddy.CodeBuddyGrowthAutoRunnableChannelKeys()` 按
// `CodeBuddyGrowthChannelSpec.AutoRunnable()` 过滤——
// 性质天然可自动的（preview/claim）直接放行；性质需人担责的（full）
// 只有 `AutoAuthorized` 显式置位（且写明依据）才会被放行。
//
// 当前 full 级里：adopt / night_cat / school 已授权（2026-09-29 用户裁定），
// **lottery 未授权**（一次抽光全部次数、不可恢复）——它进不了这个列表。
// 守它的是 `TestCodeBuddyGrowthSchedulerNeverRunsFullTierChannels`。
//
// 所以这里**可以**调 Start()（与活跃上报不同），因为它的通道集合已被分级约束。
func ProvideCodeBuddyGrowthScheduler(
	codeBuddyAdminService *CodeBuddyAdminService,
	accountRepo AccountRepository,
	settingService *SettingService,
) *CodeBuddyGrowthScheduler {
	scheduler := NewCodeBuddyGrowthScheduler(codeBuddyAdminService, accountRepo, settingService)
	scheduler.Start()

	// 设置页"立即执行"：跑一轮**已授权自动**的通道（未授权的结构上带不上）。
	// 与自动排程的差别只在触发方式——通道集合、并发、单号隔离完全同一实现。
	RegisterPlatformFeatureImmediateRunner(
		PlatformCodeBuddy, CodeBuddyGrowthFeatureKey,
		func(ctx context.Context) (*PlatformFeatureRunResult, error) {
			summary := codeBuddyAdminService.RunCodeBuddyGrowthAllNow(ctx)
			return &PlatformFeatureRunResult{
				Summary: codeBuddyGrowthSummaryText(summary),
				Detail:  summary,
			}, nil
		},
	)
	return scheduler
}

// BuildInfo contains build information
type BuildInfo struct {
	Version   string
	BuildType string
}

// ProvidePricingService creates and initializes PricingService
func ProvidePricingService(cfg *config.Config, remoteClient PricingRemoteClient) (*PricingService, error) {
	svc := NewPricingService(cfg, remoteClient)
	if err := svc.Initialize(); err != nil {
		// Pricing service initialization failure should not block startup, use fallback prices
		println("[Service] Warning: Pricing service initialization failed:", err.Error())
	}
	return svc, nil
}

// ProvideUpdateService creates UpdateService with BuildInfo
func ProvideUpdateService(cache UpdateCache, githubClient GitHubReleaseClient, buildInfo BuildInfo) *UpdateService {
	return NewUpdateService(cache, githubClient, buildInfo.Version, buildInfo.BuildType)
}

// ProvideEmailQueueService creates EmailQueueService with default worker count
func ProvideEmailQueueService(emailService *EmailService) *EmailQueueService {
	return NewEmailQueueService(emailService, 3)
}

// ProvideAuthService wires the optional captcha providers into AuthService while
// keeping NewAuthService's public constructor compatible with existing tests.
func ProvideAuthService(
	entClient *dbent.Client,
	userRepo UserRepository,
	redeemRepo RedeemCodeRepository,
	refreshTokenCache RefreshTokenCache,
	cfg *config.Config,
	settingService *SettingService,
	emailService *EmailService,
	turnstileService *TurnstileService,
	tencentCaptchaService *TencentCaptchaService,
	aliyunCaptchaService *AliyunCaptchaService,
	emailQueueService *EmailQueueService,
	promoService *PromoService,
	defaultSubAssigner DefaultSubscriptionAssigner,
	affiliateService *AffiliateService,
	userPlatformQuotaRepo UserPlatformQuotaRepository,
) *AuthService {
	svc := NewAuthService(
		entClient,
		userRepo,
		redeemRepo,
		refreshTokenCache,
		cfg,
		settingService,
		emailService,
		turnstileService,
		emailQueueService,
		promoService,
		defaultSubAssigner,
		affiliateService,
		userPlatformQuotaRepo,
	)
	svc.SetTencentCaptchaService(tencentCaptchaService)
	svc.SetAliyunCaptchaService(aliyunCaptchaService)
	return svc
}

// ProvideOAuthRefreshAPI creates OAuthRefreshAPI with the default lock TTL.
func ProvideOAuthRefreshAPI(accountRepo AccountRepository, tokenCache GeminiTokenCache) *OAuthRefreshAPI {
	return NewOAuthRefreshAPI(accountRepo, tokenCache)
}

func ProvideBatchImageModelPricingResolver(resolver *ModelPricingResolver) *BatchImageModelPricingResolver {
	return &BatchImageModelPricingResolver{Resolver: resolver}
}

func ProvideBatchImageCleanupService(repo BatchImageRepository, accountRepo AccountRepository, cfg *config.Config) *BatchImageCleanupService {
	svc := NewBatchImageCleanupService(repo, accountRepo, cfg)
	svc.Start()
	return svc
}

// ProvideOpenAIOAuthService creates OpenAIOAuthService with privacy/account enrichment support.
func ProvideOpenAIOAuthService(
	proxyRepo ProxyRepository,
	oauthClient OpenAIOAuthClient,
	privacyClientFactory PrivacyClientFactory,
) *OpenAIOAuthService {
	svc := NewOpenAIOAuthService(proxyRepo, oauthClient)
	svc.SetPrivacyClientFactory(privacyClientFactory)
	return svc
}

// ProvideTokenRefreshService creates and starts TokenRefreshService
func ProvideTokenRefreshService(
	accountRepo AccountRepository,
	oauthService *OAuthService,
	openaiOAuthService *OpenAIOAuthService,
	geminiOAuthService *GeminiOAuthService,
	antigravityOAuthService *AntigravityOAuthService,
	grokOAuthService *GrokOAuthService,
	cacheInvalidator TokenCacheInvalidator,
	schedulerCache SchedulerCache,
	cfg *config.Config,
	tempUnschedCache TempUnschedCache,
	privacyClientFactory PrivacyClientFactory,
	proxyRepo ProxyRepository,
	refreshAPI *OAuthRefreshAPI,
	runtimeBlocker AccountRuntimeBlocker,
) *TokenRefreshService {
	svc := NewTokenRefreshService(accountRepo, oauthService, openaiOAuthService, geminiOAuthService, antigravityOAuthService, cacheInvalidator, schedulerCache, cfg, tempUnschedCache, grokOAuthService)
	// 注入 OpenAI privacy opt-out 依赖
	svc.SetPrivacyDeps(privacyClientFactory, proxyRepo)
	// 注入统一 OAuth 刷新 API（消除 TokenRefreshService 与 TokenProvider 之间的竞争条件）
	svc.SetRefreshAPI(refreshAPI)
	// 调用侧显式注入后台刷新策略，避免策略漂移
	svc.SetRefreshPolicy(DefaultBackgroundRefreshPolicy())
	svc.SetAccountRuntimeBlocker(runtimeBlocker)
	svc.Start()
	return svc
}

// ProvideClaudeTokenProvider creates ClaudeTokenProvider with OAuthRefreshAPI injection
func ProvideClaudeTokenProvider(
	accountRepo AccountRepository,
	tokenCache GeminiTokenCache,
	oauthService *OAuthService,
	refreshAPI *OAuthRefreshAPI,
) *ClaudeTokenProvider {
	p := NewClaudeTokenProvider(accountRepo, tokenCache, oauthService)
	executor := NewClaudeTokenRefresher(oauthService)
	p.SetRefreshAPI(refreshAPI, executor)
	p.SetRefreshPolicy(ClaudeProviderRefreshPolicy())
	return p
}

// ProvideOpenAITokenProvider creates OpenAITokenProvider with OAuthRefreshAPI injection
func ProvideOpenAITokenProvider(
	accountRepo AccountRepository,
	tokenCache GeminiTokenCache,
	openaiOAuthService *OpenAIOAuthService,
	refreshAPI *OAuthRefreshAPI,
) *OpenAITokenProvider {
	p := NewOpenAITokenProvider(accountRepo, tokenCache, openaiOAuthService)
	executor := NewOpenAITokenRefresher(openaiOAuthService, accountRepo)
	p.SetRefreshAPI(refreshAPI, executor)
	p.SetRefreshPolicy(OpenAIProviderRefreshPolicy())
	return p
}

// ProvideOpenAIQuotaService wires the OpenAI quota query/reset service.
// It depends on the OpenAI token provider for refreshed access tokens and the
// privacy client factory for the impersonated upstream HTTP client.
func ProvideOpenAIQuotaService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	tokenProvider *OpenAITokenProvider,
	privacyClientFactory PrivacyClientFactory,
	openAIGatewayService *OpenAIGatewayService,
) *OpenAIQuotaService {
	service := NewOpenAIQuotaService(accountRepo, proxyRepo, tokenProvider, privacyClientFactory)
	service.agentIdentityWS = openAIGatewayService
	return service
}

// ProvideOpenAIQuotaAutoResetService 启动账号级自动用卡队列与补偿扫描。
func ProvideOpenAIQuotaAutoResetService(
	accountRepo AccountRepository,
	quotaService *OpenAIQuotaService,
	rateLimitService *RateLimitService,
	idempotency *IdempotencyCoordinator,
	audit *AuditLogService,
	settingService *SettingService,
	leaderLock LeaderLockCache,
) *OpenAIQuotaAutoResetService {
	service := NewOpenAIQuotaAutoResetService(
		accountRepo,
		quotaService,
		rateLimitService,
		idempotency,
		audit,
		settingService,
		leaderLock,
	)
	service.Start()
	return service
}

func ProvideAccountUsageService(
	accountRepo AccountRepository,
	usageLogRepo UsageLogRepository,
	usageFetcher ClaudeUsageFetcher,
	geminiQuotaService *GeminiQuotaService,
	antigravityQuotaFetcher *AntigravityQuotaFetcher,
	grokQuotaFetcher *GrokQuotaFetcher,
	grokQuotaService *GrokQuotaService,
	openAIQuotaService *OpenAIQuotaService,
	cache *UsageCache,
	identityCache IdentityCache,
	tlsFPProfileService *TLSFingerprintProfileService,
	openAIGatewayService *OpenAIGatewayService,
	httpUpstream HTTPUpstream,
) *AccountUsageService {
	service := NewAccountUsageService(
		accountRepo,
		usageLogRepo,
		usageFetcher,
		geminiQuotaService,
		antigravityQuotaFetcher,
		grokQuotaFetcher,
		grokQuotaService,
		openAIQuotaService,
		cache,
		identityCache,
		tlsFPProfileService,
		NewOpenCodeUsageFetcher(httpUpstream),
		NewUpstreamBalanceFetcher(httpUpstream),
	)
	service.agentIdentityWS = openAIGatewayService
	service.SetAccountRuntimeBlocker(openAIGatewayService)
	// CodeBuddy 实时积分 fetcher（A2）：追加式 DI，不改既有构造签名。
	service.SetCodeBuddyCreditsFetcher(NewCodeBuddyCreditsFetcher(httpUpstream))
	return service
}

func ProvideAccountTestService(
	accountRepo AccountRepository,
	geminiTokenProvider *GeminiTokenProvider,
	claudeTokenProvider *ClaudeTokenProvider,
	grokTokenProvider *GrokTokenProvider,
	antigravityGatewayService *AntigravityGatewayService,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
	tlsFPProfileService *TLSFingerprintProfileService,
	openAIGatewayService *OpenAIGatewayService,
	settingService *SettingService,
	pluginManager *PluginManager,
) *AccountTestService {
	service := NewAccountTestService(
		accountRepo,
		geminiTokenProvider,
		claudeTokenProvider,
		grokTokenProvider,
		antigravityGatewayService,
		httpUpstream,
		cfg,
		tlsFPProfileService,
	)
	service.agentIdentityWS = openAIGatewayService
	service.SetOpenAIGatewayService(openAIGatewayService)
	service.SetSettingService(settingService)
	service.SetPluginManager(pluginManager)
	return service
}

func ProvideGrokQuotaService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	tokenProvider *GrokTokenProvider,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
	usageLogRepo UsageLogRepository,
	settingService *SettingService,
) *GrokQuotaService {
	service := NewGrokQuotaService(accountRepo, proxyRepo, tokenProvider, httpUpstream, cfg, usageLogRepo)
	service.SetSettingService(settingService)
	return service
}

// ProvideCNProviderQuotaService 构造国产供应商 Coding Plan 额度探测服务。
func ProvideCNProviderQuotaService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
) *CNProviderQuotaService {
	return NewCNProviderQuotaService(accountRepo, proxyRepo, httpUpstream, cfg)
}

// ProvideCNProviderBalanceService 构造国产供应商余额探测服务。
func ProvideCNProviderBalanceService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
) *CNProviderBalanceService {
	return NewCNProviderBalanceService(accountRepo, proxyRepo, httpUpstream, cfg)
}

// ProvideCNProviderBalanceCheckService 构造并启动周期余额/额度检测任务。
// payg 账号探余额（低余额停调）；coding plan 账号探 5h/weekly 滚动窗口
// （落 extra 快照供调度阈值评估自动停调）。
// 间隔取自 gateway.cn_providers.balance_check_interval_minutes；<=0 或关闭时不启动。
func ProvideCNProviderBalanceCheckService(
	accountRepo AccountRepository,
	balanceService *CNProviderBalanceService,
	quotaService *CNProviderQuotaService,
	cfg *config.Config,
) *CNProviderBalanceCheckService {
	minutes := 10
	if cfg != nil && cfg.Gateway.CNProviders.BalanceCheckIntervalMinutes > 0 {
		minutes = cfg.Gateway.CNProviders.BalanceCheckIntervalMinutes
	}
	svc := NewCNProviderBalanceCheckService(accountRepo, balanceService, quotaService, cfg, time.Duration(minutes)*time.Minute)
	svc.Start()
	return svc
}

// ProvideZhipuOAuthService 构造智谱登录编排服务（design M1）：授权 URL、兑换、
// api_key 解析、建号凭据、重登。会话存储为进程内 10 分钟 TTL（Redis 化见 P4）。
func ProvideZhipuOAuthService(
	proxyRepo ProxyRepository,
	httpUpstream HTTPUpstream,
	accountRepo AccountRepository,
	cfg *config.Config,
) *ZhipuOAuthService {
	return NewZhipuOAuthService(proxyRepo, httpUpstream, accountRepo, cfg)
}

// ProvideGeminiTokenProvider creates GeminiTokenProvider with OAuthRefreshAPI injection
func ProvideGeminiTokenProvider(
	accountRepo AccountRepository,
	tokenCache GeminiTokenCache,
	geminiOAuthService *GeminiOAuthService,
	refreshAPI *OAuthRefreshAPI,
) *GeminiTokenProvider {
	p := NewGeminiTokenProvider(accountRepo, tokenCache, geminiOAuthService)
	executor := NewGeminiTokenRefresher(geminiOAuthService)
	p.SetRefreshAPI(refreshAPI, executor)
	p.SetRefreshPolicy(GeminiProviderRefreshPolicy())
	return p
}

// ProvideAntigravityTokenProvider creates AntigravityTokenProvider with OAuthRefreshAPI injection
func ProvideAntigravityTokenProvider(
	accountRepo AccountRepository,
	tokenCache GeminiTokenCache,
	antigravityOAuthService *AntigravityOAuthService,
	refreshAPI *OAuthRefreshAPI,
	tempUnschedCache TempUnschedCache,
) *AntigravityTokenProvider {
	p := NewAntigravityTokenProvider(accountRepo, tokenCache, antigravityOAuthService)
	executor := NewAntigravityTokenRefresher(antigravityOAuthService)
	p.SetRefreshAPI(refreshAPI, executor)
	p.SetRefreshPolicy(AntigravityProviderRefreshPolicy())
	p.SetTempUnschedCache(tempUnschedCache)
	return p
}

// ProvideGrokTokenProvider creates GrokTokenProvider with OAuthRefreshAPI injection.
func ProvideGrokTokenProvider(
	accountRepo AccountRepository,
	tokenCache GeminiTokenCache,
	grokOAuthService *GrokOAuthService,
	refreshAPI *OAuthRefreshAPI,
	tempUnschedCache TempUnschedCache,
) *GrokTokenProvider {
	p := NewGrokTokenProvider(accountRepo, tokenCache)
	executor := NewGrokTokenRefresher(grokOAuthService)
	p.SetRefreshAPI(refreshAPI, executor)
	p.SetRefreshPolicy(GrokProviderRefreshPolicy())
	p.SetTempUnschedCache(tempUnschedCache)
	return p
}

// ProvideDashboardAggregationService 创建并启动仪表盘聚合服务
func ProvideDashboardAggregationService(repo DashboardAggregationRepository, timingWheel *TimingWheelService, lockCache LeaderLockCache, db *sql.DB, cfg *config.Config) *DashboardAggregationService {
	svc := NewDashboardAggregationService(repo, timingWheel, cfg)
	svc.SetLeaderLock(lockCache, db)
	svc.Start()
	return svc
}

// ProvideUsageCleanupService 创建并启动使用记录清理任务服务
func ProvideUsageCleanupService(repo UsageCleanupRepository, timingWheel *TimingWheelService, dashboardAgg *DashboardAggregationService, cfg *config.Config) *UsageCleanupService {
	svc := NewUsageCleanupService(repo, timingWheel, dashboardAgg, cfg)
	svc.Start()
	return svc
}

// ProvideAccountExpiryService creates and starts AccountExpiryService.
func ProvideAccountExpiryService(accountRepo AccountRepository) *AccountExpiryService {
	svc := NewAccountExpiryService(accountRepo, time.Minute)
	svc.Start()
	return svc
}

// ProvideOpenAICodexVersionSyncService creates and starts OpenAICodexVersionSyncService.
// 出站 Codex 身份的版本号靠它跟随官方发布，无需为了跟版本而发新版本；面板可关闭。
func ProvideOpenAICodexVersionSyncService(
	settingRepo SettingRepository,
	settingService *SettingService,
	githubClient GitHubReleaseClient,
) *OpenAICodexVersionSyncService {
	svc := NewOpenAICodexVersionSyncService(settingRepo, settingService, githubClient, openAICodexVersionSyncInterval)
	svc.Start()
	return svc
}

// ProvideProxyExpiryService creates and starts ProxyExpiryService.
func ProvideProxyExpiryService(proxyRepo ProxyRepository) *ProxyExpiryService {
	svc := NewProxyExpiryService(proxyRepo, time.Minute)
	svc.Start()
	return svc
}

// ProvideSubscriptionExpiryService creates and starts SubscriptionExpiryService.
func ProvideSubscriptionExpiryService(userSubRepo UserSubscriptionRepository, settingRepo SettingRepository, notificationEmailService *NotificationEmailService, lockCache LeaderLockCache, db *sql.DB) *SubscriptionExpiryService {
	svc := NewSubscriptionExpiryService(userSubRepo, time.Minute)
	svc.SetSettingRepository(settingRepo)
	svc.SetNotificationEmailService(notificationEmailService)
	svc.SetLeaderLock(lockCache, db)
	svc.Start()
	return svc
}

// ProvideTimingWheelService creates and starts TimingWheelService
func ProvideTimingWheelService() (*TimingWheelService, error) {
	svc, err := NewTimingWheelService()
	if err != nil {
		return nil, err
	}
	svc.Start()
	return svc, nil
}

// ProvideDeferredService creates and starts DeferredService
func ProvideDeferredService(accountRepo AccountRepository, timingWheel *TimingWheelService) *DeferredService {
	svc := NewDeferredService(accountRepo, timingWheel, 10*time.Second)
	svc.Start()
	return svc
}

// ProvideConcurrencyService creates ConcurrencyService and starts slot cleanup worker.
func ProvideConcurrencyService(cache ConcurrencyCache, accountRepo AccountRepository, cfg *config.Config) *ConcurrencyService {
	svc := NewConcurrencyService(cache)
	if err := svc.CleanupStaleProcessSlots(context.Background()); err != nil {
		logger.LegacyPrintf("service.concurrency", "Warning: startup cleanup stale process slots failed: %v", err)
	}
	if cfg != nil {
		svc.SetAccountLoadBatchCacheTTL(time.Duration(cfg.Gateway.Scheduling.LoadBatchCacheTTLMS) * time.Millisecond)
		svc.StartSlotCleanupWorker(accountRepo, cfg.Gateway.Scheduling.SlotCleanupInterval)
	}
	return svc
}

// ProvideUserMessageQueueService 创建用户消息串行队列服务并启动清理 worker
func ProvideUserMessageQueueService(cache UserMsgQueueCache, rpmCache RPMCache, cfg *config.Config) *UserMessageQueueService {
	svc := NewUserMessageQueueService(cache, rpmCache, &cfg.Gateway.UserMessageQueue)
	if cfg.Gateway.UserMessageQueue.CleanupIntervalSeconds > 0 {
		svc.StartCleanupWorker(time.Duration(cfg.Gateway.UserMessageQueue.CleanupIntervalSeconds) * time.Second)
	}
	return svc
}

// ProvideSchedulerSnapshotService creates and starts SchedulerSnapshotService.
func ProvideSchedulerSnapshotService(
	cache SchedulerCache,
	outboxRepo SchedulerOutboxRepository,
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	cfg *config.Config,
) *SchedulerSnapshotService {
	svc := NewSchedulerSnapshotService(cache, outboxRepo, accountRepo, groupRepo, cfg)
	svc.Start()
	return svc
}

// ProvideRateLimitService creates RateLimitService with optional dependencies.
func ProvideRateLimitService(
	accountRepo AccountRepository,
	usageRepo UsageLogRepository,
	cfg *config.Config,
	geminiQuotaService *GeminiQuotaService,
	tempUnschedCache TempUnschedCache,
	timeoutCounterCache TimeoutCounterCache,
	openAI403CounterCache OpenAI403CounterCache,
	settingService *SettingService,
	tokenCacheInvalidator TokenCacheInvalidator,
	ollamaCloudUsage *OllamaCloudUsageService,
	httpUpstream HTTPUpstream,
) *RateLimitService {
	svc := NewRateLimitService(accountRepo, usageRepo, cfg, geminiQuotaService, tempUnschedCache)
	if healthCache, ok := tempUnschedCache.(OpenAIAPIKeyHealthCache); ok {
		svc.SetOpenAIAPIKeyHealthCache(healthCache)
	}
	svc.SetTimeoutCounterCache(timeoutCounterCache)
	svc.SetOpenAI403CounterCache(openAI403CounterCache)
	svc.SetSettingService(settingService)
	svc.SetTokenCacheInvalidator(tokenCacheInvalidator)
	svc.SetOllamaCloudUsageProbeScheduler(ollamaCloudUsage)
	svc.SetOpenCodeUsageFetcher(NewOpenCodeUsageFetcher(httpUpstream))
	return svc
}

// ProvideOpsMetricsCollector creates and starts OpsMetricsCollector.
func ProvideOpsMetricsCollector(
	opsRepo OpsRepository,
	settingRepo SettingRepository,
	accountRepo AccountRepository,
	concurrencyService *ConcurrencyService,
	db *sql.DB,
	redisClient *redis.Client,
	cfg *config.Config,
) *OpsMetricsCollector {
	collector := NewOpsMetricsCollector(opsRepo, settingRepo, accountRepo, concurrencyService, db, redisClient, cfg)
	collector.Start()
	return collector
}

// ProvideOpsAggregationService creates and starts OpsAggregationService (hourly/daily pre-aggregation).
func ProvideOpsAggregationService(
	opsRepo OpsRepository,
	settingRepo SettingRepository,
	db *sql.DB,
	redisClient *redis.Client,
	cfg *config.Config,
) *OpsAggregationService {
	svc := NewOpsAggregationService(opsRepo, settingRepo, db, redisClient, cfg)
	svc.Start()
	return svc
}

// ProvideOpsAlertEvaluatorService creates and starts OpsAlertEvaluatorService.
// zhipuSignAlerts 注入智谱签名内置指标源（design M3.1(d) / 票 25）：评估周期据此计算
// zhipu_sign_fail_window 与 zhipu_sign_effective_rate 两个内置指标；为 nil（未接线）时
// 两个指标「不可计算」，其余规则评估不受影响。
func ProvideOpsAlertEvaluatorService(
	opsService *OpsService,
	opsRepo OpsRepository,
	emailService *EmailService,
	redisClient *redis.Client,
	cfg *config.Config,
	proxyRepo ProxyRepository,
	zhipuSignAlerts *ZhipuSignAlerts,
) *OpsAlertEvaluatorService {
	svc := NewOpsAlertEvaluatorService(opsService, opsRepo, emailService, redisClient, cfg, proxyRepo)
	svc.SetZhipuSignMetrics(zhipuSignAlerts)
	svc.Start()
	return svc
}

// ProvideOpsCleanupService creates and starts OpsCleanupService (cron scheduled).
// channelMonitorSvc 让维护任务（聚合 + 历史/聚合软删）跟随 ops 清理 cron 一起跑，
// 共享 leader lock + heartbeat。
// settingRepo 让 cleanup service 自己读 ops_advanced_settings.data_retention 覆盖 cfg；
// opsService 用来反向注入 cleanup hook，以便 UI 改清理设置时能 Reload cron。
func ProvideOpsCleanupService(
	opsRepo OpsRepository,
	db *sql.DB,
	redisClient *redis.Client,
	cfg *config.Config,
	channelMonitorSvc *ChannelMonitorService,
	settingRepo SettingRepository,
	opsService *OpsService,
) *OpsCleanupService {
	svc := NewOpsCleanupService(opsRepo, db, redisClient, cfg, channelMonitorSvc, settingRepo)
	svc.Start()
	if opsService != nil {
		opsService.SetCleanupReloader(svc)
	}
	return svc
}

func ProvideOpsSystemLogSink(opsRepo OpsRepository) *OpsSystemLogSink {
	sink := NewOpsSystemLogSink(opsRepo)
	sink.Start()
	logger.SetSink(sink)
	return sink
}

// ProvideAuditLogService 创建操作审计日志服务并启动异步写入与保留期清理协程。
// 停止逻辑挂在 cmd/server 的 provideCleanup。
func ProvideAuditLogService(repo AuditLogRepository, settingService *SettingService) *AuditLogService {
	svc := NewAuditLogService(repo, settingService)
	svc.Start()
	return svc
}

func buildIdempotencyConfig(cfg *config.Config) IdempotencyConfig {
	idempotencyCfg := DefaultIdempotencyConfig()
	if cfg != nil {
		if cfg.Idempotency.DefaultTTLSeconds > 0 {
			idempotencyCfg.DefaultTTL = time.Duration(cfg.Idempotency.DefaultTTLSeconds) * time.Second
		}
		if cfg.Idempotency.SystemOperationTTLSeconds > 0 {
			idempotencyCfg.SystemOperationTTL = time.Duration(cfg.Idempotency.SystemOperationTTLSeconds) * time.Second
		}
		if cfg.Idempotency.ProcessingTimeoutSeconds > 0 {
			idempotencyCfg.ProcessingTimeout = time.Duration(cfg.Idempotency.ProcessingTimeoutSeconds) * time.Second
		}
		if cfg.Idempotency.FailedRetryBackoffSeconds > 0 {
			idempotencyCfg.FailedRetryBackoff = time.Duration(cfg.Idempotency.FailedRetryBackoffSeconds) * time.Second
		}
		if cfg.Idempotency.MaxStoredResponseLen > 0 {
			idempotencyCfg.MaxStoredResponseLen = cfg.Idempotency.MaxStoredResponseLen
		}
		idempotencyCfg.ObserveOnly = cfg.Idempotency.ObserveOnly
	}
	return idempotencyCfg
}

func ProvideIdempotencyCoordinator(repo IdempotencyRepository, cfg *config.Config) *IdempotencyCoordinator {
	coordinator := NewIdempotencyCoordinator(repo, buildIdempotencyConfig(cfg))
	SetDefaultIdempotencyCoordinator(coordinator)
	return coordinator
}

func ProvideSystemOperationLockService(repo IdempotencyRepository, cfg *config.Config) *SystemOperationLockService {
	return NewSystemOperationLockService(repo, buildIdempotencyConfig(cfg))
}

func ProvideIdempotencyCleanupService(repo IdempotencyRepository, cfg *config.Config) *IdempotencyCleanupService {
	svc := NewIdempotencyCleanupService(repo, cfg)
	svc.Start()
	return svc
}

// ProvideScheduledTestService creates ScheduledTestService.
func ProvideScheduledTestService(
	planRepo ScheduledTestPlanRepository,
	resultRepo ScheduledTestResultRepository,
) *ScheduledTestService {
	return NewScheduledTestService(planRepo, resultRepo)
}

// ProvideScheduledTestRunnerService creates and starts ScheduledTestRunnerService.
func ProvideScheduledTestRunnerService(
	planRepo ScheduledTestPlanRepository,
	scheduledSvc *ScheduledTestService,
	accountTestSvc *AccountTestService,
	rateLimitSvc *RateLimitService,
	cfg *config.Config,
) *ScheduledTestRunnerService {
	svc := NewScheduledTestRunnerService(planRepo, scheduledSvc, accountTestSvc, rateLimitSvc, cfg)
	svc.Start()
	return svc
}

// ProvideOpsScheduledReportService creates and starts OpsScheduledReportService.
func ProvideOpsScheduledReportService(
	opsService *OpsService,
	userService *UserService,
	emailService *EmailService,
	redisClient *redis.Client,
	cfg *config.Config,
) *OpsScheduledReportService {
	svc := NewOpsScheduledReportService(opsService, userService, emailService, redisClient, cfg)
	svc.Start()
	return svc
}

// ProvideAPIKeyAuthCacheInvalidator 提供 API Key 认证缓存失效能力
func ProvideAPIKeyAuthCacheInvalidator(apiKeyService *APIKeyService) APIKeyAuthCacheInvalidator {
	// Start Pub/Sub subscriber for L1 cache invalidation across instances
	apiKeyService.StartAuthCacheInvalidationSubscriber(context.Background())
	return apiKeyService
}

// ProvideImageStorageSettingService 构造异步生图对象存储的后台设置服务。
//
// config.yaml 里的 image_storage 作为回落：后台从未保存过设置时沿用它，
// 使升级前已通过配置文件开启该功能的部署不被打断。
func ProvideImageStorageSettingService(
	settingRepo SettingRepository,
	encryptor SecretEncryptor,
	backup *BackupService,
	factory ImageStorageFactory,
	cfg *config.Config,
) *ImageStorageSettingService {
	if cfg.ImageStorage.Enabled && !cfg.ImageStorage.Active() {
		// 列出具体缺失的键。若这些键其实已在环境变量里设过，说明它们没被读进来，
		// 请确认 setDefaults 中已为其注册默认值（见 config.setEnvReachableDefaults）。
		logger.L().Warn("image_storage.enabled is true in config but object storage is not fully configured; configure it in the admin UI or complete the config file",
			zap.Strings("missing_keys", cfg.ImageStorage.MissingCredentialKeys()))
	}
	return NewImageStorageSettingService(settingRepo, encryptor, backup, factory, cfg.ImageStorage)
}

// ProvideImageTaskService 构造异步图片任务服务。
//
// 对象存储是异步图片任务的启用前提：仅当开关打开且凭证齐全时功能才可用，否则整体禁用
// （handler 返回 404，不创建任务、不写 Redis），从而避免大 base64 结果撑爆 Redis。
// 启用状态由 settings 服务在运行时解析，因此后台改开关后无需重启即可生效。
func ProvideImageTaskService(store ImageTaskStore, settings *ImageStorageSettingService) *ImageTaskService {
	return NewImageTaskServiceWithResolver(store, settings.Resolver(), defaultImageTaskTTL, defaultImageTaskExecutionTimeout)
}

// ProvideImageBedService 构造站点图床服务并启动 TTL 清理定时器（票 #36）。
//
// 对象存储优先走 ImageStorageSettingService 的 resolver（与 ProvideImageTaskService 同源）：
// 后台改 image_storage 开关/凭证后图床无需重启即可生效。
//
// 没有 S3 的部署不应卡在 503 上：resolver 不可用且 gateway.image_bed.local_enabled
// （默认开启）时退化为本地磁盘存储——站点用自己的磁盘当图床，公开直链由本站
// 匿名读路由 GET /v1/images/bed/:key 提供（智谱识图工具链只能匿名抓取）。
// 两者都不可用时上传仍回 503，而不是 500。
// 清理定时器的停止逻辑挂在 cmd/server 的 provideCleanup。
func ProvideImageBedService(
	repo ImageBedUploadRepository,
	settings *ImageStorageSettingService,
	owners ImageBedOwnerResolver,
	counter ImageBedQuotaCounter,
	cfg *config.Config,
	local ImageBedLocalStorage,
) *ImageBedService {
	var resolve ImageStorageResolver
	if settings != nil {
		resolve = settings.Resolver()
	}
	// 本地兜底只在开关打开时注入：开关关闭的部署保持既有语义——
	// 没有可用存储时上传回 503，公开直链回 404。
	if cfg == nil || !cfg.Gateway.ImageBed.LocalEnabled {
		local = nil
	}
	if local != nil {
		resolve = imageBedResolverWithLocalFallback(resolve, local)
	}
	svc := NewImageBedService(repo, resolve, owners, counter, cfg)
	svc.SetLocalImageBedStorage(local)
	svc.StartCleanup()
	return svc
}

// imageBedResolverWithLocalFallback 把本地磁盘存储接到 resolver 之后：
// S3 可用时永远优先 S3（配好对象存储的部署行为不变），否则用本地兜底。
// 每次调用都重新问 S3（后台可能刚配上凭证），本地兜底是常量级返回。
func imageBedResolverWithLocalFallback(s3 ImageStorageResolver, local ImageBedLocalStorage) ImageStorageResolver {
	localUploader := NewImageResultUploader(local, "", 0, nil)
	return func() (*ImageResultUploader, bool) {
		if s3 != nil {
			if uploader, ok := s3(); ok && uploader != nil {
				return uploader, true
			}
		}
		return localUploader, true
	}
}

// ProvideImageBedOwnerResolver 把 APIKeyService 收窄成图床需要的归属解析接口。
//
// 上传只带网关鉴权得到的 apiKeyID，而记账行要 user_id；这里用闭包适配
// （imageBedOwnerResolverFunc），既不让 service 依赖 APIKeyService 的宽接口，
// 也避免为一次 ID→UserID 查询引入新的仓储。key 不存在时返回 ErrAPIKeyNotFound，
// 调用方按原错误上抛（测试断言 ErrorIs 到该哨兵）。
func ProvideImageBedOwnerResolver(apiKeyService *APIKeyService) ImageBedOwnerResolver {
	return imageBedOwnerResolverFunc(func(ctx context.Context, apiKeyID int64) (int64, error) {
		if apiKeyService == nil {
			return 0, ErrAPIKeyNotFound
		}
		apiKey, err := apiKeyService.GetByID(ctx, apiKeyID)
		if err != nil {
			return 0, err
		}
		if apiKey == nil {
			return 0, ErrAPIKeyNotFound
		}
		return apiKey.UserID, nil
	})
}

// ProvideBackupService creates and starts BackupService
func ProvideBackupService(
	settingRepo SettingRepository,
	cfg *config.Config,
	encryptor SecretEncryptor,
	storeFactory BackupObjectStoreFactory,
	dumper DBDumper,
	lockCache LeaderLockCache,
	db *sql.DB,
) *BackupService {
	svc := NewBackupService(settingRepo, cfg, encryptor, storeFactory, dumper)
	svc.SetLeaderLock(lockCache, db)
	svc.Start()
	return svc
}

// ProvideOpsService constructs OpsService and wires the SettingService-backed quota
// auto-pause cache sink. Mirrors the SetCleanupReloader pattern: OpsService doesn't
// hold a *SettingService reference, but wire injects a tiny callback so writes to
// ops_advanced_settings immediately propagate into the scheduler hot-path cache.
func ProvideOpsService(
	opsRepo OpsRepository,
	settingRepo SettingRepository,
	cfg *config.Config,
	accountRepo AccountRepository,
	userRepo UserRepository,
	concurrencyService *ConcurrencyService,
	gatewayService *GatewayService,
	openAIGatewayService *OpenAIGatewayService,
	geminiCompatService *GeminiMessagesCompatService,
	antigravityGatewayService *AntigravityGatewayService,
	systemLogSink *OpsSystemLogSink,
	settingService *SettingService,
	authCacheInvalidationWorker *AuthCacheInvalidationWorker,
	apiKeyService *APIKeyService,
) *OpsService {
	svc := NewOpsService(
		opsRepo,
		settingRepo,
		cfg,
		accountRepo,
		userRepo,
		concurrencyService,
		gatewayService,
		openAIGatewayService,
		geminiCompatService,
		antigravityGatewayService,
		systemLogSink,
	)
	if settingService != nil {
		svc.SetOpenAIQuotaAutoPauseSettingsSink(settingService.SetOpenAIQuotaAutoPauseSettings)
		// Optional warm-up so the first scheduled request after process start observes
		// a populated cache rather than zero defaults. Best-effort, sync-bounded.
		settingService.WarmOpenAIQuotaAutoPauseSettings(context.Background())
	}
	svc.authCacheInvalidationWorker = authCacheInvalidationWorker
	svc.apiKeyService = apiKeyService
	svc.StartRuntimeSettingsRefresh(context.Background())
	return svc
}

// ProvideOpsIngressRejectAggregator starts the bounded security aggregation
// runtime and attaches it to OpsService, which is the middleware recorder.
func ProvideOpsIngressRejectAggregator(opsRepo OpsRepository, opsService *OpsService) *OpsIngressRejectAggregator {
	repo, ok := opsRepo.(OpsIngressRejectRepository)
	if !ok {
		return nil
	}
	aggregator := NewOpsIngressRejectAggregator(repo)
	aggregator.Start()
	opsService.SetIngressRejectAggregator(aggregator)
	return aggregator
}

// ProvideSettingService wires SettingService with group reader and proxy repo.
func ProvideSettingService(settingRepo SettingRepository, groupRepo GroupRepository, proxyRepo ProxyRepository, cfg *config.Config) *SettingService {
	svc := NewSettingService(settingRepo, cfg)
	svc.SetDefaultSubscriptionGroupReader(groupRepo)
	svc.SetProxyRepository(proxyRepo)
	if err := svc.LoadForwardedClientIPSettings(context.Background()); err != nil {
		logger.LegacyPrintf("service.setting", "Warning: load forwarded client IP settings failed: %v", err)
	}
	if err := svc.MigrateOpenAIAllowClaudeCodeCodexPluginSetting(context.Background()); err != nil {
		logger.LegacyPrintf("service.setting", "Warning: migrate openai allow Claude Code Codex plugin setting failed: %v", err)
	}
	if err := svc.MigrateCodexBodyFingerprintToSignals(context.Background()); err != nil {
		logger.LegacyPrintf("service.setting", "Warning: migrate codex body fingerprint to signals failed: %v", err)
	}
	if err := svc.MigrateGrokDefaultTextModel(context.Background()); err != nil {
		logger.LegacyPrintf("service.setting", "Warning: migrate Grok default text model failed: %v", err)
	}
	antigravity.SetUserAgentVersionResolver(svc.GetAntigravityUserAgentVersion)
	// enforceCodexIdentityHeaders 是所有 Codex 出站路径共用的纯函数收口点，拿不到 ctx，
	// 故注入无参解析器；解析器内部自带 60s TTL 缓存，热路径不触库。
	SetCodexCanonicalUserAgentResolver(func() string {
		return svc.GetOpenAICodexCanonicalUserAgent(context.Background())
	})
	return svc
}

// ProvideBillingCacheService wires BillingCacheService with its RPM dependencies.
func ProvideBillingCacheService(
	cache BillingCache,
	userRepo UserRepository,
	subRepo UserSubscriptionRepository,
	apiKeyRepo APIKeyRepository,
	rpmCache UserRPMCache,
	rateRepo UserGroupRateRepository,
	cfg *config.Config,
	userPlatformQuotaRepo UserPlatformQuotaRepository,
) *BillingCacheService {
	return NewBillingCacheService(cache, userRepo, subRepo, apiKeyRepo, rpmCache, rateRepo, cfg, userPlatformQuotaRepo)
}

// ProvideAPIKeyService wires APIKeyService and connects rate-limit cache invalidation.
func ProvideAPIKeyService(
	apiKeyRepo APIKeyRepository,
	userRepo UserRepository,
	groupRepo GroupRepository,
	userSubRepo UserSubscriptionRepository,
	userGroupRateRepo UserGroupRateRepository,
	cache APIKeyCache,
	cfg *config.Config,
	billingCacheService *BillingCacheService,
	concurrencyService *ConcurrencyService,
	pricingPlanRepo PricingPlanRepository,
) *APIKeyService {
	svc := NewAPIKeyService(apiKeyRepo, userRepo, groupRepo, userSubRepo, userGroupRateRepo, cache, cfg)
	svc.SetRateLimitCacheInvalidator(billingCacheService)
	svc.SetConcurrencyService(concurrencyService)
	svc.SetPricingPlanRepository(pricingPlanRepo)
	// 把 APIKeyService 的认证缓存失效能力反向注入套餐仓储（可选接口）：
	// 套餐内容/删除变更时按 planID 批量失效绑定 Key 的快照（L2 + 跨实例
	// L1 广播）。走接口断言而非仓储构造参数，避免仓储与 APIKeyService
	// 形成构造环（APIKeyService 构造时又依赖 PricingPlanRepository）。
	if settable, ok := pricingPlanRepo.(interface {
		SetAuthCacheInvalidator(APIKeyAuthCacheInvalidator)
	}); ok {
		settable.SetAuthCacheInvalidator(svc)
	}
	return svc
}

// ProvideZhipuClientSigner 构造智谱数据面签名器（design M3 / 票 22）：握手 origin 固定为
// 数据面域名（open.bigmodel.cn），客户端版本与私钥 TTL 取 gateway.zhipu 配置，握手复用
// 网关共享 HTTP 上游栈。票 28 通过 Signer.SetOptions 做配置热更新。
func ProvideZhipuClientSigner(cfg *config.Config, httpUpstream HTTPUpstream) zhipuClientSigner {
	clientVersion := ""
	keyTTL := time.Duration(0)
	if cfg != nil {
		clientVersion = cfg.Gateway.Zhipu.SignClientVersion
		keyTTL = time.Duration(cfg.Gateway.Zhipu.SignKeyTTLMinutes) * time.Minute
	}
	return zcodesign.NewSigner(zhipuSignOrigin, clientVersion, keyTTL, zhipuSignHTTPDoer{upstream: httpUpstream})
}

// ProvideZhipuSignAlerts 构造智谱签名 L1 指标 / fail 策略 / 账号级熔断引擎（design
// M3.1 / 票 24），并把只读熔断状态接回管理端状态投影（票 28 的状态接口里
// circuit_break_* 与 runtime_state_available 两个字段）。counterCache 为 nil
// （未配置 Redis / 未装配实现）时计数器退化为进程内存并只告警一次，不影响数据面。
func ProvideZhipuSignAlerts(
	cfg *config.Config,
	counterCache ZhipuSignCounterCache,
	signConfig *ZhipuSignConfigService,
) *ZhipuSignAlerts {
	alerts := NewZhipuSignAlerts(cfg, counterCache, signConfig, time.Now)
	signConfig.SetCircuitBreakReader(alerts)
	return alerts
}

// ProvideZhipuAccountMonitorService 构造智谱登录态监控服务（design M4 / 票 11），
// 并在同一装配点完成 L2 费率对账器的注入（票 27 接线）：构造对账器、接 #28 的
// 生效配置面（运行层覆盖热生效）、挂到监控服务的周期入口与快照合并口。
// zhipuSignAlerts / signConfig 为 nil 时对账器退化为未接线（等价回滚）。
func ProvideZhipuAccountMonitorService(
	accountRepo AccountRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
	zhipuSignAlerts *ZhipuSignAlerts,
	signConfig *ZhipuSignConfigService,
) *ZhipuAccountMonitorService {
	monitor := NewZhipuAccountMonitorService(accountRepo, httpUpstream, cfg)
	if zhipuSignAlerts != nil && signConfig != nil {
		reconciler := NewZhipuSignReconciler(cfg, accountRepo, monitor, zhipuSignAlerts, nil)
		reconciler.SetConfigSource(signConfig)
		monitor.SetSignReconciler(reconciler)
	}
	return monitor
}

// ProvideZhipuCredentialKeeper 构造并启动智谱登录凭据周期探测（design M2 / 票 09）：
// 探针 = 监控服务；L2 对账的周期入口挂在每轮探测末尾（票 27 接线）；管理员邮件
// 通知复用既有邮箱服务与设置仓储。interval <= 0 时 Start 空转（构造仍成功），
// 便于配置回滚。
func ProvideZhipuCredentialKeeper(
	accountRepo AccountRepository,
	monitor *ZhipuAccountMonitorService,
	emailService *EmailService,
	settingRepo SettingRepository,
	cfg *config.Config,
) *ZhipuCredentialKeeper {
	notifier := NewZhipuCredentialAlertNotifier(emailService, settingRepo)
	keeper := NewZhipuCredentialKeeper(accountRepo, monitor, notifier, cfg)
	if monitor != nil {
		keeper.SetSignReconcileHook(monitor.RunDueSignReconcile)
	}
	keeper.Start()
	return keeper
}

// ProvideOpenAIGatewayService 构造 OpenAI 网关并注入智谱签名器与 L1 指标引擎。
//
// 与 NewOpenAIGatewayService 分开是为了不动既有构造函数签名：大量测试直接调用它，
// 未注入签名器时签名整体关闭（零行为变化），wire 装配路径才接上真实 Signer。
func ProvideOpenAIGatewayService(
	accountRepo AccountRepository,
	usageLogRepo UsageLogRepository,
	usageBillingRepo UsageBillingRepository,
	userRepo UserRepository,
	userSubRepo UserSubscriptionRepository,
	userGroupRateRepo UserGroupRateRepository,
	cache GatewayCache,
	cfg *config.Config,
	schedulerSnapshot *SchedulerSnapshotService,
	concurrencyService *ConcurrencyService,
	billingService *BillingService,
	rateLimitService *RateLimitService,
	billingCacheService *BillingCacheService,
	httpUpstream HTTPUpstream,
	deferredService *DeferredService,
	openAITokenProvider *OpenAITokenProvider,
	grokTokenProvider *GrokTokenProvider,
	resolver *ModelPricingResolver,
	channelService *ChannelService,
	balanceNotifyService *BalanceNotifyService,
	settingService *SettingService,
	userPlatformQuotaRepo UserPlatformQuotaRepository,
	zhipuSigner zhipuClientSigner,
	zhipuSignAlerts *ZhipuSignAlerts,
) *OpenAIGatewayService {
	svc := NewOpenAIGatewayService(
		accountRepo, usageLogRepo, usageBillingRepo, userRepo, userSubRepo, userGroupRateRepo,
		cache, cfg, schedulerSnapshot, concurrencyService, billingService, rateLimitService,
		billingCacheService, httpUpstream, deferredService, openAITokenProvider, grokTokenProvider,
		resolver, channelService, balanceNotifyService, settingService, userPlatformQuotaRepo,
	)
	svc.zhipuSigner = zhipuSigner
	svc.zhipuSignAlerts = zhipuSignAlerts
	return svc
}

// ProvideGatewayService wires GatewayService and connects the pricing plan
// repository (used for plan-aware layer resolution; hot path reads the
// auth-cache plan snapshot instead of the repository).
func ProvideGatewayService(
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	usageLogRepo UsageLogRepository,
	usageBillingRepo UsageBillingRepository,
	userRepo UserRepository,
	userSubRepo UserSubscriptionRepository,
	userGroupRateRepo UserGroupRateRepository,
	cache GatewayCache,
	cfg *config.Config,
	schedulerSnapshot *SchedulerSnapshotService,
	concurrencyService *ConcurrencyService,
	billingService *BillingService,
	rateLimitService *RateLimitService,
	billingCacheService *BillingCacheService,
	identityService *IdentityService,
	httpUpstream HTTPUpstream,
	deferredService *DeferredService,
	claudeTokenProvider *ClaudeTokenProvider,
	sessionLimitCache SessionLimitCache,
	rpmCache RPMCache,
	digestStore *DigestSessionStore,
	settingService *SettingService,
	tlsFPProfileService *TLSFingerprintProfileService,
	channelService *ChannelService,
	resolver *ModelPricingResolver,
	compositeResolver *CompositeRouteResolver,
	balanceNotifyService *BalanceNotifyService,
	userPlatformQuotaRepo UserPlatformQuotaRepository,
	pricingPlanRepo PricingPlanRepository,
) *GatewayService {
	svc := NewGatewayService(
		accountRepo, groupRepo, usageLogRepo, usageBillingRepo, userRepo, userSubRepo,
		userGroupRateRepo, cache, cfg, schedulerSnapshot, concurrencyService, billingService,
		rateLimitService, billingCacheService, identityService, httpUpstream, deferredService,
		claudeTokenProvider, sessionLimitCache, rpmCache, digestStore, settingService,
		tlsFPProfileService, channelService, resolver, compositeResolver, balanceNotifyService,
		userPlatformQuotaRepo,
	)
	svc.SetPricingPlanRepository(pricingPlanRepo)
	return svc
}

// ProviderSet is the Wire provider set for all services
var ProviderSet = wire.NewSet(
	// Core services
	ProvideAuthService,
	NewPasskeyService,
	NewUserService,
	ProvideAPIKeyService,
	ProvideAPIKeyAuthCacheInvalidator,
	ProvideAuthCacheInvalidationWorker,
	NewGroupService,
	NewCompositeRouteResolver,
	NewAccountService,
	NewProxyService,
	NewRedeemService,
	NewPromoService,
	NewUsageService,
	NewDashboardService,
	ProvidePricingService,
	NewBillingService,
	ProvideBillingCacheService,
	NewAnnouncementService,
	NewAdminService,
	ProvideGatewayService,
	ProvideOpenAIGatewayService,
	ProvideZhipuClientSigner,
	ProvideZhipuSignAlerts,
	ProvideZhipuSignRuntime,
	ProvideZhipuAccountMonitorService,
	ProvideZhipuCredentialKeeper,
	ProvideImageStorageSettingService,
	ProvideImageTaskService,
	ProvideImageBedService,
	ProvideImageBedOwnerResolver,
	ProvideBatchImageModelPricingResolver,
	NewBatchImagePublicService,
	NewBatchImageDownloadService,
	ProvideBatchImageCleanupService,
	ProvideBatchImageWorkerRuntime,
	wire.Bind(new(AccountRuntimeBlocker), new(*OpenAIGatewayService)),
	NewOAuthService,
	ProvideOpenAIOAuthService,
	ProvideGrokOAuthService,
	ProvideCodeBuddyAdminService,
	ProvideCodeBuddyCheckinScheduler,
	ProvideCodeBuddyActivityScheduler,
	ProvideCodeBuddyGrowthScheduler,
	ProvideCodeBuddyZeroCreditGate,
	wire.Bind(new(GrokOAuthTokenService), new(*GrokOAuthService)),
	NewGeminiOAuthService,
	NewGeminiQuotaService,
	NewCompositeTokenCacheInvalidator,
	wire.Bind(new(TokenCacheInvalidator), new(*CompositeTokenCacheInvalidator)),
	NewAntigravityOAuthService,
	ProvideOAuthRefreshAPI,
	ProvideGeminiTokenProvider,
	NewGeminiMessagesCompatService,
	ProvideAntigravityTokenProvider,
	ProvideGrokTokenProvider,
	ProvideOpenAITokenProvider,
	ProvideOpenAIQuotaService,
	ProvideOpenAIQuotaAutoResetService,
	ProvideGrokQuotaService,
	ProvideCNProviderQuotaService,
	ProvideCNProviderBalanceService,
	ProvideCNProviderBalanceCheckService,
	ProvideZhipuOAuthService,
	ProvideClaudeTokenProvider,
	NewAntigravityGatewayService,
	ProvideRateLimitService,
	ProvideAccountUsageService,
	ProvideAccountTestService,
	ProvideUpstreamBillingProbeService,
	ProvideOllamaCloudUsageService,
	ProvideSettingService,
	NewDataManagementService,
	ProvideBackupService,
	ProvideOpsSystemLogSink,
	ProvideOpsService,
	ProvideOpsIngressRejectAggregator,
	ProvideAuditLogService,
	ProvideOpsMetricsCollector,
	ProvideOpsAggregationService,
	ProvideOpsAlertEvaluatorService,
	ProvideOpsCleanupService,
	ProvideOpsScheduledReportService,
	NewEmailService,
	NewNotificationEmailService,
	ProvideEmailQueueService,
	NewTurnstileService,
	NewTencentCaptchaService,
	NewAliyunCaptchaService,
	NewSubscriptionService,
	wire.Bind(new(DefaultSubscriptionAssigner), new(*SubscriptionService)),
	ProvideConcurrencyService,
	ProvideUserMessageQueueService,
	NewUsageRecordWorkerPool,
	ProvideSchedulerSnapshotService,
	NewIdentityService,
	NewCRSSyncService,
	ProvideUpdateService,
	ProvideTokenRefreshService,
	wire.Bind(new(GrokOAuthReconciler), new(*TokenRefreshService)),
	ProvideAccountExpiryService,
	ProvideOpenAICodexVersionSyncService,
	ProvideProxyExpiryService,
	ProvideSubscriptionExpiryService,
	ProvideTimingWheelService,
	ProvideDashboardAggregationService,
	ProvideUsageCleanupService,
	ProvideDeferredService,
	NewAntigravityQuotaFetcher,
	NewGrokQuotaFetcher,
	NewUserAttributeService,
	NewUsageCache,
	NewTotpService,
	NewErrorPassthroughService,
	NewTLSFingerprintProfileService,
	NewPluginManager,
	NewDigestSessionStore,
	ProvideIdempotencyCoordinator,
	ProvideSystemOperationLockService,
	ProvideIdempotencyCleanupService,
	ProvideScheduledTestService,
	ProvideScheduledTestRunnerService,
	NewGroupCapacityService,
	NewChannelService,
	wire.Bind(new(ChannelCacheInvalidator), new(*ChannelService)),
	NewModelPricingResolver,
	NewModelPlazaService,
	NewContentModerationService,
	NewAffiliateService,
	ProvidePaymentConfigService,
	ProvidePaymentService,
	ProvidePaymentOrderExpiryService,
	ProvideBalanceNotifyService,
	ProvideChannelMonitorService,
	ProvideChannelMonitorRunner,
	NewChannelMonitorQuotaFetcher,
	ProvideChannelMonitorV2Service,
	ProvideChannelMonitorV2Aggregator,
	NewChannelMonitorRequestTemplateService,
	ProvideUserPlatformQuotaUsageFlusher,
	NewPricingPlanService, // 定价套餐管理端服务（CRUD 含模型协议条目与路由层）
)

// ProvideUserPlatformQuotaUsageFlusher 创建并启动 UserPlatformQuotaUsageFlusher。
func ProvideUserPlatformQuotaUsageFlusher(cfg *config.Config, cache BillingCache, quotaRepo UserPlatformQuotaRepository, tw *TimingWheelService) *UserPlatformQuotaUsageFlusher {
	svc := NewUserPlatformQuotaUsageFlusher(cfg, cache, quotaRepo, tw)
	svc.Start()
	return svc
}

// ProvidePaymentConfigService wraps NewPaymentConfigService to accept the named
// payment.EncryptionKey type instead of raw []byte, avoiding Wire ambiguity.
func ProvidePaymentConfigService(entClient *dbent.Client, settingRepo SettingRepository, key payment.EncryptionKey) *PaymentConfigService {
	return NewPaymentConfigService(entClient, settingRepo, []byte(key))
}

// ProvideBalanceNotifyService creates BalanceNotifyService
func ProvideBalanceNotifyService(emailService *EmailService, settingRepo SettingRepository, accountRepo AccountRepository, notificationEmailService *NotificationEmailService) *BalanceNotifyService {
	svc := NewBalanceNotifyService(emailService, settingRepo, accountRepo)
	svc.SetNotificationEmailService(notificationEmailService)
	return svc
}

// ProvidePaymentService creates PaymentService and attaches notification email delivery.
func ProvidePaymentService(entClient *dbent.Client, registry *payment.Registry, loadBalancer payment.LoadBalancer, redeemService *RedeemService, subscriptionSvc *SubscriptionService, configService *PaymentConfigService, userRepo UserRepository, groupRepo GroupRepository, affiliateService *AffiliateService, notificationEmailService *NotificationEmailService) *PaymentService {
	svc := NewPaymentService(entClient, registry, loadBalancer, redeemService, subscriptionSvc, configService, userRepo, groupRepo, affiliateService)
	svc.SetNotificationEmailService(notificationEmailService)
	return svc
}

// ProvidePaymentOrderExpiryService creates and starts PaymentOrderExpiryService.
func ProvidePaymentOrderExpiryService(paymentSvc *PaymentService, lockCache LeaderLockCache, db *sql.DB) *PaymentOrderExpiryService {
	svc := NewPaymentOrderExpiryService(paymentSvc, 60*time.Second)
	svc.SetLeaderLock(lockCache, db)
	svc.Start()
	return svc
}

// ProvideChannelMonitorService 创建渠道监控服务（CRUD + RunCheck + 用户视图聚合）。
// 加密器复用 wire 中已注入的 SecretEncryptor（AES-256-GCM）。
// settingService gates RunCheck via channel_monitor_enabled + channel_monitor_mode.
func ProvideChannelMonitorService(
	repo ChannelMonitorRepository,
	encryptor SecretEncryptor,
	settingService *SettingService,
) *ChannelMonitorService {
	svc := NewChannelMonitorService(repo, encryptor)
	svc.SetRuntimeReader(settingService)
	return svc
}

// ProvideChannelMonitorRunner 创建并启动渠道监控调度器。
// 通过 SetScheduler 注入回 service 后再 Start，确保启动时加载所有 enabled monitor，
// 后续 CRUD 也能即时同步任务表。Runner.Stop 由 cleanup function 调用。
// settingService 用于 runner 每次 fire 读取功能开关。
// quotaFetcher（账号侧用量聚合）也在此注入：accountUsage/CN 服务在 wire 图中
// 晚于 channelMonitorService 构造，走 setter 注入避免调整既有构造顺序。
func ProvideChannelMonitorRunner(
	svc *ChannelMonitorService,
	settingService *SettingService,
	quotaFetcher *ChannelMonitorQuotaFetcher,
) *ChannelMonitorRunner {
	r := NewChannelMonitorRunner(svc, settingService)
	if svc != nil {
		// Ensure runtime reader is set even if ProvideChannelMonitorService
		// was constructed without settings (tests / alternate providers).
		svc.SetRuntimeReader(settingService)
		svc.SetScheduler(r)
		svc.SetQuotaFetcher(quotaFetcher)
	}
	r.Start()
	return r
}

// ProvideChannelMonitorV2Service wires settings for user-facing privacy flags
// (e.g. hide RPM/TPM throughput).
func ProvideChannelMonitorV2Service(repo ChannelMonitorV2Repository, settingService *SettingService) *ChannelMonitorV2Service {
	svc := NewChannelMonitorV2Service(repo)
	svc.SetRuntimeReader(settingService)
	return svc
}

// ProvideChannelMonitorV2Aggregator starts the passive minute-rollup worker.
// Aggregation only runs when channel_monitor_enabled=true and mode=v2 (and V2 config enabled).
// Set CHANNEL_MONITOR_V2_DISABLE_AGGREGATOR=1 to skip Start (local demo with seeded facts).
func ProvideChannelMonitorV2Aggregator(repo ChannelMonitorV2Repository, db *sql.DB, settingService *SettingService) *ChannelMonitorV2Aggregator {
	aggregator := NewChannelMonitorV2Aggregator(repo, db, settingService)
	if os.Getenv("CHANNEL_MONITOR_V2_DISABLE_AGGREGATOR") == "1" {
		return aggregator
	}
	aggregator.Start()
	return aggregator
}

// ProvideCodeBuddyZeroCreditGate 构造并启动 CodeBuddy 0 积分主动门（票 #37）。
//
// 周期取自 gateway.codebuddy.zero_credit_check_interval_minutes（<=0 → 默认 30 分钟，
// 不把"键没配"与"键配成 0"混为一谈）；关门的开关是
// gateway.codebuddy.zero_credit_gate_enabled（默认 true），两者都在构造里读一次。
// 探测走 A2 的 CodeBuddyCreditsFetcher（同一实现，不另写积分解析）。
func ProvideCodeBuddyZeroCreditGate(
	accountRepo AccountRepository,
	httpUpstream HTTPUpstream,
	cfg *config.Config,
) *CodeBuddyZeroCreditGate {
	minutes := codeBuddyZeroCreditDefaultIntervalMinutes
	if cfg != nil && cfg.Gateway.CodeBuddy.ZeroCreditCheckIntervalMinutes > 0 {
		minutes = cfg.Gateway.CodeBuddy.ZeroCreditCheckIntervalMinutes
	}
	gate := NewCodeBuddyZeroCreditGate(
		accountRepo,
		NewCodeBuddyCreditsFetcher(httpUpstream),
		cfg,
		time.Duration(minutes)*time.Minute,
	)
	gate.Start()
	return gate
}

// ProvideCodeBuddyTokenKeepaliveScheduler 组装并启动 token 保活调度（T3）。
// 默认关闭：平台功能 codebuddy_token_keepalive 显式开启后才会对上游发刷新。
func ProvideCodeBuddyTokenKeepaliveScheduler(
	codeBuddyAdminService *CodeBuddyAdminService,
	accountRepo AccountRepository,
	settingService *SettingService,
) *CodeBuddyTokenKeepaliveScheduler {
	// 条件回写通道：accountRepository 运行时类型实现了
	// CodeBuddyOAuthRefreshSuccessRepository（401 failover 同款），断言取用；
	// 未实现时传 nil＝只刷不持久化（测试桩场景）。
	var credsRepo CodeBuddyOAuthRefreshSuccessRepository
	if cr, ok := accountRepo.(CodeBuddyOAuthRefreshSuccessRepository); ok {
		credsRepo = cr
	}
	keepalive := NewCodeBuddyTokenKeepalive(accountRepo, credsRepo, NewCodeBuddyTokenRefresher())
	scheduler := NewCodeBuddyTokenKeepaliveScheduler(keepalive, settingService)
	scheduler.Start()
	return scheduler
}
