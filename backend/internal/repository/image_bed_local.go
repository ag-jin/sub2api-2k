package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 本地磁盘图床（票 #36 扩展）：没有 S3 对象存储的部署，站点用自己的磁盘当图床。
// 对象布局 <root>/bed/<不透明文件名>，公开直链 <publicBaseURL>/v1/images/bed/<不透明文件名>
// 由本站匿名读路由提供（handler.ImageBedHandler.Serve）。
const (
	// imageBedLocalDirName 是对象在根目录下的子目录。
	imageBedLocalDirName = "bed"
	// imageBedLocalStorageKeyPrefix 与 service 侧的 imageBedStorageKeyPrefix 同值：
	// ImageStorage 接口传进来的 key 形如 bed/<不透明文件名>。
	imageBedLocalStorageKeyPrefix = "bed/"
	// imageBedLocalRoutePrefix 是公开直链的站点内路径前缀。
	imageBedLocalRoutePrefix = "/v1/images/bed/"
	// imageBedLocalServeMaxBytes 是公开直链单次读取上限（10MiB，与默认上传上限同量级）。
	imageBedLocalServeMaxBytes int64 = 10 << 20
	// imageBedLocalDefaultDataDir 是 DATA_DIR 未设置时的兜底数据目录（与插件目录同口径）。
	imageBedLocalDefaultDataDir = "./data"
)

// imageBedLocalNamePattern 是磁盘对象名白名单：只放行 [a-z0-9_.-] 且不以点开头，
// 长度上界 128（图床 key 生成器只产出 32 位十六进制 + 扩展名）。
// 超长 key 直接判非法，免得落到文件系统才报 ENAMETOOLONG（那时只能回 500）。
var imageBedLocalNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}$`)

// imageBedLocalContentTypes 按扩展名给出 Content-Type；只认上传校验放行的 4 种图片。
var imageBedLocalContentTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// LocalImageBedStorage 是站点图床的本地磁盘后端：实现 service.ImageBedLocalStorage
// （Save/Delete/Open），SDK 无关、无外部依赖，用作 S3 不可用时的兜底。
type LocalImageBedStorage struct {
	root          string
	publicBaseURL string
	// maxServeBytes 限制公开直链的单文件读取上限；默认 10MiB，测试里可调小。
	maxServeBytes int64
}

var _ service.ImageBedLocalStorage = (*LocalImageBedStorage)(nil)

// NewLocalImageBedStorage 构造本地磁盘图床后端。
// root 是图床根目录（对象落在 root/bed/ 下），publicBaseURL 是直链前缀
// （留空则返回站点内相对直链 /v1/images/bed/<key>）。
func NewLocalImageBedStorage(root, publicBaseURL string) *LocalImageBedStorage {
	return &LocalImageBedStorage{
		root:          strings.TrimSpace(root),
		publicBaseURL: strings.TrimRight(strings.TrimSpace(publicBaseURL), "/"),
		maxServeBytes: imageBedLocalServeMaxBytes,
	}
}

// ProvideImageBedLocalStorage 组装本地磁盘图床后端（无 S3 时的兜底）。
//
// 根目录取 gateway.image_bed.local_dir，留空则 <DATA_DIR>/image-bed
// （DATA_DIR 未设置时与插件目录一样退化为 ./data）。
// 始终返回非 nil：是否真正启用由 gateway.image_bed.local_enabled 决定
// （判据在 ProvideImageBedService），目录在首次写入时才创建。
func ProvideImageBedLocalStorage(cfg *config.Config) *LocalImageBedStorage {
	root, baseURL := "", ""
	if cfg != nil {
		root = cfg.Gateway.ImageBed.LocalDir
		baseURL = cfg.Gateway.ImageBed.PublicBaseURL
	}
	if strings.TrimSpace(root) == "" {
		dataDir := strings.TrimSpace(os.Getenv("DATA_DIR"))
		if dataDir == "" {
			dataDir = imageBedLocalDefaultDataDir
		}
		root = filepath.Join(dataDir, "image-bed")
	}
	return NewLocalImageBedStorage(root, baseURL)
}

// Save 把图片字节写到 <root>/bed/<不透明文件名>，返回公开直链。
//
// contentType 不落盘：读路由按扩展名判定 Content-Type（key 生成器保证扩展名
// 与真实图片类型一致，图床上传时已用魔数校验过）。
func (s *LocalImageBedStorage) Save(_ context.Context, key, _ string, data []byte) (string, error) {
	name, err := s.objectName(key)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, imageBedLocalDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("local image bed: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return "", fmt.Errorf("local image bed: write %s: %w", path, err)
	}
	return s.publicBaseURL + imageBedLocalRoutePrefix + name, nil
}

// Delete 删除 key 对应的本地文件。
//
// 文件不存在视为成功：与 S3 DeleteObject 的幂等语义一致，图床 TTL 清理
// 依赖「删不掉才报错、保留记账行下轮重试」，重复清理同一 key 不该失败。
// key 非法则显式报错，绝不静默当成功（否则会删行却把对象留在盘上）。
func (s *LocalImageBedStorage) Delete(_ context.Context, key string) error {
	name, err := s.objectName(key)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.root, imageBedLocalDirName, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("local image bed: remove %s: %w", name, err)
	}
	return nil
}

// Open 按公开直链的 key 打开本地对象（GET /v1/images/bed/:key）。
//
// 错误契约见 service 侧的三个哨兵：key 非法或扩展名不是图片 → 400；
// 文件不存在 → 404；超过单文件读取上限 → 413。
func (s *LocalImageBedStorage) Open(key string) (io.ReadCloser, string, error) {
	name, err := s.objectName(key)
	if err != nil {
		return nil, "", err
	}
	contentType, ok := imageBedLocalContentTypes[filepath.Ext(name)]
	if !ok {
		return nil, "", service.ErrImageBedInvalidKey
	}
	path := filepath.Join(s.root, imageBedLocalDirName, name)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", service.ErrImageBedNotFound
		}
		return nil, "", fmt.Errorf("local image bed: stat %s: %w", path, err)
	}
	// 只服务常规文件：符号链接/目录/设备一律当作不存在，
	// 避免把根目录之外的内容经公开直链暴露出去。
	if !info.Mode().IsRegular() {
		return nil, "", service.ErrImageBedNotFound
	}
	if s.maxServeBytes > 0 && info.Size() > s.maxServeBytes {
		return nil, "", service.ErrImageBedObjectTooLarge
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", service.ErrImageBedNotFound
		}
		return nil, "", fmt.Errorf("local image bed: open %s: %w", path, err)
	}
	return file, contentType, nil
}

// objectName 把调用方的 key 归一化成磁盘对象名。
//
// 两个调用面的 key 形态不同：ImageStorage 接口传存储 key（bed/<不透明文件名>），
// 公开直链传不透明文件名本身；这里统一成 <不透明文件名>，再做一次白名单校验。
// 归一化后仍含路径分隔符（如 bed/sub/x.png）或含点开头（..、.hidden）一律拒绝。
func (s *LocalImageBedStorage) objectName(key string) (string, error) {
	name := strings.TrimPrefix(key, imageBedLocalStorageKeyPrefix)
	if !imageBedLocalNamePattern.MatchString(name) {
		return "", service.ErrImageBedInvalidKey
	}
	return name, nil
}
