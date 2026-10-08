package handler

import (
	"errors"
	"io"
	"net/http"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// imageBedMultipartOverhead 是 multipart 边界/表单字段允许的额外字节，
// 与单文件上限一起决定请求体硬上限（避免把超大请求体整个读进内存）。
const imageBedMultipartOverhead int64 = 1 << 20

// ImageBedHandler 暴露站点图床的 API key 上传端点：
// POST /v1/images/uploads（网关鉴权组内，与 /v1/images/batches 同级）。
type ImageBedHandler struct {
	service *service.ImageBedService
}

func NewImageBedHandler(imageBed *service.ImageBedService) *ImageBedHandler {
	return &ImageBedHandler{service: imageBed}
}

// Submit 处理 multipart 上传：file 必填（png/jpeg/webp/gif，≤10MiB），
// prompt_hint 可选——票面的 service 签名不含它，因此当前只接受不入账。
// 成功返回 {key, url, expires_at}，其中 url 就是对象存储/CDN 的公开直链。
func (h *ImageBedHandler) Submit(c *gin.Context) {
	if h == nil || h.service == nil {
		imageBedError(c, infraerrors.New(http.StatusServiceUnavailable, "image_bed_unavailable", "image bed is unavailable"))
		return
	}
	apiKey, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil || apiKey.ID <= 0 || apiKey.UserID <= 0 {
		imageBedError(c, infraerrors.New(http.StatusUnauthorized, "API_KEY_REQUIRED", "API key is required"))
		return
	}

	maxBytes := h.service.MaxUploadBytes()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes+imageBedMultipartOverhead)
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			imageBedError(c, service.ErrImageBedFileTooLarge)
			return
		}
		imageBedError(c, service.ErrImageBedMissingFile)
		return
	}
	defer func() { _ = file.Close() }()

	declaredType := ""
	if header != nil {
		declaredType = header.Header.Get("Content-Type")
	}
	// 多读一个字节即可判定超限；上限校验的事实源在 service（后台可配）。
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		imageBedError(c, service.ErrImageBedInvalidUpload)
		return
	}
	got, err := h.service.Upload(c.Request.Context(), apiKey.ID, data, declaredType)
	if err != nil {
		imageBedError(c, err)
		return
	}
	c.JSON(http.StatusOK, got)
}

// Serve 提供本地磁盘图床的公开直链：GET /v1/images/bed/:key（**无鉴权**）。
//
// 这条路由不进网关鉴权组，因为抓取端（智谱识图工具链等）只能匿名 GET 图片字节——
// 实测形态就是「外部工具直接拉本站 URL 拿 PNG 200」。key 是不透明文件名
// （128 位随机十六进制 + 扩展名），不可枚举，因此不开目录列表、不做缓存头。
// S3 的对象不经此处：它们由对象存储自己的域名对外提供。
func (h *ImageBedHandler) Serve(c *gin.Context) {
	if h == nil || h.service == nil {
		imageBedError(c, service.ErrImageBedNotFound)
		return
	}
	reader, contentType, err := h.service.OpenLocal(c.Param("key"))
	if err != nil {
		imageBedError(c, err)
		return
	}
	defer func() { _ = reader.Close() }()

	c.Header("Content-Type", contentType)
	// 直链是 24h TTL 的短链，且内容类型由扩展名决定，禁止嗅探。
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		// 响应头已发出，改不了状态码，只能记日志（不静默丢弃）。
		logger.L().Warn("image_bed.serve_local_failed",
			zap.String("key", c.Param("key")),
			zap.Error(err),
		)
	}
}

// imageBedError 输出网关风格的错误体（与 batch image 的形态一致）。
func imageBedError(c *gin.Context, err error) {
	status := infraerrors.Code(err)
	code := infraerrors.Reason(err)
	message := infraerrors.Message(err)
	if status == 0 || code == "" {
		status = http.StatusInternalServerError
		code = "internal_error"
		message = "internal error"
	}
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    "invalid_request_error",
			"code":    code,
			"message": message,
		},
	})
}
