package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 503 流式断开自动续跑的核心原语：状态、资格谓词、续跑请求体、usage 求和。
//
// 本文件不实现 SSE 帧重写（见 05）也不做编排（见 06/07）。所有状态只在
// Forward 编排 goroutine 写；续跑 attempt 只读 BeginContinuation 冻结的快照。
const (
	openAIStream503AutoContinueMaxContinuations = 2
	openAIStream503AutoContinueMaxElapsed       = 90 * time.Second
	openAIStream503AutoContinueMaxPartialBytes  = 128 << 10
	openAIStream503AutoContinueInstruction      = "Continue the assistant response exactly where it was interrupted. Output only the remaining content; do not repeat anything already written; do not add a preamble."
)

const (
	openAIStream503AutoContinueBackoffFirst  = 300 * time.Millisecond
	openAIStream503AutoContinueBackoffLater  = 900 * time.Millisecond
	openAIStream503AutoContinueBackoffJitter = 0.2
)

// 资格谓词 reason 枚举（用于日志 / ops，稳定字符串）。
const (
	openAIStream503AutoContinueReasonDisabled         = "disabled"
	openAIStream503AutoContinueReasonAccount          = "account"
	openAIStream503AutoContinueReasonIneligibleOutput = "ineligible_output"
	openAIStream503AutoContinueReasonNotCapacity      = "not_capacity"
	openAIStream503AutoContinueReasonClientGone       = "client_gone"
	openAIStream503AutoContinueReasonBudget           = "budget"
	openAIStream503AutoContinueReasonElapsed          = "elapsed"
	openAIStream503AutoContinueReasonPartialTooBig    = "partial_too_large"
)

var (
	// ErrNoInput 表示续跑请求体的 input 缺失或形态无法安全改写。
	ErrNoInput = errors.New("openai stream continuation: input is missing or unsupported")
	// ErrNoPartial 表示没有已下发的部分文本，无法构造续写上下文。
	ErrNoPartial = errors.New("openai stream continuation: partial text is empty")
)

// openAIStreamContinuationSnapshot 是 BeginContinuation 时冻结的只读视图，
// 供 05 重写续跑帧、07 打日志使用。字段一旦冻结不得修改。
type openAIStreamContinuationSnapshot struct {
	responseID      string
	createdAt       int64
	priorUsage      OpenAIUsage
	partialText     string
	openTextItemID  string
	openTextIndex   int
	openTextContent int
	completedItems  []json.RawMessage
	nextOutputIndex int
	lastSequence    int64
	keepOriginalID  bool // v1 恒 true
	// zeroPartial 冻结 BeginContinuation 时的 !hasOpenText：
	// true 表示 attempt-1 未打开任何文本 item，续跑段走拼接器零快照模式。
	zeroPartial bool

	// 观测计数器（裁定 #5）：由 05 通过 state 的 Add* 方法写入，07 读取打日志。
	overlapTrimmedBytes    int
	droppedReasoningEvents int
}

// openAIStreamContinuationState 只在 Forward 编排 goroutine 写。
//
// 可变的观察态（text/openText*/completedItems/...）在 BeginContinuation 时
// 冻结进 snapshot，然后从 snapshot 继承，作为下一次续跑 attempt 的初值。
type openAIStreamContinuationState struct {
	enabled        bool
	accountID      int64
	continuations  int
	firstTriggerAt time.Time
	snapshot       *openAIStreamContinuationSnapshot
	currentBody    []byte
	lastSignal     *openAIStreamContinueSignal

	responseID string
	createdAt  int64
	// text 只保存用于续跑的 partial 文本（上限 MaxPartialBytes），追加是 O(1)。
	// textBytes 独立累计真实字节数，即使文本被截断也继续增长，保证
	// partial_too_large 资格判定按真实大小触发（S1）。
	text              strings.Builder
	textBytes         int
	openTextItemID    string
	openTextIndex     int
	openTextContent   int
	hasOpenText       bool
	inProgressNonText map[string]string // key: item id/索引，value: item type
	completedItems    []json.RawMessage
	nextOutputIndex   int
	lastSequence      int64
	sawRefusal        bool
	sawImageOutput    bool

	// 05 的读 goroutine 与 07 的收尾读取可能并发，使用原子计数器。
	overlapTrimmedBytes    atomic.Int64
	droppedReasoningEvents atomic.Int64
}

// openAIStreamContinueSignal 是 06 从流处理返回、07 捕获的续跑信号。
// 它不是 UpstreamFailoverError：总是携带已解析的本次尝试 usage。
type openAIStreamContinueSignal struct {
	capacityPayload   []byte // 触发降载的原始 data 帧
	message           string // 上游原文（用于 ops/日志/降级下发）
	upstreamRequestID string
	usage             OpenAIUsage // 本次失败尝试已解析的 usage
}

func (e *openAIStreamContinueSignal) Error() string {
	if e == nil {
		return ""
	}
	if msg := strings.TrimSpace(e.message); msg != "" {
		return msg
	}
	return "openai upstream capacity shed"
}

func asOpenAIStreamContinueSignal(err error) (*openAIStreamContinueSignal, bool) {
	var sig *openAIStreamContinueSignal
	if errors.As(err, &sig) {
		return sig, true
	}
	return nil, false
}

// newOpenAIStreamContinuationState 按 design §5 的门禁创建状态。
// v1 只覆盖 oauth/setup-token、非透传、非 compact 的 Responses 流式路径；
// oauth/setup-token 非透传时上游 URL 恒为 chatgptCodexURL（Responses 协议），
// 因此用显式条件替代不存在的 resolveUpstreamProtocolFor（裁定 #2）。
func (s *OpenAIGatewayService) newOpenAIStreamContinuationState(
	account *Account, c *gin.Context, reqStream bool, body []byte,
) *openAIStreamContinuationState {
	if !reqStream {
		return nil
	}
	if account == nil || account.Platform != PlatformOpenAI {
		return nil
	}
	if account.Type != AccountTypeOAuth && account.Type != AccountTypeSetupToken {
		return nil
	}
	if !account.IsOpenAIStream503AutoContinueEnabled() {
		return nil
	}
	if isOpenAIResponsesCompactPath(c) {
		return nil
	}
	if account.IsOpenAIPassthroughEnabled() {
		return nil
	}
	return &openAIStreamContinuationState{
		enabled:           true,
		accountID:         account.ID,
		currentBody:       append([]byte(nil), body...),
		inProgressNonText: make(map[string]string),
	}
}

func (st *openAIStreamContinuationState) InContinuation() bool {
	return st != nil && st.continuations > 0
}

func (st *openAIStreamContinuationState) ObservedPartialText() string {
	if st == nil {
		return ""
	}
	return st.text.String()
}

// appendText 以 O(1) 追加 delta；超过 MaxPartialBytes 后只停止存储，
// 字节计数继续增长（用于 partial_too_large 判定）。
func (st *openAIStreamContinuationState) appendText(delta string) {
	if st == nil || delta == "" {
		return
	}
	st.textBytes += len(delta)
	remaining := openAIStream503AutoContinueMaxPartialBytes - st.text.Len()
	if remaining <= 0 {
		return
	}
	if len(delta) > remaining {
		delta = delta[:remaining]
	}
	_, _ = st.text.WriteString(delta)
}

// resetText 用快照继承的 partial 重置累加器（只在 BeginContinuation 调用）。
func (st *openAIStreamContinuationState) resetText(partial string) {
	st.text.Reset()
	_, _ = st.text.WriteString(partial)
	st.textBytes = len(partial)
}

func (st *openAIStreamContinuationState) Continuations() int {
	if st == nil {
		return 0
	}
	return st.continuations
}

func (st *openAIStreamContinuationState) HasOpenTextItem() bool {
	return st != nil && st.hasOpenText
}

func (st *openAIStreamContinuationState) HasInProgressNonText() bool {
	return st != nil && len(st.inProgressNonText) > 0
}

// HasInProgressNonReasoningItem 只把非 reasoning 的进行中 item 视为阻断。
// 生产目标场景是“长推理期间被容量降载”，进行中的 reasoning 必须放行。
func (st *openAIStreamContinuationState) HasInProgressNonReasoningItem() bool {
	if st == nil {
		return false
	}
	for _, itemType := range st.inProgressNonText {
		if itemType != "reasoning" {
			return true
		}
	}
	return false
}

// HasCompletedNonReasoningItem 扫描已冻结的已完成 items。
// 解析失败的 raw 按“非 reasoning”保守处理（拒绝续跑）。
func (st *openAIStreamContinuationState) HasCompletedNonReasoningItem() bool {
	if st == nil {
		return false
	}
	for _, raw := range st.completedItems {
		if strings.TrimSpace(gjson.GetBytes(raw, "type").String()) != "reasoning" {
			return true
		}
	}
	return false
}

// continuationOutputShapeEligible 是输出形态资格的唯一事实源（design §3 决策 1）。
//   - A（hasOpenText）：v1 partial 形态，逐字保持“任何进行中非文本项都拒绝”。
//   - B（!hasOpenText）：v1.1 零 partial 形态，仅允许 reasoning 作为不可回滚帧。
func (st *openAIStreamContinuationState) continuationOutputShapeEligible() bool {
	if st == nil || st.sawRefusal || st.sawImageOutput {
		return false
	}
	if st.hasOpenText {
		return !st.HasInProgressNonText()
	}
	return !st.HasInProgressNonReasoningItem() && !st.HasCompletedNonReasoningItem()
}

func (st *openAIStreamContinuationState) SawRefusal() bool {
	return st != nil && st.sawRefusal
}

func (st *openAIStreamContinuationState) SawImageOutput() bool {
	return st != nil && st.sawImageOutput
}

// SetCurrentBody 由 07 在构造续跑请求前设置原始上游 body（不含续跑附加项）。
// 必须始终基于原始 body 构造，避免第二次续跑重复追加前一次的 assistant item。
func (st *openAIStreamContinuationState) SetCurrentBody(body []byte) {
	if st == nil {
		return
	}
	st.currentBody = append(st.currentBody[:0], body...)
}

// AddOverlapTrimmedBytes 由 05 在裁剪重叠文本时累加（裁定 #5）。
func (st *openAIStreamContinuationState) AddOverlapTrimmedBytes(n int) {
	if st == nil || n <= 0 {
		return
	}
	st.overlapTrimmedBytes.Add(int64(n))
}

// AddDroppedReasoningEvents 由 05 在丢弃续跑段 reasoning 事件时累加（裁定 #5）。
func (st *openAIStreamContinuationState) AddDroppedReasoningEvents(n int) {
	if st == nil || n <= 0 {
		return
	}
	st.droppedReasoningEvents.Add(int64(n))
}

func (st *openAIStreamContinuationState) OverlapTrimmedBytes() int {
	if st == nil {
		return 0
	}
	return int(st.overlapTrimmedBytes.Load())
}

func (st *openAIStreamContinuationState) DroppedReasoningEvents() int {
	if st == nil {
		return 0
	}
	return int(st.droppedReasoningEvents.Load())
}

// Snapshot 返回最近一次 BeginContinuation 冻结的只读快照（拷贝），
// 计数器镜像为调用时的 live 值，便于 07 在收尾时直接读取。
func (st *openAIStreamContinuationState) Snapshot() *openAIStreamContinuationSnapshot {
	if st == nil || st.snapshot == nil {
		return nil
	}
	snap := *st.snapshot
	snap.completedItems = append([]json.RawMessage(nil), st.snapshot.completedItems...)
	snap.overlapTrimmedBytes = st.OverlapTrimmedBytes()
	snap.droppedReasoningEvents = st.DroppedReasoningEvents()
	return &snap
}

// RecordCapacitySignal 保存最近一次触发续跑的降载信号。
func (st *openAIStreamContinuationState) RecordCapacitySignal(sig *openAIStreamContinueSignal) {
	if st == nil {
		return
	}
	st.lastSignal = sig
}

// CanContinue 只做预算/耗时/文本形态判定（资格谓词的权威门是
// openAIStream503AutoContinueEligible）。now 由调用方注入便于测试。
func (st *openAIStreamContinuationState) CanContinue(now time.Time) (bool, string) {
	if st == nil || !st.enabled {
		return false, openAIStream503AutoContinueReasonDisabled
	}
	if st.continuations >= openAIStream503AutoContinueMaxContinuations {
		return false, openAIStream503AutoContinueReasonBudget
	}
	if !st.firstTriggerAt.IsZero() && now.Sub(st.firstTriggerAt) > openAIStream503AutoContinueMaxElapsed {
		return false, openAIStream503AutoContinueReasonElapsed
	}
	if st.textBytes > openAIStream503AutoContinueMaxPartialBytes {
		return false, openAIStream503AutoContinueReasonPartialTooBig
	}
	if !st.continuationOutputShapeEligible() {
		return false, openAIStream503AutoContinueReasonIneligibleOutput
	}
	return true, ""
}

// BeginContinuation 冻结当前观察态并开始一次续跑 attempt。
// priorUsage 由调用方传入累计值（含本次失败尝试的 usage，裁定 #1）。
func (st *openAIStreamContinuationState) BeginContinuation(priorUsage OpenAIUsage, sig *openAIStreamContinueSignal) {
	if st == nil {
		return
	}
	st.continuations++
	if st.firstTriggerAt.IsZero() {
		st.firstTriggerAt = time.Now()
	}
	st.lastSignal = sig

	snapshot := &openAIStreamContinuationSnapshot{
		responseID:             st.responseID,
		createdAt:              st.createdAt,
		priorUsage:             priorUsage,
		partialText:            st.text.String(),
		openTextItemID:         st.openTextItemID,
		openTextIndex:          st.openTextIndex,
		openTextContent:        st.openTextContent,
		completedItems:         append([]json.RawMessage(nil), st.completedItems...),
		nextOutputIndex:        st.nextOutputIndex,
		lastSequence:           st.lastSequence,
		keepOriginalID:         true,
		zeroPartial:            !st.hasOpenText,
		overlapTrimmedBytes:    st.OverlapTrimmedBytes(),
		droppedReasoningEvents: st.DroppedReasoningEvents(),
	}
	st.snapshot = snapshot

	// 续跑 attempt 从快照继承：文本、坐标、索引、序号。
	st.resetText(snapshot.partialText)
	st.openTextItemID = snapshot.openTextItemID
	st.openTextIndex = snapshot.openTextIndex
	st.openTextContent = snapshot.openTextContent
	st.completedItems = append([]json.RawMessage(nil), snapshot.completedItems...)
	st.nextOutputIndex = snapshot.nextOutputIndex
	st.lastSequence = snapshot.lastSequence
	st.inProgressNonText = make(map[string]string)
	st.sawRefusal = false
	st.sawImageOutput = false
}

// NextRequestBody 在零 partial 时逐字节重放原始上游 body；
// 有 partial 时基于原始 body + partial 构造续写上下文。
// ErrNoPartial 的语义收窄为“nil snapshot 哨兵”与 builder 的空 partial 校验。
func (st *openAIStreamContinuationState) NextRequestBody() ([]byte, error) {
	if st == nil || st.snapshot == nil {
		return nil, ErrNoPartial
	}
	if st.snapshot.partialText == "" {
		// 零 partial：原始 body 的独立拷贝，不原地改写调用方切片。
		return append([]byte(nil), st.currentBody...), nil
	}
	return buildOpenAIStreamContinuationBody(st.currentBody, st.snapshot.partialText)
}

// Backoff 返回本次续跑前的等待时间：第 1 次 300ms，之后 900ms，±20% 抖动。
func (st *openAIStreamContinuationState) Backoff() time.Duration {
	base := openAIStream503AutoContinueBackoffFirst
	if st != nil && st.continuations >= 2 {
		base = openAIStream503AutoContinueBackoffLater
	}
	factor := 1 + openAIStream503AutoContinueBackoffJitter*(2*rand.Float64()-1)
	return time.Duration(float64(base) * factor)
}

// openAIStream503AutoContinueEligible 是触发点的权威资格门（裁定 #7）。
// 判定顺序与 reason 枚举固定，供 06 直接用于日志。
func (s *OpenAIGatewayService) openAIStream503AutoContinueEligible(
	c *gin.Context, account *Account, st *openAIStreamContinuationState,
	data []byte, outputStarted, cyberHit bool,
) (eligible bool, reason string) {
	if st == nil || !st.enabled {
		return false, openAIStream503AutoContinueReasonDisabled
	}
	if account == nil || account.Platform != PlatformOpenAI {
		return false, openAIStream503AutoContinueReasonAccount
	}
	if !outputStarted || cyberHit || !st.continuationOutputShapeEligible() {
		return false, openAIStream503AutoContinueReasonIneligibleOutput
	}
	if !isOpenAIUpstreamCapacityShedEvent(data) {
		return false, openAIStream503AutoContinueReasonNotCapacity
	}
	if c == nil || c.Request == nil || c.Request.Context().Err() != nil {
		return false, openAIStream503AutoContinueReasonClientGone
	}
	return st.CanContinue(time.Now())
}

// observeOpenAIStreamContinuationEvent 在 handler 的 data 帧处理里被调用。
// 它只维护 handler goroutine 私有的观察态；不解析/改写帧本身。
func (s *OpenAIGatewayService) observeOpenAIStreamContinuationEvent(
	st *openAIStreamContinuationState, data []byte, eventType string,
) {
	if st == nil || !st.enabled || len(data) == 0 {
		return
	}
	eventType = effectiveOpenAISSEEventType(data, eventType)
	if eventType == "" {
		return
	}

	switch eventType {
	case "response.created":
		if id := gjson.GetBytes(data, "response.id").String(); id != "" && st.responseID == "" {
			st.responseID = id
		}
		if createdAt := gjson.GetBytes(data, "response.created_at"); createdAt.Exists() && createdAt.Int() != 0 {
			st.createdAt = createdAt.Int()
		}

	case "response.output_item.added":
		item := gjson.GetBytes(data, "item")
		itemType := item.Get("type").String()
		outputIndex := gjson.GetBytes(data, "output_index")
		hasOutputIndex := outputIndex.Exists()
		if !hasOutputIndex {
			outputIndex = item.Get("output_index")
			hasOutputIndex = outputIndex.Exists()
		}
		switch {
		case itemType == "message":
			if st.openTextItemID == "" {
				st.openTextItemID = item.Get("id").String()
				if hasOutputIndex {
					st.openTextIndex = int(outputIndex.Int())
				}
				if contentIndex := item.Get("content.0.index"); contentIndex.Exists() {
					st.openTextContent = int(contentIndex.Int())
				}
			}
			st.hasOpenText = true
			if hasOutputIndex {
				st.bumpNextOutputIndex(int(outputIndex.Int()))
			}
		case isOpenAIStreamContinuationNonTextItemType(itemType):
			key := openAIStreamContinuationItemKey(item.Get("id").String(), itemType, outputIndex)
			st.inProgressNonText[key] = itemType
			if itemType == "image_generation_call" {
				st.sawImageOutput = true
			}
			if hasOutputIndex {
				st.bumpNextOutputIndex(int(outputIndex.Int()))
			}
		}

	case "response.output_item.done":
		item := gjson.GetBytes(data, "item")
		itemType := item.Get("type").String()
		outputIndex := gjson.GetBytes(data, "output_index")
		if itemType == "message" {
			st.hasOpenText = true
		}
		if isOpenAIStreamContinuationNonTextItemType(itemType) {
			key := openAIStreamContinuationItemKey(item.Get("id").String(), itemType, outputIndex)
			delete(st.inProgressNonText, key)
			if raw := strings.TrimSpace(item.Raw); raw != "" {
				st.completedItems = append(st.completedItems, json.RawMessage(append([]byte(nil), raw...)))
			}
			if itemType == "image_generation_call" {
				st.sawImageOutput = true
			}
		}
		if outputIndex.Exists() {
			st.bumpNextOutputIndex(int(outputIndex.Int()))
		}

	case "response.output_text.delta":
		if delta := gjson.GetBytes(data, "delta").String(); delta != "" {
			st.appendText(delta)
			st.hasOpenText = true
			if st.openTextItemID == "" {
				st.openTextItemID = gjson.GetBytes(data, "item_id").String()
				if index := gjson.GetBytes(data, "output_index"); index.Exists() {
					st.openTextIndex = int(index.Int())
					st.bumpNextOutputIndex(int(index.Int()))
				}
				if index := gjson.GetBytes(data, "content_index"); index.Exists() {
					st.openTextContent = int(index.Int())
				}
			}
		}

	case "response.output_text.done":
		st.hasOpenText = true
		if st.openTextItemID == "" {
			st.openTextItemID = gjson.GetBytes(data, "item_id").String()
		}
		if index := gjson.GetBytes(data, "output_index"); index.Exists() {
			st.openTextIndex = int(index.Int())
			st.bumpNextOutputIndex(int(index.Int()))
		}

	default:
		if strings.HasPrefix(eventType, "response.refusal") {
			st.sawRefusal = true
		}
		if strings.HasPrefix(eventType, "response.image_generation_call") {
			st.sawImageOutput = true
		}
	}

	if seq := gjson.GetBytes(data, "sequence_number"); seq.Exists() && seq.Int() > st.lastSequence {
		st.lastSequence = seq.Int()
	}
}

func (st *openAIStreamContinuationState) bumpNextOutputIndex(index int) {
	if index+1 > st.nextOutputIndex {
		st.nextOutputIndex = index + 1
	}
}

func isOpenAIStreamContinuationNonTextItemType(itemType string) bool {
	switch itemType {
	case "function_call", "custom_tool_call", "reasoning", "image_generation_call", "web_search_call":
		return true
	}
	return false
}

func openAIStreamContinuationItemKey(itemID, itemType string, outputIndex gjson.Result) string {
	if itemID != "" {
		return itemID
	}
	if outputIndex.Exists() {
		return fmt.Sprintf("%s:%d", itemType, outputIndex.Int())
	}
	return itemType
}

// buildOpenAIStreamContinuationBody 在原始 body 上局部追加
// assistant(partial) + user(continue instruction)，不触碰其它字段。
func buildOpenAIStreamContinuationBody(body []byte, partial string) ([]byte, error) {
	if strings.TrimSpace(partial) == "" {
		return nil, ErrNoPartial
	}

	assistantItem := map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "output_text", "text": partial},
		},
	}
	continueItem := map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "text": openAIStream503AutoContinueInstruction},
		},
	}

	input := gjson.GetBytes(body, "input")
	switch {
	case input.IsArray():
		assistantRaw, err := marshalOpenAIUpstreamJSON(assistantItem)
		if err != nil {
			return nil, fmt.Errorf("marshal continuation assistant item: %w", err)
		}
		continueRaw, err := marshalOpenAIUpstreamJSON(continueItem)
		if err != nil {
			return nil, fmt.Errorf("marshal continuation instruction item: %w", err)
		}
		out, err := sjson.SetRawBytes(body, "input.-1", assistantRaw)
		if err != nil {
			return nil, fmt.Errorf("append continuation assistant item: %w", err)
		}
		out, err = sjson.SetRawBytes(out, "input.-1", continueRaw)
		if err != nil {
			return nil, fmt.Errorf("append continuation instruction item: %w", err)
		}
		return out, nil

	case input.Type == gjson.String:
		originalItem := map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": input.String()},
			},
		}
		rewritten, err := marshalOpenAIUpstreamJSON([]any{originalItem, assistantItem, continueItem})
		if err != nil {
			return nil, fmt.Errorf("marshal continuation input: %w", err)
		}
		out, err := sjson.SetRawBytes(body, "input", rewritten)
		if err != nil {
			return nil, fmt.Errorf("rewrite continuation input: %w", err)
		}
		return out, nil
	}
	return nil, ErrNoInput
}

func addOpenAIUsage(dst *OpenAIUsage, src OpenAIUsage) {
	if dst == nil {
		return
	}
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CacheCreationInputTokens += src.CacheCreationInputTokens
	dst.CacheReadInputTokens += src.CacheReadInputTokens
	dst.ImageInputTokens += src.ImageInputTokens
	dst.ImageCacheReadTokens += src.ImageCacheReadTokens
	dst.ImageOutputTokens += src.ImageOutputTokens
	dst.UpstreamCredit += src.UpstreamCredit
}
