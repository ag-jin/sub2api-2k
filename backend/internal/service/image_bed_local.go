package service

import (
	"io"
	"net/http"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 站点图床的本地磁盘后端（票 #36 扩展）：没有 S3 对象存储的部署用站点自己的磁盘
// 当图床，公开直链由本站匿名读路由 GET /v1/images/bed/:key 提供
// （实测智谱识图工具链只能匿名抓取，所以直链不能要鉴权）。
//
// 写入路径与 S3 完全同构——同一个 ImageResultUploader、同一套 TTL 记账与清理；
// 差别只在装配：resolver 可用时走 S3，不可用且本地开关打开时退化为本地存储。

// ImageBedLocalStorage 是本地磁盘图床后端的能力面：
// 写入/删除满足 ImageStorage / ImageObjectDeleter（TTL 清理依赖 Delete），
// Open 供公开直链路由读取。
// 实现见 repository.LocalImageBedStorage（纯本地文件，无外部依赖）。
type ImageBedLocalStorage interface {
	ImageStorage
	ImageObjectDeleter
	// Open 按公开直链的 key 打开本地对象，返回内容与按扩展名判定的 Content-Type。
	Open(key string) (io.ReadCloser, string, error)
}

// 本地图床读路径的错误契约：由 repository 的本地实现返回（错误词汇只此一套，
// 不为一次文件读取引入第三份错误定义），handler 按 infraerrors 的码映射 400/404/413。
var (
	// ErrImageBedInvalidKey: key 非法（目录穿越、白名单外字符、非图片扩展名）。
	ErrImageBedInvalidKey = infraerrors.New(http.StatusBadRequest, "image_bed_invalid_key", "image key is invalid")
	// ErrImageBedNotFound: 本地对象不存在，或本地后端未启用（直链只服务本地对象，
	// S3 的对象由对象存储自己的域名对外提供）。
	ErrImageBedNotFound = infraerrors.New(http.StatusNotFound, "image_bed_not_found", "image not found")
	// ErrImageBedObjectTooLarge: 本地对象超过公开直链的单文件读取上限（10MiB）。
	ErrImageBedObjectTooLarge = infraerrors.New(http.StatusRequestEntityTooLarge, "image_bed_object_too_large", "image exceeds the size limit")
)

// SetLocalImageBedStorage 注入本地磁盘后端（由 ProvideImageBedService 装配）。
//
// 未注入表示本地兜底未启用：上传回 503（没有任何可用存储），公开直链回 404。
func (s *ImageBedService) SetLocalImageBedStorage(local ImageBedLocalStorage) {
	if s == nil {
		return
	}
	s.local = local
}

// OpenLocal 打开本地磁盘图床的公开直链对象（GET /v1/images/bed/:key）。
//
// 本地后端未启用时返回 ErrImageBedNotFound：对静态资源来说 404 才是正确语义，
// 503 会让抓取端误以为「稍后重试就能拿到」。
func (s *ImageBedService) OpenLocal(key string) (io.ReadCloser, string, error) {
	if s == nil || s.local == nil {
		return nil, "", ErrImageBedNotFound
	}
	return s.local.Open(key)
}
