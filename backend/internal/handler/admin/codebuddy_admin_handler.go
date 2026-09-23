package admin

import (
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/platform/codebuddy"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// CodeBuddyAdminHandler 暴露管理面 CodeBuddy 专属端点（A1 扫码纳管 / A3 签到）。
//
//   - POST /admin/codebuddy/qr/start           发起扫码（返回 {state, authUrl}）
//   - GET  /admin/codebuddy/qr/poll?state=     轮询（只答发起者；他人 404）
//   - POST /admin/codebuddy/accounts/:id/checkin  每日签到（0 / 10001 幂等）
type CodeBuddyAdminHandler struct {
	codeBuddyService *service.CodeBuddyAdminService
	// activityScheduler 活跃上报执行器（`full` 级，仅手动入口，不自动跑）。
	activityScheduler *service.CodeBuddyActivityScheduler
	// audit 审计服务（建号处显式 Record，不赌中间件推导；poll 是 GET，中间件
	// 天然不记录，成功纳管必须显式写 actor/uid/nickname/create|update）。
	audit *service.AuditLogService
}

func NewCodeBuddyAdminHandler(
	codeBuddyService *service.CodeBuddyAdminService,
	audit *service.AuditLogService,
	activityScheduler *service.CodeBuddyActivityScheduler,
) *CodeBuddyAdminHandler {
	return &CodeBuddyAdminHandler{
		codeBuddyService:  codeBuddyService,
		audit:             audit,
		activityScheduler: activityScheduler,
	}
}

// QRStart POST /admin/codebuddy/qr/start。
func (h *CodeBuddyAdminHandler) QRStart(c *gin.Context) {
	if h == nil || h.codeBuddyService == nil {
		response.BadRequest(c, "codebuddy admin service is not enabled")
		return
	}
	actor, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || actor.UserID <= 0 {
		response.Unauthorized(c, "operator identity required for qr start")
		return
	}
	result, err := h.codeBuddyService.Start(c.Request.Context(), strconv.FormatInt(actor.UserID, 10))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	middleware.SetAuditAction(c, "admin.codebuddy.qr.start")
	response.Success(c, result)
}

// QRPoll GET /admin/codebuddy/qr/poll?state=。
func (h *CodeBuddyAdminHandler) QRPoll(c *gin.Context) {
	if h == nil || h.codeBuddyService == nil {
		response.BadRequest(c, "codebuddy admin service is not enabled")
		return
	}
	actor, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || actor.UserID <= 0 {
		response.Unauthorized(c, "operator identity required for qr poll")
		return
	}
	result, err := h.codeBuddyService.Poll(
		c.Request.Context(),
		strconv.FormatInt(actor.UserID, 10),
		c.Query("state"),
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if result.Status == "ok" && result.Account != nil {
		// 建号处显式写审计（actor/uid/nickname/create|update，不赌中间件推导）。
		middleware.SetAuditAction(c, "admin.codebuddy.qr.import")
		middleware.SetAuditExtra(c, map[string]any{
			"uid":      result.Account.UID,
			"nickname": result.Account.Nickname,
			"result":   codebuddyImportResultName(result.Account.Created),
		})
		h.recordImportAudit(c, result.Account)
	}
	response.Success(c, result)
}

// recordImportAudit 显式写入纳管审计（poll 是 GET 不经审计中间件；若中间件
// 已接管未来变更，Record 仍幂等无副作用——审计异步批量落库）。
func (h *CodeBuddyAdminHandler) recordImportAudit(c *gin.Context, account *CodeBuddyAdminAccountRef) {
	if h == nil || h.audit == nil || account == nil {
		return
	}
	subject, _ := middleware.GetAuthSubjectFromContext(c)
	entry := &service.AuditLog{
		CreatedAt: time.Now().UTC(),
		Method:    c.Request.Method,
		Path:      "/api/v1/admin/codebuddy/qr/poll",
		ClientIP:  middleware.SecurityClientIP(c),
		UserAgent: c.Request.UserAgent(),
		Action:    "admin.codebuddy.qr.import",
		Extra: map[string]any{
			"uid":      account.UID,
			"nickname": account.Nickname,
			"result":   codebuddyImportResultName(account.Created),
		},
		StatusCode: c.Writer.Status(),
	}
	if subject.UserID > 0 {
		uid := subject.UserID
		entry.ActorUserID = &uid
		entry.AuthMethod = service.AuditAuthMethodJWT
	}
	entry.ActorEmail = c.GetString(middleware.ContextKeyAuthEmail)
	h.audit.Record(entry)
}

// CodeBuddyAdminAccountRef recordImportAudit 参数最小引用（避免与 poll result
// 深关联）。
type CodeBuddyAdminAccountRef = service.CodeBuddyQRAccountSummary

// Checkin POST /admin/codebuddy/accounts/:id/checkin。
func (h *CodeBuddyAdminHandler) Checkin(c *gin.Context) {
	if h == nil || h.codeBuddyService == nil {
		response.BadRequest(c, "codebuddy admin service is not enabled")
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	result, err := h.codeBuddyService.Checkin(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	middleware.SetAuditAction(c, "admin.codebuddy.checkin")
	response.Success(c, result)
}

// CheckinAll POST /admin/codebuddy/accounts/checkin-all。
//
// 批量签到：逐账号汇总（成功 / 已签到 / 失败 / 跳过），单账号失败不影响其他账号。
// 部分失败仍然返回 200 + 明细，而不是整体报错——否则管理员拿不到"哪些成功了"。
func (h *CodeBuddyAdminHandler) CheckinAll(c *gin.Context) {
	if h == nil || h.codeBuddyService == nil {
		response.BadRequest(c, "codebuddy admin service is not enabled")
		return
	}
	result, err := h.codeBuddyService.CheckinAll(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// 审计只记汇总计数，不写账号级明细（明细可能上百条，审计表不适合承载）。
	middleware.SetAuditAction(c, "admin.codebuddy.checkin_all")
	if result != nil {
		middleware.SetAuditExtra(c, map[string]any{
			"total":     result.Total,
			"succeeded": result.Succeeded,
			"already":   result.AlreadyCheckedIn,
			"failed":    result.Failed,
			"skipped":   result.Skipped,
		})
	}
	response.Success(c, result)
}

func codebuddyImportResultName(created bool) string {
	if created {
		return "create"
	}
	return "update"
}

// --- 成长链手动入口（A6 批 P5）---
//
// ## 为什么这些必须是**手动**端点
//
// 用户裁定的三级分级里 `full` = 含伪造活跃上报语义 = **仅手动，不得进自动排程**。
// 层级说明：`full` 通道（领养 / 夜猫子 / 开学季点亮）没有自动排程可走，
// 所以**管理端点是它们唯一的存在形式**——没有这些端点，分级裁定等于把功能删掉。
//
// 对应地，`preview` / `claim` 级通道（旅行、连登、trial）有自动排程，
// 但这里也提供手动入口：联调与排障时需要一个"立刻跑一次"的开关，
// 否则只能等下一个窗口。

// GrowthChannels GET /admin/codebuddy/growth/channels。
//
// 列出全部成长通道及其**合规分级**，供管理端渲染与运维核对。
// 这个只读端点存在的意义：让人不必翻代码就能确认"哪些通道会自动跑、哪些只能手动"。
func (h *CodeBuddyAdminHandler) GrowthChannels(c *gin.Context) {
	channels := make([]gin.H, 0)
	for _, spec := range codebuddy.GrowthChannelSpecsForAPI() {
		channels = append(channels, gin.H{
			"key":           spec.Key,
			"tier":          spec.Tier,
			"auto_runnable": spec.AutoRunnable,
			"rationale":     spec.Rationale,
		})
	}
	middleware.SetAuditAction(c, "admin.codebuddy.growth.channels")
	response.Success(c, gin.H{"channels": channels})
}

// GrowthRunChannel POST /admin/codebuddy/accounts/:id/growth/run。
//
// 手动执行**一个**成长通道。body: {"channel": "<key>"}。
//
// 这是 `full` 级通道（领养 / 夜猫子 / 开学季）的唯一入口。
// **不读平台功能开关**：手动触发意味着人明确要求执行，
// 被"自动排程的开关"挡住会让人点了没反应且找不到原因。
func (h *CodeBuddyAdminHandler) GrowthRunChannel(c *gin.Context) {
	if h == nil || h.codeBuddyService == nil {
		response.BadRequest(c, "codebuddy admin service is not enabled")
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var body struct {
		Channel string `json:"channel"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, "Invalid request body")
		return
	}
	account, err := h.codeBuddyService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	result := h.codeBuddyService.RunCodeBuddyGrowthChannelNow(c.Request.Context(), account, body.Channel)

	// 审计记通道与分级——**分级入审计**是刻意的：事后追查"谁在什么时候手动跑了
	// 伪造上报类动作"时，这一条是唯一线索。
	middleware.SetAuditAction(c, "admin.codebuddy.growth.run")
	middleware.SetAuditExtra(c, map[string]any{
		"channel":       result.Channel,
		"tier":          result.Tier,
		"auto_runnable": result.AutoRunnable,
	})
	response.Success(c, result)
}

// GrowthRunAll POST /admin/codebuddy/growth/run-all。
//
// 对全部候选账号执行**可自动级别的**成长通道（等价于手动触发一轮排程）。
//
// 通道集合由服务层的分级过滤决定——**full 级不会被带上**，
// 哪怕调用方什么都没传。这是"手动触发一轮自动排程"而不是"跑所有东西"。
func (h *CodeBuddyAdminHandler) GrowthRunAll(c *gin.Context) {
	if h == nil || h.codeBuddyService == nil {
		response.BadRequest(c, "codebuddy admin service is not enabled")
		return
	}
	summary := h.codeBuddyService.RunCodeBuddyGrowthAllNow(c.Request.Context())
	middleware.SetAuditAction(c, "admin.codebuddy.growth.run_all")
	middleware.SetAuditExtra(c, map[string]any{
		"channels":  summary.Channels,
		"attempted": summary.Attempted,
		"succeeded": summary.Succeeded,
		"failed":    summary.Failed,
	})
	response.Success(c, summary)
}

// ActivityRunNow POST /admin/codebuddy/growth/activity-run。
//
// ⚠️ 手动触发**活跃上报**（`full` 级，常规下不自动跑）。
//
// 为什么放在这里而不是让它自动：见 `ProvideCodeBuddyActivityScheduler` 的注释——
// 用户裁定活跃上报属 full 级（伪造对话活跃以过 chat_5 门槛），仅手动。
// 本端点就是那个"手动"。
func (h *CodeBuddyAdminHandler) ActivityRunNow(c *gin.Context) {
	if h == nil || h.codeBuddyService == nil || h.activityScheduler == nil {
		response.BadRequest(c, "codebuddy activity scheduler is not enabled")
		return
	}
	summary := h.activityScheduler.RunActivityNow(c.Request.Context())
	middleware.SetAuditAction(c, "admin.codebuddy.activity.run")
	middleware.SetAuditExtra(c, map[string]any{
		"attempted": summary.Attempted,
		"reported":  summary.Reported,
		"failed":    summary.Failed,
	})
	response.Success(c, summary)
}
