package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

var imageBedHandlerPNG = []byte("\x89PNG\r\n\x1a\nfake-png-payload")

type imageBedHandlerRepo struct {
	inserted []*service.ImageBedUploadRecord
}

func (r *imageBedHandlerRepo) InsertImageBedUpload(_ context.Context, upload *service.ImageBedUploadRecord) error {
	r.inserted = append(r.inserted, upload)
	return nil
}

func (r *imageBedHandlerRepo) ListExpiredImageBedUploads(context.Context, time.Time, int) ([]*service.ImageBedUploadRecord, error) {
	return nil, nil
}

func (r *imageBedHandlerRepo) DeleteImageBedUpload(context.Context, int64) error { return nil }

type imageBedHandlerOwners struct{}

func (imageBedHandlerOwners) ResolveImageBedOwner(context.Context, int64) (int64, error) {
	return 7, nil
}

type imageBedHandlerStorage struct {
	saved []string
}

func (s *imageBedHandlerStorage) Save(_ context.Context, key, _ string, _ []byte) (string, error) {
	s.saved = append(s.saved, key)
	return "https://cdn.test/" + key, nil
}

type imageBedHandlerCounter struct {
	value int64
}

func (c *imageBedHandlerCounter) IncrImageBedQuota(context.Context, int64, time.Duration) (int64, error) {
	c.value++
	return c.value, nil
}

// newImageBedHandlerRouter 用真实 service + 假存储装配上传端点。
// withAPIKey 为假时模拟「未通过网关鉴权」（上下文里没有 API key）。
func newImageBedHandlerRouter(
	imageBed config.ImageBedConfig,
	storageReady bool,
	counter service.ImageBedQuotaCounter,
	withAPIKey bool,
) (*gin.Engine, *imageBedHandlerStorage) {
	storage := &imageBedHandlerStorage{}
	resolver := service.ImageStorageResolver(func() (*service.ImageResultUploader, bool) {
		if !storageReady {
			return nil, false
		}
		return service.NewImageResultUploader(storage, "images/", 0, nil), true
	})
	svc := service.NewImageBedService(&imageBedHandlerRepo{}, resolver, imageBedHandlerOwners{}, counter, &config.Config{
		Gateway: config.GatewayConfig{ImageBed: imageBed},
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	if withAPIKey {
		router.Use(func(c *gin.Context) {
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 9, UserID: 7})
			c.Next()
		})
	}
	router.POST("/v1/images/uploads", NewImageBedHandler(svc).Submit)
	return router, storage
}

// imageBedMultipartRequest 构造一次 multipart 上传（部件 Content-Type 默认
// application/octet-stream，与 curl/Python 的常见行为一致）。
func imageBedMultipartRequest(t *testing.T, data []byte, extraFields map[string]string) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "cat.png")
	require.NoError(t, err)
	_, err = part.Write(data)
	require.NoError(t, err)
	for name, value := range extraFields {
		field, err := writer.CreateFormField(name)
		require.NoError(t, err)
		_, err = field.Write([]byte(value))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	req := httptest.NewRequest(http.MethodPost, "/v1/images/uploads", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestImageBedHandlerSubmitMultipartUpload(t *testing.T) {
	router, storage := newImageBedHandlerRouter(config.ImageBedConfig{Enabled: true}, true, &imageBedHandlerCounter{}, true)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, imageBedMultipartRequest(t, imageBedHandlerPNG, map[string]string{"prompt_hint": "a cat"}))

	require.Equal(t, http.StatusOK, recorder.Code)
	var got service.ImageBedUploadResult
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
	require.NotEmpty(t, got.Key)
	require.Equal(t, "https://cdn.test/bed/"+got.Key, got.URL)
	require.False(t, got.ExpiresAt.IsZero())
	require.Equal(t, []string{"bed/" + got.Key}, storage.saved)
}

func TestImageBedHandlerSubmitRequiresAPIKey(t *testing.T) {
	router, storage := newImageBedHandlerRouter(config.ImageBedConfig{Enabled: true}, true, &imageBedHandlerCounter{}, false)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, imageBedMultipartRequest(t, imageBedHandlerPNG, nil))

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.Contains(t, recorder.Body.String(), "API_KEY_REQUIRED")
	require.Empty(t, storage.saved)
}

func TestImageBedHandlerSubmitMissingFile(t *testing.T) {
	router, storage := newImageBedHandlerRouter(config.ImageBedConfig{Enabled: true}, true, &imageBedHandlerCounter{}, true)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	field, err := writer.CreateFormField("prompt_hint")
	require.NoError(t, err)
	_, err = field.Write([]byte("a cat"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	req := httptest.NewRequest(http.MethodPost, "/v1/images/uploads", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "image_bed_missing_file")
	require.Empty(t, storage.saved)
}

func TestImageBedHandlerSubmitRejectsNonImage(t *testing.T) {
	router, storage := newImageBedHandlerRouter(config.ImageBedConfig{Enabled: true}, true, &imageBedHandlerCounter{}, true)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, imageBedMultipartRequest(t, []byte("just text, not an image"), nil))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "image_bed_unsupported_type")
	require.Empty(t, storage.saved)
}

func TestImageBedHandlerSubmitRejectsOversizeFile(t *testing.T) {
	router, storage := newImageBedHandlerRouter(
		config.ImageBedConfig{Enabled: true, MaxBytes: int64(len(imageBedHandlerPNG))},
		true,
		&imageBedHandlerCounter{},
		true,
	)
	oversize := append(append([]byte(nil), imageBedHandlerPNG...), 'x')

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, imageBedMultipartRequest(t, oversize, nil))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "image_bed_file_too_large")
	require.Empty(t, storage.saved)
}

// 存储未配置（无 S3）时必须给出明确 503，而不是 500 或静默兜底到本地盘。
func TestImageBedHandlerSubmitWithoutStorageReturns503(t *testing.T) {
	router, storage := newImageBedHandlerRouter(config.ImageBedConfig{Enabled: true}, false, &imageBedHandlerCounter{}, true)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, imageBedMultipartRequest(t, imageBedHandlerPNG, nil))

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), "image_bed_storage_unavailable")
	require.Empty(t, storage.saved)
}

func TestImageBedHandlerSubmitQuotaExceededReturns429(t *testing.T) {
	counter := &imageBedHandlerCounter{value: 1} // 本小时已用 1 张
	router, storage := newImageBedHandlerRouter(config.ImageBedConfig{Enabled: true, HourlyLimitPerKey: 1}, true, counter, true)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, imageBedMultipartRequest(t, imageBedHandlerPNG, nil))

	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Contains(t, recorder.Body.String(), "image_bed_quota_exceeded")
	require.Empty(t, storage.saved)
}

func TestImageBedHandlerSubmitDisabledReturns404(t *testing.T) {
	router, storage := newImageBedHandlerRouter(config.ImageBedConfig{Enabled: false}, true, &imageBedHandlerCounter{}, true)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, imageBedMultipartRequest(t, imageBedHandlerPNG, nil))

	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Contains(t, recorder.Body.String(), "image_bed_disabled")
	require.Empty(t, storage.saved)
}
