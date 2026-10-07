package service

// 智谱视觉桥（票 #35）：盲模型请求含内联图片时，用同一账号的桥模型（默认
// glm-5.3-flash）先行识别，把图片块原地替换为文字描述（「flash 作眼睛」）。
//
// 背景：glm-5.3 主模型对内联 base64 图是盲的（图 token 计费但不识别），而同账号的
// glm-5.3-flash 能看内联图。官方直登体验靠「上传→URL→analyze_image」，二开客户端
// 无登录态、无 URL 且图片会被剥掉，故在站点网关侧补这一层：
//
//	盲模型（glm-5.3/glm-5.2 等）∧ zhipu 账号 ∧ body 含 image 块
//	  → 多图并行调用 flash 识别（每图一次独立请求，关闭思考）→ 图片块按块序原地替换为
//	     "[图片 N 内容] <描述>" → 照常下发
//
// 边界与纪律：
//   - 全链路 fail-open：桥的任一失败（取图、flash 报错、预算耗尽）都只在该图位置留
//     占位文本，绝不阻断原请求，也绝不改变原请求的错误/计费语义；
//   - 触发面收窄：仅 zhipu 账号（anthropic 原生直通路径上的账号上下文，与
//     applyZhipuClientSign 同一判定位置；主流人群是 zcode 登录托管账号，flash 在
//     套餐内）——桥模型必须与请求同账号；非 zhipu 账号一律不桥；
//   - 桥模型与盲模型集互斥：解析配置时把桥模型硬性剔除出盲集，视觉/极速变体
//     （含 flash / 以 v 结尾）同样剔除（宁可漏桥不可误桥），启动时另做显式冲突检查；
//   - 延迟与并发：单图 flash 往返实测 1.5-2.3s（thinking.type=disabled），多图并行、
//     整桥共享一个总预算（默认 20s）；
//   - wire 面最小：桥是既有网关服务的内部步骤（不新增构造参数、不新增注入），
//     flash 调用复用 anthropic 原生直通的请求发送最小单元
//     （sendNativeAnthropicUpstreamRequest），不经 forwardAnthropicViaNativeAnthropicEndpoint
//     整个函数——挂点在后者内部，绕开它即天然无递归。
//
// 配置只读部署层（gateway.zhipu.vision_bridge.*），不做 settingsValues 热更新。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// zhipuVisionBridgeSettings 是解析后的生效配置（默认值已落地，盲集已剔除桥模型）。
type zhipuVisionBridgeSettings struct {
	enabled     bool
	model       string
	blindModels map[string]struct{}
	maxImages   int
	budget      time.Duration
}

// zhipuVisionBridgeSettingsFrom 解析部署层配置：空值/非正值一律落到票面默认值。
// 桥模型是解析结果的一部分，并作为盲模型集的排除项（互斥的唯一执行点）。
func zhipuVisionBridgeSettingsFrom(cfg *config.Config) zhipuVisionBridgeSettings {
	var raw config.GatewayZhipuVisionBridgeConfig
	if cfg != nil {
		raw = cfg.Gateway.Zhipu.VisionBridge
	}

	model := strings.TrimSpace(raw.Model)
	if model == "" {
		model = config.DefaultZhipuVisionBridgeModel
	}
	blindModels := strings.TrimSpace(raw.BlindModels)
	if blindModels == "" {
		blindModels = config.DefaultZhipuVisionBridgeBlindModels
	}
	maxImages := raw.MaxImages
	if maxImages <= 0 {
		maxImages = config.DefaultZhipuVisionBridgeMaxImages
	}
	budget := time.Duration(raw.BudgetSeconds) * time.Second
	if budget <= 0 {
		budget = time.Duration(config.DefaultZhipuVisionBridgeBudgetSeconds) * time.Second
	}

	return zhipuVisionBridgeSettings{
		enabled:     raw.Enabled,
		model:       model,
		blindModels: zhipuVisionBridgeBlindModelSet(blindModels, model),
		maxImages:   maxImages,
		budget:      budget,
	}
}

// zhipuVisionBridgeBlindModelSet 解析逗号分隔的盲模型集（大小写/空白归一化），并剔除两类
// 「不该被桥」的模型：
//
//  1. **桥模型本身**——它出现在盲集里等于让 flash 自己被桥（递归红线），硬性剔除；
//  2. 视觉/极速变体（含 "flash" 或以 "v" 结尾，如 glm-5.3-flash、glm-4.5v）——按票面口径
//     本就不在盲集内；实测 glm-4.5v 在该端点虽收图却完全幻觉，更不能被当成盲模型去桥。
//
// 判定错了宁可漏桥（直通现状=模型看不见图），绝不误桥（自噬，或把能看图的模型描成文字）。
func zhipuVisionBridgeBlindModelSet(raw string, bridgeModel string) map[string]struct{} {
	excluded := strings.ToLower(strings.TrimSpace(bridgeModel))
	set := make(map[string]struct{}, 4)
	for _, item := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(item))
		if name == "" || name == excluded || zhipuVisionBridgeLooksLikeVisionModel(name) {
			continue
		}
		set[name] = struct{}{}
	}
	return set
}

// zhipuVisionBridgeLooksLikeVisionModel 判定模型名是否属于视觉/极速变体（含 "flash" 或
// 以 "v" 结尾）：这类模型本就能看图，不该被桥（票面口径「flash/含 v 结尾的不在集内」）。
func zhipuVisionBridgeLooksLikeVisionModel(model string) bool {
	return strings.Contains(model, "flash") || strings.HasSuffix(model, "v")
}

// isBlindModel 判定上游模型是否属于盲模型集（大小写不敏感，精确匹配；不做前缀/
// 后缀猜测——判定错了宁可漏桥）。
func (s zhipuVisionBridgeSettings) isBlindModel(model string) bool {
	if len(s.blindModels) == 0 {
		return false
	}
	_, ok := s.blindModels[strings.ToLower(strings.TrimSpace(model))]
	return ok
}

// zhipuVisionBridgeConfigConflict 报告「桥模型被误配进盲模型集」的配置冲突：返回
// 冲突的桥模型名与是否冲突。这是启动断言（NewOpenAIGatewayService 内调用）与测试
// 断言的共用判定；冲突时运行期仍安全（settings 解析会剔除），但配置本身必须被喊出来。
func zhipuVisionBridgeConfigConflict(cfg *config.Config) (string, bool) {
	var raw config.GatewayZhipuVisionBridgeConfig
	if cfg != nil {
		raw = cfg.Gateway.Zhipu.VisionBridge
	}
	model := strings.TrimSpace(raw.Model)
	if model == "" {
		model = config.DefaultZhipuVisionBridgeModel
	}
	blindModels := strings.TrimSpace(raw.BlindModels)
	if blindModels == "" {
		blindModels = config.DefaultZhipuVisionBridgeBlindModels
	}
	excluded := strings.ToLower(model)
	for _, item := range strings.Split(blindModels, ",") {
		if strings.ToLower(strings.TrimSpace(item)) == excluded {
			return model, true
		}
	}
	return "", false
}

// 桥的执行面常量。
const (
	// zhipuVisionBridgePrompt 是单图描述提示词：一句话中性事实描述（主会话 2026-10-07
	// 实测口径——一句式指令又快又准；含文字/界面元素的截图场景放宽到 80 字以内）。
	zhipuVisionBridgePrompt = "一句话描述图片的颜色与整体布局。" +
		"若画面含文字或界面元素（如截图），可放宽到 80 字以内的客观描述。" +
		"只输出描述本身，不要寒暄，不要猜测用户意图。"
	// zhipuVisionBridgeDescriptionMaxRunes 是单图描述的字数硬上限（超出即截断；
	// prompt 只要求 80 字以内，这里留的余量对应票面的 ≤120 字口径）。
	zhipuVisionBridgeDescriptionMaxRunes = 120
	// zhipuVisionBridgeHintMaxRunes 是相邻文本 hint 的字数上限（防止把整段长文塞给 flash）。
	zhipuVisionBridgeHintMaxRunes = 200
	// zhipuVisionBridgeMaxTokens 是 flash 识图请求的 max_tokens：描述本身很短，收紧上限
	// 让 flash 一句话答完（实测口径 max_tokens ≤120）。
	zhipuVisionBridgeMaxTokens = 120
	// zhipuVisionBridgeMaxConcurrent 是多图并行的在途上限（实测：每图独立请求可并行），
	// 防止 max_images 被配大时对同一账号打出并发风暴。
	zhipuVisionBridgeMaxConcurrent = 4
	// zhipuVisionBridgeResponseMaxBytes 是 flash 响应体的读取上限（防内存放大）。
	zhipuVisionBridgeResponseMaxBytes = 1 << 20
	// zhipuVisionBridgeImageFetchTimeout 是 url 型图片取回的超时（站点直连）。
	zhipuVisionBridgeImageFetchTimeout = 10 * time.Second
	// 占位文本原因（票面冻结的字样）。
	zhipuVisionBridgeReasonFailed    = "识别失败"
	zhipuVisionBridgeReasonOverLimit = "超出上限未识别"
)

// zhipuVisionBridgeImage 是一个待桥接的图片块：记录它在原 body 中的位置（消息下标 +
// 块下标，用于原地替换）、块序（1-based，用于 "[图片 N ...]" 编号）与图源信息。
type zhipuVisionBridgeImage struct {
	messageIndex int
	blockIndex   int
	index        int
	sourceType   string
	mediaType    string
	data         string
	url          string
	hint         string
}

// blockPath 返回该图片块在 body 里的 sjson 路径（原地替换的落点）。
func (i zhipuVisionBridgeImage) blockPath() string {
	return "messages." + strconv.Itoa(i.messageIndex) + ".content." + strconv.Itoa(i.blockIndex)
}

// zhipuVisionBridgeCollectImages 按块序收集请求里的 image 块。只识别 messages[].content[]
// 顶层的块（客户端贴图走的就是这个位置）；tool_result 内嵌的图片块不在本期范围，
// body 里没有命中的图片块时不桥接（宁可漏桥）。返回空表示无需桥接。
func zhipuVisionBridgeCollectImages(body []byte) []zhipuVisionBridgeImage {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return nil
	}
	var images []zhipuVisionBridgeImage
	messageIndex := 0
	messages.ForEach(func(_, message gjson.Result) bool {
		content := message.Get("content")
		if content.IsArray() {
			blocks := content.Array()
			for blockIndex, block := range blocks {
				if strings.TrimSpace(block.Get("type").String()) != "image" {
					continue
				}
				source := block.Get("source")
				images = append(images, zhipuVisionBridgeImage{
					messageIndex: messageIndex,
					blockIndex:   blockIndex,
					index:        len(images) + 1,
					sourceType:   strings.TrimSpace(source.Get("type").String()),
					mediaType:    strings.TrimSpace(source.Get("media_type").String()),
					data:         strings.TrimSpace(source.Get("data").String()),
					url:          strings.TrimSpace(source.Get("url").String()),
					hint:         zhipuVisionBridgeNearestHint(blocks, blockIndex),
				})
			}
		}
		messageIndex++
		return true
	})
	return images
}

// zhipuVisionBridgeNearestHint 取同一 content 数组里最近的非空 text 块作为 hint
// （先看前一块，再看后一块）：把用户在这张图旁边的提问一并交给 flash，提升相关性。
func zhipuVisionBridgeNearestHint(blocks []gjson.Result, imageIndex int) string {
	if imageIndex > 0 {
		if text := zhipuVisionBridgeTextValue(blocks[imageIndex-1]); text != "" {
			return text
		}
	}
	if imageIndex+1 < len(blocks) {
		return zhipuVisionBridgeTextValue(blocks[imageIndex+1])
	}
	return ""
}

// zhipuVisionBridgeTextValue 读 text 块的文本（非 text 块或空文本返回空串）。
func zhipuVisionBridgeTextValue(block gjson.Result) string {
	if strings.TrimSpace(block.Get("type").String()) != "text" {
		return ""
	}
	return strings.TrimSpace(block.Get("text").String())
}

// zhipuVisionBridgeRewrite 是挂点唯一入口：命中条件时返回替换后的 body 与 true；
// 未命中返回原 body 与 false。函数不返回 error —— 桥对主请求的影响面被限制在
// body 内容上（fail-open：任何失败都只体现为该图位置的占位文本）。
func (s *OpenAIGatewayService) zhipuVisionBridgeRewrite(
	ctx context.Context,
	account *Account,
	body []byte,
	upstreamModel string,
) (out []byte, bridgedAny bool) {
	out, bridgedAny = body, false
	// 最后一道 fail-open 兜底（票面红线「绝不阻断原请求」）：桥处理的是客户端可控 JSON，
	// 任何意外 panic 都只回退成直通现状，绝不让主请求陪葬。
	defer func() {
		if r := recover(); r != nil {
			logger.L().Error("zhipu vision bridge panicked; falling back to the original request body",
				zap.Any("panic", r),
			)
			out, bridgedAny = body, false
		}
	}()

	if s == nil {
		return body, false
	}
	settings := zhipuVisionBridgeSettingsFrom(s.cfg)
	// 触发面（票面挂点口径）：开关开 ∧ zhipu 账号（与 applyZhipuClientSign 同一账号
	// 上下文：anthropic 原生直通路径上的智谱账号；主流人群是 zcode 登录托管账号，
	// flash 在套餐内，但判定按平台收口——非 zhipu 账号一律不桥）∧ 上游模型在盲模型集。
	if !settings.enabled || account == nil || account.Platform != PlatformZhipu || !settings.isBlindModel(upstreamModel) {
		return body, false
	}
	images := zhipuVisionBridgeCollectImages(body)
	if len(images) == 0 {
		return body, false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	started := time.Now()
	apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	targetURL, targetErr := s.nativeAnthropicTargetURL(account)
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	bridgeCtx, cancel := context.WithTimeout(ctx, settings.budget)
	defer cancel()

	// 多图并行（每图一次独立 flash 请求；整桥共享同一预算）：主会话 2026-10-07 实测确认
	// 该端点可并行调用，并行只为压缩多图请求的附加首字延迟（单图 1.5-2.3s）。结果按图片
	// 下标回收，替换仍严格按块序进行（与调用完成顺序无关）。
	texts := make([]string, len(images))
	oks := make([]bool, len(images))
	semaphore := make(chan struct{}, zhipuVisionBridgeMaxConcurrent)
	var waitGroup sync.WaitGroup
	for i := range images {
		waitGroup.Add(1)
		go func(i int) {
			defer waitGroup.Done()
			// 每个 goroutine 自带 recover：并发路径上的 panic 绝不能掀翻主请求。
			defer func() {
				if r := recover(); r != nil {
					logger.L().Error("zhipu vision bridge image goroutine panicked",
						zap.Int64("account_id", zhipuSignAccountID(account)),
						zap.Int("image_index", images[i].index),
						zap.Any("panic", r),
					)
					texts[i] = zhipuVisionBridgePlaceholder(images[i].index, zhipuVisionBridgeReasonFailed)
					oks[i] = false
				}
			}()
			select {
			case semaphore <- struct{}{}:
			case <-bridgeCtx.Done():
				// 预算已耗尽：该图直接按识别失败处理（不占用并发额度）。
				texts[i] = zhipuVisionBridgePlaceholder(images[i].index, zhipuVisionBridgeReasonFailed)
				oks[i] = false
				return
			}
			defer func() { <-semaphore }()
			texts[i], oks[i] = s.zhipuVisionBridgeDescribeImage(bridgeCtx, account, images[i], settings, apiKey, targetURL, targetErr, proxyURL)
		}(i)
	}
	waitGroup.Wait()

	rewritten := body
	bridged, failed := 0, 0
	for i, image := range images {
		if oks[i] {
			bridged++
		} else {
			failed++
		}
		block, err := zhipuVisionBridgeTextBlock(texts[i])
		if err != nil {
			continue
		}
		updated, err := sjson.SetRawBytes(rewritten, image.blockPath(), block)
		if err != nil {
			continue
		}
		rewritten = updated
	}

	// 可观测（票面：最小实现先日志）：images_in 为命中的图片块数，bridged 为真实识别
	// 成功数，failed 为未识别数（含超限与各类失败）。flash 调用本身不计独立 usage 记录
	// （票面最小实现：先计数日志）。
	logger.LegacyPrintf("service.gateway",
		"[智谱视觉桥] account=%d(%s) model=%s bridge_model=%s images_in=%d bridged=%d failed=%d elapsed=%s",
		account.ID, account.Name, upstreamModel, settings.model, len(images), bridged, failed, time.Since(started))
	return rewritten, true
}

// zhipuVisionBridgeDescribeImage 识别单张图片，返回**最终注入文本**（成功为
// "[图片 N 内容] <描述>"，失败/超限为票面占位文本）与是否识别成功。
// 失败一律不外抛（不漏桥、也不阻断原请求）：调用方按占位文本处理。
//
// flash 调用复用 anthropic 原生直通的请求发送最小单元 sendNativeAnthropicUpstreamRequest
// （含签名注入挂点与 VERIFY_* 自愈），**不经** forwardAnthropicViaNativeAnthropicEndpoint
// 整个函数 —— 挂点在后者内部，绕开它就是桥的递归防护之一（另一道是桥模型与盲模型集
// 互斥）。gin.Context 传 nil：桥是站内服务步骤而非客户端转发步骤，不复制客户端请求头，
// 也不写 ops 上游错误事件（桥的失败绝不污染主请求的 ops 记录）。
func (s *OpenAIGatewayService) zhipuVisionBridgeDescribeImage(
	bridgeCtx context.Context,
	account *Account,
	image zhipuVisionBridgeImage,
	settings zhipuVisionBridgeSettings,
	apiKey string,
	targetURL string,
	targetErr error,
	proxyURL string,
) (string, bool) {
	if image.index > settings.maxImages {
		return zhipuVisionBridgePlaceholder(image.index, zhipuVisionBridgeReasonOverLimit), false
	}
	if targetErr != nil {
		s.zhipuVisionBridgeLogImageFailure(account, image, targetErr)
		return zhipuVisionBridgePlaceholder(image.index, zhipuVisionBridgeReasonFailed), false
	}
	if apiKey == "" {
		s.zhipuVisionBridgeLogImageFailure(account, image, errors.New("account has no api key"))
		return zhipuVisionBridgePlaceholder(image.index, zhipuVisionBridgeReasonFailed), false
	}
	data, mediaType, err := s.zhipuVisionBridgeImagePayload(bridgeCtx, image)
	if err != nil {
		s.zhipuVisionBridgeLogImageFailure(account, image, err)
		return zhipuVisionBridgePlaceholder(image.index, zhipuVisionBridgeReasonFailed), false
	}
	flashBody, err := zhipuVisionBridgeFlashBody(settings.model, image, data, mediaType)
	if err != nil {
		s.zhipuVisionBridgeLogImageFailure(account, image, err)
		return zhipuVisionBridgePlaceholder(image.index, zhipuVisionBridgeReasonFailed), false
	}
	resp, err := s.sendNativeAnthropicUpstreamRequest(bridgeCtx, nil, account, flashBody, apiKey, targetURL, proxyURL, false)
	if err != nil {
		s.zhipuVisionBridgeLogImageFailure(account, image, err)
		return zhipuVisionBridgePlaceholder(image.index, zhipuVisionBridgeReasonFailed), false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		s.zhipuVisionBridgeLogImageFailure(account, image, fmt.Errorf("flash upstream status %d", resp.StatusCode))
		return zhipuVisionBridgePlaceholder(image.index, zhipuVisionBridgeReasonFailed), false
	}
	description, err := zhipuVisionBridgeReadDescription(resp.Body)
	if err != nil {
		s.zhipuVisionBridgeLogImageFailure(account, image, err)
		return zhipuVisionBridgePlaceholder(image.index, zhipuVisionBridgeReasonFailed), false
	}
	return fmt.Sprintf("[图片 %d 内容] %s", image.index, description), true
}

// zhipuVisionBridgePlaceholder 是失败/超限图的占位文本（票面冻结字样）。
func zhipuVisionBridgePlaceholder(index int, reason string) string {
	return fmt.Sprintf("[图片 %d：%s]", index, reason)
}

// zhipuVisionBridgeImagePayload 解析出可喂给 flash 的图片载荷（base64 + 媒体类型）：
// base64 型原样透传（绝不重编码），url 型由站点取回后编码。
func (s *OpenAIGatewayService) zhipuVisionBridgeImagePayload(ctx context.Context, image zhipuVisionBridgeImage) (string, string, error) {
	switch image.sourceType {
	case "", "base64":
		if image.data == "" {
			return "", "", errors.New("image block has no base64 data")
		}
		return image.data, zhipuVisionBridgeBase64MediaType(image.mediaType, image.data), nil
	case "url":
		return s.zhipuVisionBridgeFetchImageURL(ctx, image.url)
	default:
		return "", "", fmt.Errorf("unsupported image source type %q", image.sourceType)
	}
}

// zhipuVisionBridgeFetchImageURL 由站点把 url 型图片块取回并编码为 base64 —— 站点在
// 美国，官方工具抓不到的境外图 URL 恰好由这条路径补齐。出站 URL 走与网关其它主动
// 请求同一套 security.url_allowlist 校验（格式/白名单/私网护栏）；取回超时 10s
// （票面），失败按该图识别失败处理（调用方落占位文本，不阻断原请求）。
func (s *OpenAIGatewayService) zhipuVisionBridgeFetchImageURL(ctx context.Context, rawURL string) (string, string, error) {
	if s == nil || s.httpUpstream == nil {
		return "", "", errors.New("no upstream transport configured")
	}
	if strings.TrimSpace(rawURL) == "" {
		return "", "", errors.New("image block has no url")
	}
	validated, err := s.validateOutboundURL(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid image url: %w", err)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, zhipuVisionBridgeImageFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, validated, nil)
	if err != nil {
		return "", "", fmt.Errorf("build image request: %w", err)
	}
	resp, err := s.httpUpstream.Do(req, "", 0, 0)
	if err != nil {
		return "", "", fmt.Errorf("fetch image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", "", fmt.Errorf("fetch image: unexpected status %d", resp.StatusCode)
	}
	limit := defaultImageMaxDownloadBytes
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", "", fmt.Errorf("read image body: %w", err)
	}
	if int64(len(data)) > limit {
		return "", "", fmt.Errorf("fetched image exceeds %d bytes", limit)
	}
	if len(data) == 0 {
		return "", "", errors.New("fetched image is empty")
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if !strings.HasPrefix(mediaType, "image/") {
		mediaType = detectImageContentType(data)
	}
	if !strings.HasPrefix(mediaType, "image/") {
		return "", "", fmt.Errorf("fetched content type %q is not an image", mediaType)
	}
	return base64.StdEncoding.EncodeToString(data), mediaType, nil
}

// zhipuVisionBridgeBase64MediaType 归一化图片媒体类型：声明值合法（image/*）则原样使用；
// 否则对 base64 载荷按字节嗅探，嗅不出来才退回 image/png（客户端漏配 media_type 时不至于
// 直接把图判失败）。
func zhipuVisionBridgeBase64MediaType(declared string, data string) string {
	if mediaType := strings.ToLower(strings.TrimSpace(declared)); strings.HasPrefix(mediaType, "image/") {
		return mediaType
	}
	if decoded, err := base64.StdEncoding.DecodeString(data); err == nil && len(decoded) > 0 {
		if detected := detectImageContentType(decoded); detected != "" {
			return detected
		}
	}
	return "image/png"
}

// zhipuVisionBridgeFlashBody 组装 flash 识图请求：单图 + 描述提示词（含 hint），
// 非流式、**关闭思考**（flash 默认思考模式要 6-7s；关掉后实测 1.5-2.3s 且答案全对），
// 模型名为桥模型（与盲模型集互斥，防自噬）。thinking.type=disabled 是该端点已实测
// 接受的字段（不认也不会报错）。
func zhipuVisionBridgeFlashBody(model string, image zhipuVisionBridgeImage, data string, mediaType string) ([]byte, error) {
	prompt := zhipuVisionBridgePrompt
	if image.hint != "" {
		prompt += "\n\n补充上下文（仅供提高相关性）：" + zhipuVisionBridgeTruncateRunes(image.hint, zhipuVisionBridgeHintMaxRunes)
	}
	payload := map[string]any{
		"model":      model,
		"max_tokens": zhipuVisionBridgeMaxTokens,
		"stream":     false,
		"thinking":   map[string]any{"type": "disabled"},
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": prompt},
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": mediaType,
							"data":       data,
						},
					},
				},
			},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode flash request: %w", err)
	}
	return encoded, nil
}

// zhipuVisionBridgeReadDescription 解析 flash 的 Anthropic Messages 响应，取全部
// text 块拼成描述（thinking 等其它块忽略）；空描述按识别失败处理。
func zhipuVisionBridgeReadDescription(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, zhipuVisionBridgeResponseMaxBytes))
	if err != nil {
		return "", fmt.Errorf("read flash response: %w", err)
	}
	var payload struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("decode flash response: %w", err)
	}
	var builder strings.Builder
	for _, block := range payload.Content {
		if block.Type != "" && block.Type != "text" {
			continue
		}
		text := strings.TrimSpace(block.Text)
		if text == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString(" ")
		}
		builder.WriteString(text)
	}
	description := strings.Join(strings.Fields(builder.String()), " ")
	description = zhipuVisionBridgeTruncateRunes(description, zhipuVisionBridgeDescriptionMaxRunes)
	if description == "" {
		return "", errors.New("flash returned empty description")
	}
	return description, nil
}

// zhipuVisionBridgeTruncateRunes 按字符（rune）截断，避免把多字节字符切成半个。
// 描述 ≤120 字与 hint ≤200 字是票面硬边界：模型自带长度不可信，一律在此兜底。
func zhipuVisionBridgeTruncateRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// zhipuVisionBridgeTextBlock 把注入文本编码成 Anthropic text 块（JSON 转义交给
// encoding/json，描述内容原样写入）。
func zhipuVisionBridgeTextBlock(text string) ([]byte, error) {
	block, err := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "text", Text: text})
	if err != nil {
		return nil, err
	}
	return block, nil
}

// zhipuVisionBridgeLogImageFailure 记录单图识别失败（桥的失败必须可诊断，但不阻断）。
func (s *OpenAIGatewayService) zhipuVisionBridgeLogImageFailure(account *Account, image zhipuVisionBridgeImage, err error) {
	reason := "unknown"
	if err != nil {
		reason = strings.TrimSpace(sanitizeUpstreamErrorMessage(err.Error()))
	}
	logger.L().Warn("zhipu vision bridge image description failed",
		zap.Int64("account_id", zhipuSignAccountID(account)),
		zap.Int("image_index", image.index),
		zap.String("reason", reason),
	)
}

// logZhipuVisionBridgeStartup 是视觉桥的启动断言与接线日志（NewOpenAIGatewayService 内
// 调用一次）：
//
//   - 配置冲突（桥模型出现在盲模型集内）→ Error：运行期解析会硬性剔除桥模型（防
//     「flash 被桥」），故不致命，但配置本身是错的，必须显式喊出来；
//   - 开关开启 → Info：把生效的桥模型/图片上限/预算打进启动日志，便于上线核对
//     （桥只在请求命中时才写业务日志，启动期需要一条明确的「已接线」证据）。
func logZhipuVisionBridgeStartup(cfg *config.Config) {
	if conflict, conflicted := zhipuVisionBridgeConfigConflict(cfg); conflicted {
		logger.L().Error("zhipu vision bridge misconfigured: bridge model is listed in blind models; runtime excludes it (no self-bridging)",
			zap.String("bridge_model", conflict),
		)
	}
	settings := zhipuVisionBridgeSettingsFrom(cfg)
	if !settings.enabled {
		return
	}
	logger.L().Info("zhipu vision bridge enabled",
		zap.String("bridge_model", settings.model),
		zap.Int("blind_model_count", len(settings.blindModels)),
		zap.Int("max_images", settings.maxImages),
		zap.Duration("budget", settings.budget),
	)
}
