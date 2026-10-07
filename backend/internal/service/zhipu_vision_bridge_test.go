//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 票 #35 站点视觉桥：glm-5.3（盲模型）经站点的请求含内联图片时，站点用同账号的
// glm-5.3-flash 先行识别，把图片块替换为文字描述（「flash 作眼睛」）。
//
// 本文件覆盖桥服务本体（internal/service/zhipu_vision_bridge.go）：盲模型集判定、
// 图像块提取与原地替换、超限/失败/预算耗尽的占位语义、url 型图片取回、以及
// flash 侧请求体的形状（原图 + hint，模型名为桥模型）。挂点（anthropic 原生直通
// 路径的触发矩阵）在 openai_gateway_messages_anthropic_native_test.go。

// ---- 测试夹具 ----

// zhipuVisionBridgeTestImageDataA/B 是 1x1 PNG 的固定测试数据（与 #33 保图测试同款，
// 无任何用户图片内容，遵守 R0）。
const (
	zhipuVisionBridgeTestImageDataA = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	zhipuVisionBridgeTestImageDataB = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="
)

// zhipuVisionBridgeTestImageBlock 构造内联 base64 图片块。
func zhipuVisionBridgeTestImageBlock(data string) string {
	return `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}}`
}

// zhipuVisionBridgeTestURLImageBlock 构造 url 型图片块。
func zhipuVisionBridgeTestURLImageBlock(rawURL string) string {
	return `{"type":"image","source":{"type":"url","url":"` + rawURL + `"}}`
}

// zhipuVisionBridgeTestMessagesBody 用 messages 原文拼出最小 Anthropic Messages 请求体。
func zhipuVisionBridgeTestMessagesBody(messages string) []byte {
	return []byte(`{"model":"glm-5.3","max_tokens":64,"stream":false,"messages":` + messages + `}`)
}

// zhipuVisionBridgeTestAccount 是桥唯一适用的账号形态：智谱登录托管
// （Account.IsZhipuLoginManaged）+ anthropic 原生直通协议（桥模型与请求同账号、同签名通道）。
func zhipuVisionBridgeTestAccount(t *testing.T) *Account {
	t.Helper()
	return zhipuSignTestAccount(t, map[string]any{"api_protocol": APIProtocolAnthropic})
}

// zhipuVisionBridgeSignerStub 是并发安全的签名桩：桥的多图并行会让 Sign 并发进入
// （票 22 的 zhipuSignStubSigner 只服务串行用例，未加锁）。
type zhipuVisionBridgeSignerStub struct {
	mu    sync.Mutex
	calls int
}

func (s *zhipuVisionBridgeSignerStub) Sign(_ context.Context, _, _ string, h http.Header) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	for name, value := range map[string]string{
		"X-Client-Ts":      "stub-ts",
		"X-Client-Version": "stub-version",
		"X-Client-Sig":     "stub-sig",
		"X-Session-Id":     "stub-session",
		"X-Client-Nonce":   "stub-nonce",
		"X-App-Id":         "stub-app",
		"X-Client-Pow":     "stub-pow",
	} {
		h.Set(name, value)
	}
	return nil
}

func (s *zhipuVisionBridgeSignerStub) Invalidate(string) {}

func (s *zhipuVisionBridgeSignerStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// zhipuVisionBridgeTestService 装配桥的执行体：开启视觉桥的配置 + 假上游 + 签名桩
// （签名桩用于断言 flash 调用确实带着签名走出站，且调用次数与 flash 调用数一致）。
func zhipuVisionBridgeTestService(cfg *config.Config, upstream HTTPUpstream) (*OpenAIGatewayService, *zhipuVisionBridgeSignerStub) {
	stub := &zhipuVisionBridgeSignerStub{}
	return &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, zhipuSigner: stub}, stub
}

// zhipuVisionBridgeUpstreamFake 是桥的假上游（HTTPUpstream 接缝）：
//   - GET = url 型图片取回（返回 getBody / getStatus）；
//   - POST 且请求体的 model 等于 mainModel 时返回 mainResponse（模拟主请求，不计入
//     flash 计数）——供挂点用例在同一条转发路径上区分「桥调用」与「主请求」；
//   - 其余 POST = flash 识图：按**图片 base64 数据**索引 descriptions / statuses
//     （多图并行下与出站顺序无关，断言因此保持确定性）。
//
// 传入的父 ctx 已失效即模拟「预算耗尽」；parallelBarrier > 1 时要求同时有该数量的
// flash 调用在途才放行（锁定「多图并行」，串行实现会屏障超时并把该图判失败）。
type zhipuVisionBridgeUpstreamFake struct {
	descriptions        map[string]string
	statuses            map[string]int
	fallbackDescription string

	getBody         []byte
	getStatus       int
	mainModel       string
	mainResponse    *http.Response
	parallelBarrier int
	parallelCh      chan struct{}
	parallelOnce    sync.Once
	parallelWaiting int

	// mu 保护并发写入（多图并行时多个 goroutine 同时进 Do）；用例读取发生在桥返回之后
	// （桥内部 join 过全部 goroutine），故读取无需加锁。
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	// posts 只统计 flash 识图调用，断言「每张图一次、超限/未命中零次」。
	posts int
}

func (f *zhipuVisionBridgeUpstreamFake) record(req *http.Request, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	f.bodies = append(f.bodies, append([]byte(nil), body...))
}

func (f *zhipuVisionBridgeUpstreamFake) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	f.record(req, body)

	if req.Method == http.MethodGet {
		status := f.getStatus
		if status == 0 {
			status = http.StatusOK
		}
		if status >= http.StatusBadRequest {
			return zhipuVisionBridgeTestJSONResponse(status, `{}`), nil
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(f.getBody)),
		}, nil
	}

	if f.mainResponse != nil && f.mainModel != "" && gjson.GetBytes(body, "model").String() == f.mainModel {
		return f.mainResponse, nil
	}

	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.posts++
	f.mu.Unlock()
	if err := f.waitParallelBarrier(); err != nil {
		return nil, err
	}

	imageData := gjson.GetBytes(body, `messages.0.content.#(type=="image").source.data`).String()
	status := http.StatusOK
	if f.statuses != nil && f.statuses[imageData] > 0 {
		status = f.statuses[imageData]
	}
	description := f.fallbackDescription
	if f.descriptions != nil {
		if value, ok := f.descriptions[imageData]; ok {
			description = value
		}
	}
	if status >= http.StatusBadRequest {
		return zhipuVisionBridgeTestJSONResponse(status,
			`{"type":"error","error":{"type":"invalid_request_error","message":"flash boom"}}`), nil
	}
	text, err := json.Marshal(description)
	if err != nil {
		return nil, err
	}
	return zhipuVisionBridgeTestJSONResponse(status,
		`{"id":"msg_flash","type":"message","role":"assistant","content":[{"type":"text","text":`+string(text)+`}]}`), nil
}

// waitParallelBarrier 要求 parallelBarrier 个 flash 调用同时在途才放行；串行实现会超时，
// 并把该图按失败返回（用例据此报错，而不是挂死）。
func (f *zhipuVisionBridgeUpstreamFake) waitParallelBarrier() error {
	if f.parallelBarrier < 2 || f.parallelCh == nil {
		return nil
	}
	f.mu.Lock()
	f.parallelWaiting++
	reached := f.parallelWaiting >= f.parallelBarrier
	f.mu.Unlock()
	if reached {
		f.parallelOnce.Do(func() { close(f.parallelCh) })
	}
	select {
	case <-f.parallelCh:
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("flash calls were served serially (parallel barrier not reached)")
	}
}

func (f *zhipuVisionBridgeUpstreamFake) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return f.Do(req, proxyURL, accountID, accountConcurrency)
}

func zhipuVisionBridgeTestJSONResponse(status int, payload string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
	}
}

// zhipuVisionBridgeFlashImageData 取 flash 请求体里的图片 base64：桥必须把原图原样
// 喂给桥模型（url 型则是取回后的字节编码）。
func zhipuVisionBridgeFlashImageData(body []byte) string {
	return gjson.GetBytes(body, `messages.0.content.#(type=="image").source.data`).String()
}

// zhipuVisionBridgeFlashPrompt 取 flash 请求体里的提示文本（单图描述 prompt + hint）。
func zhipuVisionBridgeFlashPrompt(body []byte) string {
	return gjson.GetBytes(body, `messages.0.content.#(type=="text").text`).String()
}

// flashBodyFor 取「喂了指定图片数据」的那次 flash 请求体（多图并行下与出参顺序无关）。
func (f *zhipuVisionBridgeUpstreamFake) flashBodyFor(t *testing.T, imageData string) []byte {
	t.Helper()
	for _, body := range f.bodies {
		if gjson.GetBytes(body, "model").String() == "" {
			continue
		}
		if zhipuVisionBridgeFlashImageData(body) == imageData {
			return body
		}
	}
	t.Fatalf("no flash request carried image data %q", imageData)
	return nil
}

// zhipuVisionBridgeImageBlockCount 数 body 里剩余的 image 块数量（跨全部 message）。
func zhipuVisionBridgeImageBlockCount(body []byte) int {
	count := 0
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() == "image" {
				count++
			}
			return true
		})
		return true
	})
	return count
}

// zhipuVisionBridgeTestConfig 构造开启视觉桥的网关配置：签名配置沿用票 22 的
// zhipuSignTestConfig（URL allowlist 关闭 + 放行 http 测试域，url 型图片可取回）。
func zhipuVisionBridgeTestConfig() *config.Config {
	cfg := zhipuSignTestConfig(true)
	cfg.Gateway.Zhipu.VisionBridge = config.GatewayZhipuVisionBridgeConfig{
		Enabled:       true,
		Model:         "glm-5.3-flash",
		BlindModels:   "glm-5.3,glm-5.2",
		MaxImages:     4,
		BudgetSeconds: 20,
	}
	return cfg
}

// TestZhipuVisionBridgeSettingsResolution 覆盖部署层配置的解析与默认值（票面：
// gateway.zhipu.vision_bridge.{enabled,blind_models,model,max_images,budget_seconds}）：
// 零值/空串一律落到默认值，盲模型集大小写与空白归一化，**桥模型硬性排除在盲集外**
// （防「flash 被桥」= 互斥断言）。
func TestZhipuVisionBridgeSettingsResolution(t *testing.T) {
	tests := []struct {
		name          string
		bridge        config.GatewayZhipuVisionBridgeConfig
		wantEnabled   bool
		wantModel     string
		wantMaxImages int
		wantBudget    time.Duration
		blindIn       []string
		blindNotIn    []string
	}{
		{
			name:          "零值配置：关闭 + 全部默认值",
			bridge:        config.GatewayZhipuVisionBridgeConfig{},
			wantEnabled:   false,
			wantModel:     config.DefaultZhipuVisionBridgeModel,
			wantMaxImages: config.DefaultZhipuVisionBridgeMaxImages,
			wantBudget:    time.Duration(config.DefaultZhipuVisionBridgeBudgetSeconds) * time.Second,
			blindIn:       []string{"glm-5.3", "glm-5.2"},
			blindNotIn:    []string{config.DefaultZhipuVisionBridgeModel},
		},
		{
			name:          "开启但字段留空：解析为同一套默认值",
			bridge:        config.GatewayZhipuVisionBridgeConfig{Enabled: true},
			wantEnabled:   true,
			wantModel:     config.DefaultZhipuVisionBridgeModel,
			wantMaxImages: config.DefaultZhipuVisionBridgeMaxImages,
			wantBudget:    time.Duration(config.DefaultZhipuVisionBridgeBudgetSeconds) * time.Second,
			blindIn:       []string{"glm-5.3", "glm-5.2"},
			blindNotIn:    []string{config.DefaultZhipuVisionBridgeModel},
		},
		{
			name: "盲模型集大小写/空白归一化且剔除桥模型",
			bridge: config.GatewayZhipuVisionBridgeConfig{
				Enabled:     true,
				Model:       "glm-5.3-flash",
				BlindModels: " GLM-5.3 , glm-5.3-flash , glm-4.7 ",
			},
			wantEnabled:   true,
			wantModel:     "glm-5.3-flash",
			wantMaxImages: config.DefaultZhipuVisionBridgeMaxImages,
			wantBudget:    time.Duration(config.DefaultZhipuVisionBridgeBudgetSeconds) * time.Second,
			blindIn:       []string{"glm-5.3", "glm-4.7", "GLM-5.3"},
			blindNotIn:    []string{"glm-5.3-flash", "GLM-5.3-FLASH"},
		},
		{
			name: "视觉/极速变体（含 flash、v 结尾）一律不进盲集",
			bridge: config.GatewayZhipuVisionBridgeConfig{
				Enabled:     true,
				Model:       "glm-5.3-flash",
				BlindModels: "glm-5.3,glm-4.5v,glm-5.2-flash,GLM-4.5V",
			},
			wantEnabled:   true,
			wantModel:     "glm-5.3-flash",
			wantMaxImages: config.DefaultZhipuVisionBridgeMaxImages,
			wantBudget:    time.Duration(config.DefaultZhipuVisionBridgeBudgetSeconds) * time.Second,
			blindIn:       []string{"glm-5.3"},
			blindNotIn:    []string{"glm-4.5v", "GLM-4.5V", "glm-5.2-flash", "glm-5.3-flash"},
		},
		{
			name: "自定义 max_images / budget_seconds 生效",
			bridge: config.GatewayZhipuVisionBridgeConfig{
				Enabled:       true,
				Model:         "glm-5.3-flash",
				BlindModels:   "glm-5.3",
				MaxImages:     2,
				BudgetSeconds: 5,
			},
			wantEnabled:   true,
			wantModel:     "glm-5.3-flash",
			wantMaxImages: 2,
			wantBudget:    5 * time.Second,
			blindIn:       []string{"glm-5.3"},
			blindNotIn:    []string{"glm-5.3-flash"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.Zhipu.VisionBridge = tt.bridge

			settings := zhipuVisionBridgeSettingsFrom(cfg)
			require.Equal(t, tt.wantEnabled, settings.enabled)
			require.Equal(t, tt.wantModel, settings.model)
			require.Equal(t, tt.wantMaxImages, settings.maxImages)
			require.Equal(t, tt.wantBudget, settings.budget)
			for _, model := range tt.blindIn {
				require.True(t, settings.isBlindModel(model), "%s 应在盲模型集内", model)
			}
			for _, model := range tt.blindNotIn {
				require.False(t, settings.isBlindModel(model), "%s 不得在盲模型集内", model)
			}
		})
	}
}

// TestZhipuVisionBridgeStartupAssertionDefaultsAreDisjoint 是「启动/测试双断言」的
// 测试侧：默认桥模型与默认盲模型集必须互斥（否则默认配置就会把 flash 自己桥掉）。
func TestZhipuVisionBridgeStartupAssertionDefaultsAreDisjoint(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.Zhipu.VisionBridge = config.GatewayZhipuVisionBridgeConfig{
		Enabled:     true,
		Model:       config.DefaultZhipuVisionBridgeModel,
		BlindModels: config.DefaultZhipuVisionBridgeBlindModels,
	}

	conflict, conflicted := zhipuVisionBridgeConfigConflict(cfg)
	require.False(t, conflicted, "默认配置不得存在桥模型 ∩ 盲模型集（冲突项：%s）", conflict)
	require.Empty(t, conflict)
	require.False(t, zhipuVisionBridgeSettingsFrom(cfg).isBlindModel(config.DefaultZhipuVisionBridgeModel))
}

// TestZhipuVisionBridgeStartupAssertionReportsConflict 覆盖启动断言的正例：桥模型
// 被误配进盲模型集时，启动检查必须报出冲突（运行期仍安全：解析时硬性剔除）。
func TestZhipuVisionBridgeStartupAssertionReportsConflict(t *testing.T) {
	tests := []struct {
		name   string
		bridge config.GatewayZhipuVisionBridgeConfig
		want   string
	}{
		{
			name: "显式把桥模型写进盲集",
			bridge: config.GatewayZhipuVisionBridgeConfig{
				Enabled:     true,
				Model:       "glm-5.3-flash",
				BlindModels: "glm-5.3,glm-5.3-flash",
			},
			want: "glm-5.3-flash",
		},
		{
			name: "模型名留空时按默认桥模型判冲突",
			bridge: config.GatewayZhipuVisionBridgeConfig{
				Enabled:     true,
				BlindModels: "GLM-5.3-Flash",
			},
			want: config.DefaultZhipuVisionBridgeModel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.Zhipu.VisionBridge = tt.bridge

			conflict, conflicted := zhipuVisionBridgeConfigConflict(cfg)
			require.True(t, conflicted)
			require.Equal(t, tt.want, conflict)
			// 冲突配置下运行期仍然安全：解析后的盲集不含桥模型。
			require.False(t, zhipuVisionBridgeSettingsFrom(cfg).isBlindModel(conflict))
		})
	}
}

// TestZhipuVisionBridgeRewritesImagesInBlockOrder 覆盖桥的主路径：多图**并行**把原图
// （base64 原样）与相邻 text 块作为 hint 喂给 flash（模型名为桥模型、思考关闭），并把
// 图片块**原地**替换为文字块 "[图片 N 内容] <描述>"，其余块与块序保持不变。
func TestZhipuVisionBridgeRewritesImagesInBlockOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := zhipuVisionBridgeTestMessagesBody(
		`[{"role":"user","content":[{"type":"text","text":"这张图里是什么"},` +
			zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataA) + `]},` +
			`{"role":"user","content":[` + zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataB) +
			`,{"type":"text","text":"第二张呢"}]}]`)
	// 并行屏障：两张图的 flash 调用必须同时在途（串行实现会屏障超时、该用例失败）。
	upstream := &zhipuVisionBridgeUpstreamFake{
		descriptions: map[string]string{
			zhipuVisionBridgeTestImageDataA: "一只红色的圆球",
			zhipuVisionBridgeTestImageDataB: "蓝天与白云",
		},
		parallelBarrier: 2,
		parallelCh:      make(chan struct{}),
	}
	svc, signer := zhipuVisionBridgeTestService(zhipuVisionBridgeTestConfig(), upstream)

	rewritten, changed := svc.zhipuVisionBridgeRewrite(
		context.Background(), zhipuVisionBridgeTestAccount(t), body, "glm-5.3")

	require.True(t, changed)
	require.Zero(t, zhipuVisionBridgeImageBlockCount(rewritten), "替换后不得残留 image 块")
	// 替换按块序、与 flash 完成顺序无关：text 在前、描述占原位（两张图各自的原位）。
	require.JSONEq(t,
		`[{"type":"text","text":"这张图里是什么"},{"type":"text","text":"[图片 1 内容] 一只红色的圆球"}]`,
		gjson.GetBytes(rewritten, "messages.0.content").Raw)
	require.JSONEq(t,
		`[{"type":"text","text":"[图片 2 内容] 蓝天与白云"},{"type":"text","text":"第二张呢"}]`,
		gjson.GetBytes(rewritten, "messages.1.content").Raw)

	require.Equal(t, 2, upstream.posts, "每张图恰好一次 flash 调用")
	require.Equal(t, 2, signer.callCount(), "flash 调用必须走既有签名挂点")
	flashA := upstream.flashBodyFor(t, zhipuVisionBridgeTestImageDataA)
	require.Equal(t, "http://upstream.example/v1/messages", upstream.requests[0].URL.String())
	require.Equal(t, "glm-5.3-flash", gjson.GetBytes(flashA, "model").String())
	require.Equal(t, "disabled", gjson.GetBytes(flashA, "thinking.type").String(),
		"flash 必须关闭思考（实测：默认思考 6-7s，关闭后 1.5-2.3s）")
	require.Equal(t, zhipuVisionBridgeMaxTokens, int(gjson.GetBytes(flashA, "max_tokens").Int()))
	require.Equal(t, zhipuVisionBridgeTestImageDataA, zhipuVisionBridgeFlashImageData(flashA),
		"flash 必须收到原图 base64（原样，不重编码）")
	require.Contains(t, zhipuVisionBridgeFlashPrompt(flashA), "这张图里是什么",
		"相邻 text 块必须作为 hint 传入")
	require.NotEmpty(t, upstream.requests[0].Header.Get("X-Client-Sig"),
		"flash 请求必须带签名头出站")
	require.False(t, gjson.GetBytes(flashA, "stream").Bool(), "flash 调用为非流式")
	flashB := upstream.flashBodyFor(t, zhipuVisionBridgeTestImageDataB)
	require.Contains(t, zhipuVisionBridgeFlashPrompt(flashB), "第二张呢")
}

// TestZhipuVisionBridgeLeavesBodiesWithoutImagesUntouched 覆盖快速路径：没有 image 块的
// 请求体逐字节不变、零 flash 调用（桥绝不为了「顺手」而重建 body）。
func TestZhipuVisionBridgeLeavesBodiesWithoutImagesUntouched(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := zhipuVisionBridgeTestMessagesBody(`[{"role":"user","content":[{"type":"text","text":"你好"}]}]`)
	upstream := &zhipuVisionBridgeUpstreamFake{fallbackDescription: "不应被调用"}
	svc, _ := zhipuVisionBridgeTestService(zhipuVisionBridgeTestConfig(), upstream)

	rewritten, changed := svc.zhipuVisionBridgeRewrite(
		context.Background(), zhipuVisionBridgeTestAccount(t), body, "glm-5.3")

	require.False(t, changed)
	require.Equal(t, body, rewritten, "无图请求必须逐字节原样")
	require.Zero(t, upstream.posts)
}

// TestZhipuVisionBridgeOverLimitImagesGetPlaceholder 覆盖 max_images 上限：超出的图块
// 不调 flash，只留占位文本 "[图片 N：超出上限未识别]"，且不影响上限内的图。
func TestZhipuVisionBridgeOverLimitImagesGetPlaceholder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := zhipuVisionBridgeTestConfig()
	cfg.Gateway.Zhipu.VisionBridge.MaxImages = 1
	body := zhipuVisionBridgeTestMessagesBody(`[{"role":"user","content":[` +
		zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataA) + `,` +
		zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataB) + `]}]`)
	upstream := &zhipuVisionBridgeUpstreamFake{
		descriptions: map[string]string{zhipuVisionBridgeTestImageDataA: "一只红色的圆球"},
	}
	svc, _ := zhipuVisionBridgeTestService(cfg, upstream)

	rewritten, changed := svc.zhipuVisionBridgeRewrite(
		context.Background(), zhipuVisionBridgeTestAccount(t), body, "glm-5.3")

	require.True(t, changed)
	require.Equal(t, 1, upstream.posts, "超限图片不得触发 flash 调用")
	require.JSONEq(t,
		`[{"type":"text","text":"[图片 1 内容] 一只红色的圆球"},{"type":"text","text":"[图片 2：超出上限未识别]"}]`,
		gjson.GetBytes(rewritten, "messages.0.content").Raw)
	require.Zero(t, zhipuVisionBridgeImageBlockCount(rewritten))
}

// TestZhipuVisionBridgeImageFailuresDoNotBlockRequest 覆盖单图失败隔离：任一图识别失败
// （上游 4xx/5xx、空描述、图源不受支持）只在该图位置留 "[图片 N：识别失败]"，
// 其余图照常桥接，函数仍返回替换后的 body（绝不阻断原请求）。
func TestZhipuVisionBridgeImageFailuresDoNotBlockRequest(t *testing.T) {
	tests := []struct {
		name         string
		imageA       string
		imageB       string
		descriptions map[string]string
		statuses     map[string]int
		wantA        string
		wantB        string
		wantPosts    int
	}{
		{
			name:   "第二张图 flash 500 不影响第一张",
			imageA: zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataA),
			imageB: zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataB),
			descriptions: map[string]string{
				zhipuVisionBridgeTestImageDataA: "一只红色的圆球",
				zhipuVisionBridgeTestImageDataB: "蓝天与白云",
			},
			statuses:  map[string]int{zhipuVisionBridgeTestImageDataB: http.StatusInternalServerError},
			wantA:     "[图片 1 内容] 一只红色的圆球",
			wantB:     "[图片 2：识别失败]",
			wantPosts: 2,
		},
		{
			name:   "空描述按识别失败处理",
			imageA: zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataA),
			imageB: zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataB),
			descriptions: map[string]string{
				zhipuVisionBridgeTestImageDataA: "",
				zhipuVisionBridgeTestImageDataB: "",
			},
			wantA:     "[图片 1：识别失败]",
			wantB:     "[图片 2：识别失败]",
			wantPosts: 2,
		},
		{
			name:         "不受支持的图源（file/source 缺失）不调 flash",
			imageA:       `{"type":"image","source":{"type":"file","file_id":"file_1"}}`,
			imageB:       `{"type":"image","source":{"type":"base64","media_type":"image/png"}}`,
			descriptions: map[string]string{zhipuVisionBridgeTestImageDataA: "不应被调用"},
			wantA:        "[图片 1：识别失败]",
			wantB:        "[图片 2：识别失败]",
			wantPosts:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			body := zhipuVisionBridgeTestMessagesBody(
				`[{"role":"user","content":[` + tt.imageA + `,` + tt.imageB + `]}]`)
			upstream := &zhipuVisionBridgeUpstreamFake{
				descriptions: tt.descriptions,
				statuses:     tt.statuses,
			}
			svc, _ := zhipuVisionBridgeTestService(zhipuVisionBridgeTestConfig(), upstream)

			rewritten, changed := svc.zhipuVisionBridgeRewrite(
				context.Background(), zhipuVisionBridgeTestAccount(t), body, "glm-5.3")

			require.True(t, changed, "失败路径同样必须完成替换（占位）而不是放弃整桥")
			require.Equal(t, tt.wantPosts, upstream.posts)
			textA, _ := json.Marshal(tt.wantA)
			textB, _ := json.Marshal(tt.wantB)
			require.JSONEq(t,
				`[{"type":"text","text":`+string(textA)+`},{"type":"text","text":`+string(textB)+`}]`,
				gjson.GetBytes(rewritten, "messages.0.content").Raw)
		})
	}
}

// TestZhipuVisionBridgeTruncatesLongDescriptionAndHint 覆盖长度兜底：描述截断到 120 字、
// hint 截断到 200 字（flash 提示词按票面「≤120 字/图」的硬边界执行，不信任模型自带长度）。
func TestZhipuVisionBridgeTruncatesLongDescriptionAndHint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	longDescription := strings.Repeat("字", zhipuVisionBridgeDescriptionMaxRunes) + "尾巴不该出现"
	longHint := strings.Repeat("问", zhipuVisionBridgeHintMaxRunes) + "尾巴不该出现"
	body := zhipuVisionBridgeTestMessagesBody(`[{"role":"user","content":[{"type":"text","text":"` + longHint + `"},` +
		zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataA) + `]}]`)
	upstream := &zhipuVisionBridgeUpstreamFake{
		descriptions: map[string]string{zhipuVisionBridgeTestImageDataA: longDescription},
	}
	svc, _ := zhipuVisionBridgeTestService(zhipuVisionBridgeTestConfig(), upstream)

	rewritten, changed := svc.zhipuVisionBridgeRewrite(
		context.Background(), zhipuVisionBridgeTestAccount(t), body, "glm-5.3")

	require.True(t, changed)
	want := "[图片 1 内容] " + strings.Repeat("字", zhipuVisionBridgeDescriptionMaxRunes)
	injected := gjson.GetBytes(rewritten, "messages.0.content.1.text").String()
	require.Equal(t, want, injected)
	require.Len(t, []rune(injected), len([]rune("[图片 1 内容] "))+zhipuVisionBridgeDescriptionMaxRunes)

	prompt := zhipuVisionBridgeFlashPrompt(upstream.flashBodyFor(t, zhipuVisionBridgeTestImageDataA))
	require.Contains(t, prompt, strings.Repeat("问", zhipuVisionBridgeHintMaxRunes))
	require.NotContains(t, prompt, "尾巴不该出现", "hint 也必须截断，不得把整段长文塞给 flash")
}

// TestZhipuVisionBridgeSniffsMissingMediaType 覆盖客户端漏配 media_type 的情形：站点按
// 字节嗅探补上媒体类型，仍能把图喂给 flash（而不是直接判该图失败）。
func TestZhipuVisionBridgeSniffsMissingMediaType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := zhipuVisionBridgeTestMessagesBody(`[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"base64","data":"` + zhipuVisionBridgeTestImageDataA + `"}}]}]`)
	upstream := &zhipuVisionBridgeUpstreamFake{
		descriptions: map[string]string{zhipuVisionBridgeTestImageDataA: "一只红色的圆球"},
	}
	svc, _ := zhipuVisionBridgeTestService(zhipuVisionBridgeTestConfig(), upstream)

	rewritten, changed := svc.zhipuVisionBridgeRewrite(
		context.Background(), zhipuVisionBridgeTestAccount(t), body, "glm-5.3")

	require.True(t, changed)
	require.Equal(t, 1, upstream.posts)
	require.Equal(t, "image/png",
		gjson.GetBytes(upstream.flashBodyFor(t, zhipuVisionBridgeTestImageDataA),
			`messages.0.content.#(type=="image").source.media_type`).String())
	require.Equal(t, "[图片 1 内容] 一只红色的圆球",
		gjson.GetBytes(rewritten, "messages.0.content.0.text").String())
}

// TestZhipuVisionBridgeFetchesURLImages 覆盖 url 型图片块：站点侧取回字节、编码成 base64
// 后喂给 flash（补官方工具抓不到的境外抓图盲区）；取回失败或 URL 不合法时该图占位，
// 不发 flash 调用，也不阻断原请求。
func TestZhipuVisionBridgeFetchesURLImages(t *testing.T) {
	rawPNG, err := base64.StdEncoding.DecodeString(zhipuVisionBridgeTestImageDataA)
	require.NoError(t, err)

	tests := []struct {
		name      string
		rawURL    string
		getStatus int
		wantText  string
		wantPosts int
		wantFetch bool
	}{
		{
			name:      "取回成功 → base64 喂 flash",
			rawURL:    "http://image.example/a.png",
			wantText:  "[图片 1 内容] 蓝天与白云",
			wantPosts: 1,
			wantFetch: true,
		},
		{
			name:      "取回 404 → 占位不阻断",
			rawURL:    "http://image.example/missing.png",
			getStatus: http.StatusNotFound,
			wantText:  "[图片 1：识别失败]",
			wantPosts: 0,
			wantFetch: true,
		},
		{
			name:      "非法 URL（非 http/https）→ 不发取回请求",
			rawURL:    "ftp://image.example/a.png",
			wantText:  "[图片 1：识别失败]",
			wantPosts: 0,
			wantFetch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			body := zhipuVisionBridgeTestMessagesBody(`[{"role":"user","content":[` +
				zhipuVisionBridgeTestURLImageBlock(tt.rawURL) + `]}]`)
			upstream := &zhipuVisionBridgeUpstreamFake{
				descriptions: map[string]string{zhipuVisionBridgeTestImageDataA: "蓝天与白云"},
				getBody:      rawPNG,
				getStatus:    tt.getStatus,
			}
			svc, _ := zhipuVisionBridgeTestService(zhipuVisionBridgeTestConfig(), upstream)

			rewritten, changed := svc.zhipuVisionBridgeRewrite(
				context.Background(), zhipuVisionBridgeTestAccount(t), body, "glm-5.3")

			require.True(t, changed)
			require.Equal(t, tt.wantText, gjson.GetBytes(rewritten, "messages.0.content.0.text").String())
			require.Equal(t, tt.wantPosts, upstream.posts)
			if !tt.wantFetch {
				require.Empty(t, upstream.requests, "URL 不合法时不得发出取回请求")
				return
			}
			require.Equal(t, http.MethodGet, upstream.requests[0].Method)
			require.Equal(t, tt.rawURL, upstream.requests[0].URL.String())
			_, hasDeadline := upstream.requests[0].Context().Deadline()
			require.True(t, hasDeadline, "url 取回必须带超时（票面 10s）")
			if tt.wantPosts == 0 {
				return
			}
			// 取回的字节必须**编码成 base64** 喂给 flash（flashBodyFor 按图片数据定位该请求）。
			flashBody := upstream.flashBodyFor(t, zhipuVisionBridgeTestImageDataA)
			require.Equal(t, "image/png",
				gjson.GetBytes(flashBody, `messages.0.content.#(type=="image").source.media_type`).String())
		})
	}
}

// TestZhipuVisionBridgeFailsOpenWhenBudgetAlreadyExpired 覆盖预算耗尽（超时）语义：
// 桥的整桥预算是 WithTimeout(父 ctx, budget)，父 ctx 已失效时每张图都按识别失败落占位，
// 但请求照常完成替换（绝不阻断原请求、绝不外抛错误）。
//
// 超时与取消在站点侧是同一条路径（ctx 失效 → 该图失败），这里用显式取消确定性覆盖，
// 不依赖墙钟。
func TestZhipuVisionBridgeFailsOpenWhenBudgetAlreadyExpired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := zhipuVisionBridgeTestMessagesBody(`[{"role":"user","content":[` +
		zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataA) + `,` +
		zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataB) + `]}]`)
	upstream := &zhipuVisionBridgeUpstreamFake{fallbackDescription: "不应被调用"}
	svc, _ := zhipuVisionBridgeTestService(zhipuVisionBridgeTestConfig(), upstream)

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	rewritten, changed := svc.zhipuVisionBridgeRewrite(expired, zhipuVisionBridgeTestAccount(t), body, "glm-5.3")

	require.True(t, changed)
	require.Equal(t, 0, upstream.posts, "ctx 已失效时不得有 flash 调用真正出站")
	// 出站尝试次数不作断言：并行分发 + 父 ctx 已取消时，前置检查/出站竞态下
	// 0/1/2 次尝试都是合法 fail-open 路径（真实 HTTP 客户端会立即失败）。
	require.LessOrEqual(t, len(upstream.requests), 2)
	require.JSONEq(t,
		`[{"type":"text","text":"[图片 1：识别失败]"},{"type":"text","text":"[图片 2：识别失败]"}]`,
		gjson.GetBytes(rewritten, "messages.0.content").Raw)
}

// TestZhipuVisionBridgeBudgetBoundsFlashRequest 断言 budget_seconds 真的作用在出站请求上：
// flash 请求的 ctx deadline 就是桥的整桥预算（默认 20s）——多图并行共享同一个预算窗口，
// 超时后未完成的图按识别失败落占位（见上一个用例）。
func TestZhipuVisionBridgeBudgetBoundsFlashRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := zhipuVisionBridgeTestMessagesBody(`[{"role":"user","content":[` +
		zhipuVisionBridgeTestImageBlock(zhipuVisionBridgeTestImageDataA) + `]}]`)
	upstream := &zhipuVisionBridgeUpstreamFake{
		descriptions: map[string]string{zhipuVisionBridgeTestImageDataA: "一只红色的圆球"},
	}
	svc, _ := zhipuVisionBridgeTestService(zhipuVisionBridgeTestConfig(), upstream)

	started := time.Now()
	_, changed := svc.zhipuVisionBridgeRewrite(
		context.Background(), zhipuVisionBridgeTestAccount(t), body, "glm-5.3")

	require.True(t, changed)
	require.Equal(t, 1, upstream.posts)
	deadline, ok := upstream.requests[0].Context().Deadline()
	require.True(t, ok, "flash 请求必须带整桥预算的 deadline")
	require.WithinDuration(t, started.Add(20*time.Second), deadline, 2*time.Second)
}
