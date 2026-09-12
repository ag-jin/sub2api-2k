package admin

import (
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
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
	// audit 审计服务（建号处显式 Record，不赌中间件推导；poll 是 GET，中间件
	// 天然不记录，成功纳管必须显式写 actor/uid/nickname/create|update）。
	audit *service.AuditLogService
}

func NewCodeBuddyAdminHandler(codeBuddyService *service.CodeBuddyAdminService, audit *service.AuditLogService) *CodeBuddyAdminHandler {
	return &CodeBuddyAdminHandler{codeBuddyService: codeBuddyService, audit: audit}
}

// qrStart 解析发起者身份（审计积极响应 actor/uid/nickname/create|update）。
func (h *CodeBuddyAdminHandler) actorID(c *gin.Context) string {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		return ""
	}
	return strconv.FormatInt(subject.UserID, 10)
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

func codebuddyImportResultName(created bool) string {
	if created {
		return "create"
	}
	return "update"
}
