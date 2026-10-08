package repository

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 本地磁盘图床（无 S3 时的兜底）：Save 落盘到 <root>/bed/<key>，
// 返回本站匿名读路由的公开直链 <base>/v1/images/bed/<key>。
func TestLocalImageBedStorageSaveWritesFileAndReturnsPublicURL(t *testing.T) {
	root := t.TempDir()
	storage := NewLocalImageBedStorage(root, "https://ai.facaiai.top/")
	payload := []byte("fake-png-payload")

	got, err := storage.Save(context.Background(), "bed/0123456789abcdef.png", "image/png", payload)
	require.NoError(t, err)

	// 直链必须是绝对 URL 且指向本站读路由（智谱识图工具链只能匿名抓取）。
	require.Equal(t, "https://ai.facaiai.top/v1/images/bed/0123456789abcdef.png", got)
	onDisk, err := os.ReadFile(filepath.Join(root, "bed", "0123456789abcdef.png"))
	require.NoError(t, err)
	require.Equal(t, payload, onDisk)
}

// 穿越类/越界 key 一律拒绝（双保险：key 生成器本就只产出 [a-z0-9] 十六进制），
// 且拒绝时连根目录都不该建——绝不能在根目录之外落下任何文件。
func TestLocalImageBedStorageSaveRejectsUnsafeKeys(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "evil.png")
	storage := NewLocalImageBedStorage(root, "https://ai.test")

	for _, key := range []string{
		"",
		"../evil.png",
		"bed/../../evil.png",
		"bed/sub/evil.png",
		"bed/",
		"bed/.hidden.png",
		"bed/UPPERCASE.png",
	} {
		got, err := storage.Save(context.Background(), key, "image/png", []byte("x"))
		require.ErrorIs(t, err, service.ErrImageBedInvalidKey, "key=%q", key)
		require.Empty(t, got)
	}

	require.NoFileExists(t, outside)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// Delete 幂等：存在的删掉、不存在的也算成功（与 S3 DeleteObject 语义一致，
// 图床 TTL 清理依赖「删不掉才报错」）；非法 key 显式报错，不静默当成功。
func TestLocalImageBedStorageDeleteIsIdempotent(t *testing.T) {
	root := t.TempDir()
	storage := NewLocalImageBedStorage(root, "https://ai.test")
	path := filepath.Join(root, "bed", "abc123.png")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o640))

	require.NoError(t, storage.Delete(context.Background(), "bed/abc123.png"))
	require.NoFileExists(t, path)
	require.NoError(t, storage.Delete(context.Background(), "bed/abc123.png"))
	require.NoError(t, storage.Delete(context.Background(), "bed/missing.png"))
	require.ErrorIs(t, storage.Delete(context.Background(), "../evil.png"), service.ErrImageBedInvalidKey)
}

// Open 是公开直链路由的数据来源：读回原字节，并按扩展名给出 Content-Type。
func TestLocalImageBedStorageOpenReturnsBytesAndContentType(t *testing.T) {
	root := t.TempDir()
	storage := NewLocalImageBedStorage(root, "https://ai.test")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bed"), 0o750))
	for name, body := range map[string]string{
		"a.png":  "png-bytes",
		"b.jpg":  "jpg-bytes",
		"c.jpeg": "jpeg-bytes",
		"d.webp": "webp-bytes",
		"e.gif":  "gif-bytes",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, "bed", name), []byte(body), 0o640))
	}

	for _, tc := range []struct{ key, contentType, body string }{
		{"a.png", "image/png", "png-bytes"},
		{"b.jpg", "image/jpeg", "jpg-bytes"},
		{"c.jpeg", "image/jpeg", "jpeg-bytes"},
		{"d.webp", "image/webp", "webp-bytes"},
		{"e.gif", "image/gif", "gif-bytes"},
	} {
		reader, contentType, err := storage.Open(tc.key)
		require.NoError(t, err, "key=%s", tc.key)
		require.Equal(t, tc.contentType, contentType, "key=%s", tc.key)
		body, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		require.Equal(t, tc.body, string(body), "key=%s", tc.key)
	}
}

// Open 的错误契约：不存在 → 404；非法 key / 非图片扩展名 → 400；超过单文件上限 → 413。
func TestLocalImageBedStorageOpenErrorContract(t *testing.T) {
	root := t.TempDir()
	storage := NewLocalImageBedStorage(root, "https://ai.test")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bed"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bed", "a.png"), []byte("png-bytes"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bed", "big.png"), []byte("0123456789"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(root, "bed", "notes.txt"), []byte("x"), 0o640))

	_, _, err := storage.Open("missing.png")
	require.ErrorIs(t, err, service.ErrImageBedNotFound)

	_, _, err = storage.Open("..")
	require.ErrorIs(t, err, service.ErrImageBedInvalidKey)

	_, _, err = storage.Open("notes.txt")
	require.ErrorIs(t, err, service.ErrImageBedInvalidKey)

	storage.maxServeBytes = 4
	_, _, err = storage.Open("big.png")
	require.ErrorIs(t, err, service.ErrImageBedObjectTooLarge)
}

// 只服务常规文件：指向根目录之外的符号链接、目录一律当作不存在，
// 公开直链绝不能变成「读文件系统任意路径」的通道。
func TestLocalImageBedStorageOpenRejectsNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	storage := NewLocalImageBedStorage(root, "https://ai.test")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bed", "dir.png"), 0o750))

	outside := filepath.Join(t.TempDir(), "secret.png")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "bed", "link.png")))

	for _, key := range []string{"dir.png", "link.png"} {
		_, _, err := storage.Open(key)
		require.ErrorIs(t, err, service.ErrImageBedNotFound, "key=%s", key)
	}
}

// 根目录取法：local_dir 优先，留空则 <DATA_DIR>/image-bed（与插件目录同口径）。
func TestProvideImageBedLocalStorageRootResolution(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)

	fromDataDir := ProvideImageBedLocalStorage(&config.Config{})
	_, err := fromDataDir.Save(context.Background(), "bed/aaaaaaaaaaaaaaaa.png", "image/png", []byte("x"))
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(dataDir, "image-bed", "bed", "aaaaaaaaaaaaaaaa.png"))

	explicit := t.TempDir()
	fromConfig := ProvideImageBedLocalStorage(&config.Config{Gateway: config.GatewayConfig{
		ImageBed: config.ImageBedConfig{LocalDir: explicit, PublicBaseURL: "https://www.facaiai.top"},
	}})
	got, err := fromConfig.Save(context.Background(), "bed/bbbbbbbbbbbbbbbb.png", "image/png", []byte("x"))
	require.NoError(t, err)
	require.Equal(t, "https://www.facaiai.top/v1/images/bed/bbbbbbbbbbbbbbbb.png", got)
	require.FileExists(t, filepath.Join(explicit, "bed", "bbbbbbbbbbbbbbbb.png"))
}
