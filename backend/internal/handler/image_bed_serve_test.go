package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// fakeLocalImageBedFiles 是本地磁盘后端的替身。handler 包不能依赖 repository
// （.golangci.yml 的 depguard 规则），所以这里只复制它的对外形态：
// 直链 <base>/v1/images/bed/<key>，Open 返回字节 + image/png。
// 真实磁盘读写由 repository 的单测和 routes 的公开路由测试覆盖。
type fakeLocalImageBedFiles struct {
	files map[string][]byte
	err   error
}

func newFakeLocalImageBedFiles() *fakeLocalImageBedFiles {
	return &fakeLocalImageBedFiles{files: map[string][]byte{}}
}

func (f *fakeLocalImageBedFiles) Save(_ context.Context, key, _ string, data []byte) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	name := strings.TrimPrefix(key, "bed/")
	f.files[name] = append([]byte(nil), data...)
	return "http://img.test/v1/images/bed/" + name, nil
}

func (f *fakeLocalImageBedFiles) Delete(_ context.Context, key string) error {
	if f.err != nil {
		return f.err
	}
	delete(f.files, strings.TrimPrefix(key, "bed/"))
	return nil
}

func (f *fakeLocalImageBedFiles) Open(key string) (io.ReadCloser, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	data, ok := f.files[key]
	if !ok {
		return nil, "", service.ErrImageBedNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), "image/png", nil
}

// newImageBedLocalRouter 装配真实的 ProvideImageBedService（settings=nil 表示
// 没有对象存储，即无 S3 部署的形态）+ 传入的本地后端。
// 上传端点挂 API key（网关鉴权后的形态），公开直链**不挂任何中间件**。
func newImageBedLocalRouter(t *testing.T, imageBed config.ImageBedConfig, local service.ImageBedLocalStorage) *gin.Engine {
	t.Helper()
	svc := service.ProvideImageBedService(
		&imageBedHandlerRepo{},
		nil,
		imageBedHandlerOwners{},
		&imageBedHandlerCounter{},
		&config.Config{Gateway: config.GatewayConfig{ImageBed: imageBed}},
		local,
	)
	t.Cleanup(svc.Stop)

	imageBedHandler := NewImageBedHandler(svc)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/images/uploads", func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 9, UserID: 7})
		imageBedHandler.Submit(c)
	})
	router.GET("/v1/images/bed/:key", imageBedHandler.Serve)
	return router
}

// 无 S3 时的闭环：上传落到本地后端、返回本站直链，匿名 GET 该直链拿回同样的 PNG 字节。
func TestImageBedServeReturnsUploadedImageAnonymously(t *testing.T) {
	local := newFakeLocalImageBedFiles()
	router := newImageBedLocalRouter(t, config.ImageBedConfig{Enabled: true, LocalEnabled: true}, local)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, imageBedMultipartRequest(t, imageBedHandlerPNG, nil))
	require.Equal(t, http.StatusOK, recorder.Code)

	var uploaded service.ImageBedUploadResult
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &uploaded))
	require.Equal(t, "http://img.test/v1/images/bed/"+uploaded.Key, uploaded.URL)
	require.Equal(t, imageBedHandlerPNG, local.files[uploaded.Key])

	// 抓取端形态：不带任何凭据直接 GET 直链（实测智谱识图工具链只能匿名抓取）。
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, uploaded.URL, nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "image/png", recorder.Header().Get("Content-Type"))
	require.Equal(t, imageBedHandlerPNG, recorder.Body.Bytes())
}

// 本地后端的错误契约映射成 HTTP：不存在 404、key 非法 400、超限 413、其它 500。
func TestImageBedServeMapsLocalStorageErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"not found", service.ErrImageBedNotFound, http.StatusNotFound, "image_bed_not_found"},
		{"invalid key", service.ErrImageBedInvalidKey, http.StatusBadRequest, "image_bed_invalid_key"},
		{"too large", service.ErrImageBedObjectTooLarge, http.StatusRequestEntityTooLarge, "image_bed_object_too_large"},
		{"unexpected failure", errors.New("disk is on fire"), http.StatusInternalServerError, "internal_error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := newFakeLocalImageBedFiles()
			local.err = tc.err
			router := newImageBedLocalRouter(t, config.ImageBedConfig{Enabled: true, LocalEnabled: true}, local)

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/images/bed/0123456789abcdef.png", nil))

			require.Equal(t, tc.wantStatus, recorder.Code)
			require.Contains(t, recorder.Body.String(), tc.wantCode)
		})
	}
}

// 本地兜底未装配（关闭本地开关）时直链回 404：拿不到就该是 404，而不是 503。
func TestImageBedServeReturns404WhenLocalStorageNotConfigured(t *testing.T) {
	router := newImageBedLocalRouter(t, config.ImageBedConfig{Enabled: true, LocalEnabled: false}, nil)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/images/bed/0123456789abcdef.png", nil))

	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Contains(t, recorder.Body.String(), "image_bed_not_found")
}
