package service

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// openAIStreamContinuationBody 把续跑 attempt 的上游 Responses SSE 流逐帧
// 重写成“同一个 response 的连续事件流”，让下游 handler 无感拼接。
//
// 结构照 openai_gateway_grok_tool_protocol.go 的 io.Pipe 范式：后台 goroutine
// 读 source，重写后写入 pipe；下游 handler 从 PipeReader 读。source 读错误
// 通过 CloseWithError 原样传给下游，保持“续跑段失败可被触发点再次识别”的语义。
type openAIStreamContinuationBody struct {
	*io.PipeReader
	closeSource func() error
}

func (b *openAIStreamContinuationBody) Close() error {
	if b == nil {
		return nil
	}
	readerErr := b.PipeReader.Close()
	sourceErr := b.closeSource()
	if readerErr != nil {
		return readerErr
	}
	return sourceErr
}

// newOpenAIStreamContinuationBody 包装续跑 attempt 的上游流。
//
// 前三个参数是 05 卡约定的公开契约。可选的 state 用于按裁定 #5 上报
// overlap_trimmed_bytes / dropped_reasoning_events（07 应传入 continuation
// state；不传时重写行为完全一致，只是不累计观测计数）。
func newOpenAIStreamContinuationBody(
	source io.ReadCloser,
	snapshot *openAIStreamContinuationSnapshot,
	maxLineSize int,
	state ...*openAIStreamContinuationState,
) io.ReadCloser {
	snap := snapshot
	if snap == nil {
		snap = &openAIStreamContinuationSnapshot{}
	}
	var counter *openAIStreamContinuationState
	if len(state) > 0 {
		counter = state[0]
	}

	reader, writer := io.Pipe()
	var closeOnce sync.Once
	var closeErr error
	closeSource := func() error {
		closeOnce.Do(func() {
			if source != nil {
				closeErr = source.Close()
			}
		})
		return closeErr
	}

	body := &openAIStreamContinuationBody{PipeReader: reader, closeSource: closeSource}
	go transformOpenAIStreamContinuation(source, writer, snap, counter, maxLineSize, closeSource)
	return body
}

// openAIStreamContinuationOverlapWindow 是重叠去重缓存的字节上限（design §3 决策 3）。
const openAIStreamContinuationOverlapWindow = 512

type openAIStreamPendingDelta struct {
	eventLine string
	hasEvent  bool
	payload   []byte
	delta     string
}

// openAIStreamRewriteFailure 记录本流里第一次 sjson 改写失败的位置与原始错误。
// 失败时帧保持原样下发（降级），收尾时只打一条 WARN（S3）。
type openAIStreamRewriteFailure struct {
	frameType string
	path      string
	err       error
}

// openAIStreamContinuationReplayer 只在 body 包装器的读 goroutine 使用。
// snapshot 是只读的；runningText 持有“客户端最终看到的完整文本”（含第 1 次
// 的 partial），因此终态合并时不需要再跨 goroutine 读取。
type openAIStreamContinuationReplayer struct {
	snapshot *openAIStreamContinuationSnapshot
	counter  *openAIStreamContinuationState
	buffered *bufio.Writer

	runningText strings.Builder

	seenTextMessage     bool
	mappedItemID        string
	mappedUpstreamIndex int
	itemMap             map[string]int
	nextOutputIndex     int
	nextSequence        int64

	pendingDeltas []openAIStreamPendingDelta
	pendingBytes  int
	overlapDone   bool

	mergedItem        json.RawMessage
	newCompletedItems []json.RawMessage
	sawDone           bool

	// frameType 是当前正在重写的帧类型（供失败日志定位）。
	frameType      string
	rewriteFailure *openAIStreamRewriteFailure
}

func newOpenAIStreamContinuationReplayer(
	snapshot *openAIStreamContinuationSnapshot,
	counter *openAIStreamContinuationState,
	buffered *bufio.Writer,
) *openAIStreamContinuationReplayer {
	replayer := &openAIStreamContinuationReplayer{
		snapshot:        snapshot,
		counter:         counter,
		buffered:        buffered,
		itemMap:         make(map[string]int),
		nextOutputIndex: snapshot.nextOutputIndex,
		nextSequence:    snapshot.lastSequence,
		// partial 为空时不存在可裁剪的重复前缀，直接放行以保住 TTFT。
		overlapDone: snapshot.partialText == "",
	}
	_, _ = replayer.runningText.WriteString(snapshot.partialText)
	return replayer
}

func transformOpenAIStreamContinuation(
	source io.ReadCloser,
	destination *io.PipeWriter,
	snapshot *openAIStreamContinuationSnapshot,
	counter *openAIStreamContinuationState,
	maxLineSize int,
	closeSource func() error,
) {
	defer func() { _ = closeSource() }()
	if maxLineSize <= 0 {
		maxLineSize = defaultMaxLineSize
	}

	scanner := bufio.NewScanner(source)
	scanBuf := getSSEScannerBuf64K()
	defer putSSEScannerBuf64K(scanBuf)
	scanner.Buffer(scanBuf[:0], maxLineSize)
	documents := newOpenAISSEJSONDocumentScanner(scanner)

	buffered := bufio.NewWriterSize(destination, 4*1024)
	replayer := newOpenAIStreamContinuationReplayer(snapshot, counter, buffered)
	defer replayer.logRewriteFailure()

	writeLine := func(line string) error {
		if _, err := buffered.WriteString(line); err != nil {
			return err
		}
		return buffered.WriteByte('\n')
	}
	fail := func(err error) {
		_ = buffered.Flush()
		_ = destination.CloseWithError(err)
	}

	pendingEvent := ""
	pendingEventType := ""
	hasEvent := false

	for documents.Scan() {
		line := documents.Text()
		if line == "" {
			continue // 帧由 replayer 自行以空行收尾；被丢弃的帧不产生空行。
		}
		if strings.HasPrefix(line, ":") {
			// SSE 注释原样转发。
			if err := writeLine(line); err != nil {
				fail(err)
				return
			}
			if err := buffered.Flush(); err != nil {
				fail(err)
				return
			}
			continue
		}
		if eventType, ok := extractOpenAISSEEventLine(line); ok {
			pendingEvent = line
			pendingEventType = eventType
			hasEvent = true
			continue
		}
		if data, ok := extractOpenAISSEDataLine(line); ok {
			if err := replayer.processDataLine(pendingEvent, hasEvent, pendingEventType, []byte(data)); err != nil {
				fail(err)
				return
			}
			pendingEvent = ""
			pendingEventType = ""
			hasEvent = false
			continue
		}

		// 未知 SSE 字段（例如 id:/retry:）原样保留。
		if hasEvent {
			if err := writeLine(pendingEvent); err != nil {
				fail(err)
				return
			}
			pendingEvent = ""
			pendingEventType = ""
			hasEvent = false
		}
		if err := writeLine(line); err != nil {
			fail(err)
			return
		}
	}

	if err := replayer.flushOverlap(); err != nil {
		fail(err)
		return
	}
	if err := buffered.Flush(); err != nil {
		fail(err)
		return
	}
	if err := documents.Err(); err != nil {
		_ = destination.CloseWithError(err)
		return
	}
	_ = destination.Close()
}

func (r *openAIStreamContinuationReplayer) processDataLine(
	eventLine string, hasEvent bool, upstreamEventType string, data []byte,
) error {
	if string(data) == "[DONE]" {
		if err := r.flushOverlap(); err != nil {
			return err
		}
		if r.sawDone {
			return nil
		}
		r.sawDone = true
		return r.writeFrame(eventLine, hasEvent, data)
	}
	if !gjson.ValidBytes(data) {
		if err := r.flushOverlap(); err != nil {
			return err
		}
		return r.writeFrame(eventLine, hasEvent, data)
	}

	eventType := effectiveOpenAISSEEventType(data, upstreamEventType)
	r.frameType = eventType

	// 被映射 message 的文本 delta 需要先缓存做重叠裁剪；其余事件先 flush，
	// 保证下游看到的事件顺序与上游一致。
	if eventType == "response.output_text.delta" && r.isMappedDelta(data) {
		return r.handleMappedDelta(eventLine, hasEvent, data)
	}
	if err := r.flushOverlap(); err != nil {
		return err
	}

	if strings.HasPrefix(eventType, "response.reasoning") {
		r.countDroppedReasoning()
		return nil
	}
	if strings.HasPrefix(eventType, "response.refusal") {
		// refusal 不并入文本，也不展示给客户端。
		return nil
	}

	switch eventType {
	case "response.created", "response.in_progress", "response.queued":
		return nil

	case "response.output_item.added":
		item := gjson.GetBytes(data, "item")
		itemType := item.Get("type").String()
		itemID := item.Get("id").String()
		if itemType == "reasoning" {
			r.countDroppedReasoning()
			return nil
		}
		if itemType == "message" && !r.seenTextMessage {
			r.seenTextMessage = true
			r.mappedItemID = itemID
			if index := gjson.GetBytes(data, "output_index"); index.Exists() {
				r.mappedUpstreamIndex = int(index.Int())
			}
			if itemID != "" {
				r.itemMap[itemID] = r.snapshot.openTextIndex
			}
			return nil // 客户端已经有这个 message item。
		}
		index := r.allocateOutputIndex()
		if itemID != "" {
			r.itemMap[itemID] = index
		}
		return r.deliverJSON(eventLine, hasEvent, r.setJSON(data, "output_index", index))

	case "response.output_item.done":
		item := gjson.GetBytes(data, "item")
		itemType := item.Get("type").String()
		itemID := item.Get("id").String()
		if itemType == "reasoning" {
			r.countDroppedReasoning()
			return nil
		}
		if itemType == "message" && r.isMappedMessageItem(itemID, data) {
			if !r.seenTextMessage {
				r.seenTextMessage = true
				r.mappedItemID = itemID
			}
			merged := r.runningText.String()
			rewritten := r.setJSON(data, "output_index", r.snapshot.openTextIndex)
			if r.snapshot.openTextItemID != "" {
				rewritten = r.setJSON(rewritten, "item.id", r.snapshot.openTextItemID)
			}
			rewritten = r.rewriteMessageContent(rewritten, "item.content", merged)
			if raw := gjson.GetBytes(rewritten, "item").Raw; raw != "" {
				r.mergedItem = json.RawMessage(append([]byte(nil), raw...))
			}
			return r.deliverJSON(eventLine, hasEvent, rewritten)
		}
		index, ok := r.outputIndexForItem(itemID)
		if !ok {
			index = r.allocateOutputIndex()
			if itemID != "" {
				r.itemMap[itemID] = index
			}
		}
		rewritten := r.setJSON(data, "output_index", index)
		if raw := gjson.GetBytes(rewritten, "item").Raw; raw != "" {
			r.newCompletedItems = append(r.newCompletedItems, json.RawMessage(append([]byte(nil), raw...)))
		}
		return r.deliverJSON(eventLine, hasEvent, rewritten)

	case "response.content_part.added", "response.content_part.done":
		if gjson.GetBytes(data, "part.type").String() == "refusal" {
			return nil
		}
		if r.isMappedMessageItem(gjson.GetBytes(data, "item_id").String(), data) {
			return nil
		}
		return r.deliverJSON(eventLine, hasEvent, r.rewriteNewItemIndex(data))

	case "response.output_text.delta":
		return r.deliverJSON(eventLine, hasEvent, r.rewriteNewItemIndex(data))

	case "response.output_text.done":
		if r.isMappedMessageItem(gjson.GetBytes(data, "item_id").String(), data) {
			rewritten := r.setJSON(data, "output_index", r.snapshot.openTextIndex)
			rewritten = r.setJSON(rewritten, "content_index", r.snapshot.openTextContent)
			if r.snapshot.openTextItemID != "" {
				rewritten = r.setJSON(rewritten, "item_id", r.snapshot.openTextItemID)
			}
			rewritten = r.setJSON(rewritten, "text", r.runningText.String())
			return r.deliverJSON(eventLine, hasEvent, rewritten)
		}
		return r.deliverJSON(eventLine, hasEvent, r.rewriteNewItemIndex(data))

	case "response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return r.deliverJSON(eventLine, hasEvent, r.rewriteNewItemIndex(data))

	case "error":
		return r.deliverJSON(eventLine, hasEvent, data)

	case "response.failed":
		rewritten := data
		if r.snapshot.responseID != "" && gjson.GetBytes(rewritten, "response.id").Exists() {
			rewritten = r.setJSON(rewritten, "response.id", r.snapshot.responseID)
		}
		return r.deliverJSON(eventLine, hasEvent, rewritten)

	case "response.completed", "response.done", "response.incomplete", "response.cancelled":
		return r.deliverJSON(eventLine, hasEvent, r.rewriteTerminal(data))
	}

	return r.deliverJSON(eventLine, hasEvent, r.rewriteNewItemIndex(data))
}

func (r *openAIStreamContinuationReplayer) isMappedDelta(data []byte) bool {
	itemID := gjson.GetBytes(data, "item_id").String()
	if !r.seenTextMessage {
		// 续跑段的第一个文本 delta：视为第 1 次已打开 message 的延续。
		r.seenTextMessage = true
		r.mappedItemID = itemID
		if index := gjson.GetBytes(data, "output_index"); index.Exists() {
			r.mappedUpstreamIndex = int(index.Int())
		}
		if itemID != "" {
			r.itemMap[itemID] = r.snapshot.openTextIndex
		}
		return true
	}
	if itemID != "" && r.mappedItemID != "" {
		return itemID == r.mappedItemID
	}
	if index := gjson.GetBytes(data, "output_index"); index.Exists() {
		return int(index.Int()) == r.mappedUpstreamIndex
	}
	return r.mappedItemID == ""
}

func (r *openAIStreamContinuationReplayer) isMappedMessageItem(itemID string, data []byte) bool {
	if itemID != "" && r.mappedItemID != "" {
		return itemID == r.mappedItemID
	}
	if index := gjson.GetBytes(data, "output_index"); index.Exists() && r.seenTextMessage {
		return int(index.Int()) == r.mappedUpstreamIndex
	}
	return !r.seenTextMessage
}

func (r *openAIStreamContinuationReplayer) handleMappedDelta(eventLine string, hasEvent bool, data []byte) error {
	delta := gjson.GetBytes(data, "delta").String()
	if delta == "" {
		return nil
	}
	if r.overlapDone {
		rewritten := r.rewriteMappedDeltaCoordinates(data, delta)
		_, _ = r.runningText.WriteString(delta)
		return r.deliverJSON(eventLine, hasEvent, rewritten)
	}
	r.pendingDeltas = append(r.pendingDeltas, openAIStreamPendingDelta{
		eventLine: eventLine,
		hasEvent:  hasEvent,
		payload:   append([]byte(nil), data...),
		delta:     delta,
	})
	r.pendingBytes += len(delta)
	if r.pendingBytes < openAIStreamContinuationOverlapWindow {
		return nil
	}
	return r.flushOverlap()
}

// flushOverlap 找到“第 1 次 partial 后缀”与“续跑前缀”的最长精确重叠并裁掉。
// 裁剪按原始 delta 边界回放，保证每个非空 delta 仍是独立帧。
func (r *openAIStreamContinuationReplayer) flushOverlap() error {
	if r.overlapDone {
		return nil
	}
	pending := r.pendingDeltas
	if len(pending) == 0 {
		// 首个 delta 之前的非 delta 事件（如 output_item.added）也会走到这里；
		// 没有可裁内容时不得消耗重叠判定窗口，等首个带内容的 flush 再决策。
		return nil
	}
	r.overlapDone = true
	r.pendingDeltas = nil
	r.pendingBytes = 0

	var combined strings.Builder
	for _, item := range pending {
		_, _ = combined.WriteString(item.delta)
	}
	prefix := combined.String()
	if len(prefix) > openAIStreamContinuationOverlapWindow {
		// 窗口截断也必须落在 rune 起始字节上，否则后续按 trimmed 回放 delta
		// 会从多字节 rune 中间切开（S2）。
		cut := openAIStreamContinuationOverlapWindow
		for cut > 0 && !utf8.RuneStart(prefix[cut]) {
			cut--
		}
		prefix = prefix[:cut]
	}
	trimmed := openAIStreamContinuationOverlap(r.snapshot.partialText, prefix)
	if trimmed > 0 && r.counter != nil {
		r.counter.AddOverlapTrimmedBytes(trimmed)
	}

	skip := trimmed
	for _, item := range pending {
		if skip >= len(item.delta) {
			skip -= len(item.delta)
			continue
		}
		delta := item.delta
		if skip > 0 {
			delta = delta[skip:]
			skip = 0
		}
		if delta == "" {
			continue
		}
		_, _ = r.runningText.WriteString(delta)
		if err := r.deliverJSON(item.eventLine, item.hasEvent, r.rewriteMappedDeltaCoordinates(item.payload, delta)); err != nil {
			return err
		}
	}
	return nil
}

func (r *openAIStreamContinuationReplayer) rewriteMappedDeltaCoordinates(data []byte, delta string) []byte {
	rewritten := r.setJSON(data, "delta", delta)
	if r.snapshot.openTextItemID != "" {
		rewritten = r.setJSON(rewritten, "item_id", r.snapshot.openTextItemID)
	}
	rewritten = r.setJSON(rewritten, "output_index", r.snapshot.openTextIndex)
	rewritten = r.setJSON(rewritten, "content_index", r.snapshot.openTextContent)
	return rewritten
}

func (r *openAIStreamContinuationReplayer) rewriteTerminal(data []byte) []byte {
	rewritten := data
	if r.snapshot.responseID != "" {
		rewritten = r.setJSON(rewritten, "response.id", r.snapshot.responseID)
	}
	if r.snapshot.createdAt != 0 {
		rewritten = r.setJSON(rewritten, "response.created_at", r.snapshot.createdAt)
	}
	rewritten = r.rewriteTerminalUsage(rewritten)

	if items, err := json.Marshal(r.terminalItems(data, r.runningText.String())); err == nil {
		rewritten = r.setRawJSON(rewritten, "response.output", items)
	}
	return rewritten
}

// terminalItems 按 05 卡的固定顺序合并：
// 第 1 次已完成 items + 合并后的文本 message + 续跑新增的已完成 items。
func (r *openAIStreamContinuationReplayer) terminalItems(payload []byte, merged string) []json.RawMessage {
	items := make([]json.RawMessage, 0, len(r.snapshot.completedItems)+1+len(r.newCompletedItems))
	items = append(items, r.snapshot.completedItems...)
	items = append(items, r.mergedTextItem(merged))
	if len(r.newCompletedItems) > 0 {
		items = append(items, r.newCompletedItems...)
		return items
	}
	items = append(items, r.terminalFallbackItems(payload)...)
	return items
}

// terminalFallbackItems 覆盖“续跑段没有单独 output_item.done、终态才带 output”
// 的上游形态：取终态 output 中除 reasoning 和被映射 message 之外的 items。
func (r *openAIStreamContinuationReplayer) terminalFallbackItems(payload []byte) []json.RawMessage {
	output := gjson.GetBytes(payload, "response.output")
	if !output.IsArray() {
		return nil
	}
	items := make([]json.RawMessage, 0, len(output.Array()))
	skippedMappedMessage := false
	for _, item := range output.Array() {
		itemType := item.Get("type").String()
		if itemType == "reasoning" {
			continue
		}
		if itemType == "message" && !skippedMappedMessage {
			skippedMappedMessage = true
			continue
		}
		if item.Raw == "" {
			continue
		}
		items = append(items, json.RawMessage(append([]byte(nil), item.Raw...)))
	}
	return items
}

func (r *openAIStreamContinuationReplayer) mergedTextItem(merged string) json.RawMessage {
	if len(r.mergedItem) > 0 {
		return json.RawMessage(r.rewriteMessageContent(r.mergedItem, "content", merged))
	}
	itemID := r.snapshot.openTextItemID
	if itemID == "" {
		itemID = r.mappedItemID
	}
	item := map[string]any{
		"id":     itemID,
		"type":   "message",
		"status": "completed",
		"role":   "assistant",
		"content": []any{
			map[string]any{
				"type":        "output_text",
				"text":        merged,
				"annotations": []any{},
			},
		},
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return nil
	}
	return json.RawMessage(raw)
}

func (r *openAIStreamContinuationReplayer) rewriteTerminalUsage(payload []byte) []byte {
	usage := gjson.GetBytes(payload, "response.usage")
	if !usage.IsObject() {
		if r.snapshot.priorUsage != (OpenAIUsage{}) {
			if raw, err := json.Marshal(r.snapshot.priorUsage); err == nil {
				return r.setRawJSON(payload, "response.usage", raw)
			}
		}
		return payload
	}
	continuationUsage, ok := openAIUsageFromGJSON(usage)
	if !ok {
		return payload
	}
	merged := r.snapshot.priorUsage
	addOpenAIUsage(&merged, continuationUsage)

	rewritten := r.setJSON(payload, "response.usage.input_tokens", merged.InputTokens)
	rewritten = r.setJSON(rewritten, "response.usage.output_tokens", merged.OutputTokens)
	rewritten = r.setOptionalUsage(rewritten, usage, "cache_read_input_tokens", merged.CacheReadInputTokens)
	rewritten = r.setOptionalUsage(rewritten, usage, "cache_creation_input_tokens", merged.CacheCreationInputTokens)
	rewritten = r.setOptionalUsage(rewritten, usage, "image_input_tokens", merged.ImageInputTokens)
	rewritten = r.setOptionalUsage(rewritten, usage, "image_cache_read_tokens", merged.ImageCacheReadTokens)
	rewritten = r.setOptionalUsage(rewritten, usage, "image_output_tokens", merged.ImageOutputTokens)
	if merged.UpstreamCredit != 0 || usage.Get("upstream_credit").Exists() {
		rewritten = r.setJSON(rewritten, "response.usage.upstream_credit", merged.UpstreamCredit)
	}
	if usage.Get("total_tokens").Exists() {
		rewritten = r.setJSON(rewritten, "response.usage.total_tokens", merged.InputTokens+merged.OutputTokens)
	}
	return rewritten
}

func (r *openAIStreamContinuationReplayer) setOptionalUsage(payload []byte, source gjson.Result, field string, value int) []byte {
	if value == 0 && !source.Get(field).Exists() {
		return payload
	}
	return r.setJSON(payload, "response.usage."+field, value)
}

func (r *openAIStreamContinuationReplayer) countDroppedReasoning() {
	if r.counter != nil {
		r.counter.AddDroppedReasoningEvents(1)
	}
}

func (r *openAIStreamContinuationReplayer) allocateOutputIndex() int {
	index := r.nextOutputIndex
	r.nextOutputIndex++
	return index
}

func (r *openAIStreamContinuationReplayer) outputIndexForItem(itemID string) (int, bool) {
	if itemID == "" {
		return 0, false
	}
	index, ok := r.itemMap[itemID]
	return index, ok
}

func (r *openAIStreamContinuationReplayer) rewriteNewItemIndex(payload []byte) []byte {
	index, ok := r.outputIndexForItem(gjson.GetBytes(payload, "item_id").String())
	if !ok {
		return payload
	}
	return r.setJSON(payload, "output_index", index)
}

func (r *openAIStreamContinuationReplayer) deliverJSON(eventLine string, hasEvent bool, payload []byte) error {
	r.nextSequence++
	rewritten := r.setJSON(payload, "sequence_number", r.nextSequence)
	return r.writeFrame(eventLine, hasEvent, rewritten)
}

func (r *openAIStreamContinuationReplayer) writeFrame(eventLine string, hasEvent bool, payload []byte) error {
	if hasEvent && eventLine != "" {
		if _, err := r.buffered.WriteString(eventLine); err != nil {
			return err
		}
		if err := r.buffered.WriteByte('\n'); err != nil {
			return err
		}
	}
	if _, err := r.buffered.WriteString("data: "); err != nil {
		return err
	}
	if _, err := r.buffered.Write(payload); err != nil {
		return err
	}
	if err := r.buffered.WriteByte('\n'); err != nil {
		return err
	}
	if err := r.buffered.WriteByte('\n'); err != nil {
		return err
	}
	return r.buffered.Flush()
}

func openAIStreamContinuationOverlap(prior, prefix string) int {
	limit := len(prior)
	if len(prefix) < limit {
		limit = len(prefix)
	}
	if limit > openAIStreamContinuationOverlapWindow {
		limit = openAIStreamContinuationOverlapWindow
	}
	for k := limit; k > 0; k-- {
		// k 是 prefix 的切分点；只接受完整 rune 边界，避免把多字节
		// rune 切成非法 UTF-8（S2）。k == len(prefix) 由调用方保证对齐。
		if k < len(prefix) && !utf8.RuneStart(prefix[k]) {
			continue
		}
		if strings.HasSuffix(prior, prefix[:k]) {
			return k
		}
	}
	return 0
}

func (r *openAIStreamContinuationReplayer) rewriteMessageContent(payload []byte, contentPath, text string) []byte {
	content := gjson.GetBytes(payload, contentPath)
	if !content.IsArray() || len(content.Array()) == 0 {
		raw, err := json.Marshal([]any{
			map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			},
		})
		if err != nil {
			return payload
		}
		return r.setRawJSON(payload, contentPath, raw)
	}
	rewritten := r.setJSON(payload, contentPath+".0.text", text)
	if gjson.GetBytes(rewritten, contentPath+".0.type").String() == "" {
		rewritten = r.setJSON(rewritten, contentPath+".0.type", "output_text")
	}
	return rewritten
}

// setJSON / setRawJSON 在 sjson 改写失败时保留原帧，并记录首个失败点（S3）。
func (r *openAIStreamContinuationReplayer) setJSON(payload []byte, path string, value any) []byte {
	rewritten, err := setOpenAIStreamJSON(payload, path, value)
	if err != nil {
		r.noteRewriteFailure(path, err)
	}
	return rewritten
}

func (r *openAIStreamContinuationReplayer) setRawJSON(payload []byte, path string, raw []byte) []byte {
	rewritten, err := setOpenAIStreamRawJSON(payload, path, raw)
	if err != nil {
		r.noteRewriteFailure(path, err)
	}
	return rewritten
}

func (r *openAIStreamContinuationReplayer) noteRewriteFailure(path string, err error) {
	if r == nil || err == nil || r.rewriteFailure != nil {
		return
	}
	r.rewriteFailure = &openAIStreamRewriteFailure{
		frameType: r.frameType,
		path:      path,
		err:       err,
	}
}

// logRewriteFailure 在流收尾时打一条 WARN，含帧类型与原始错误。
// 失败帧已按原样下发，这里只做可观测性，不改变客户端行为。
func (r *openAIStreamContinuationReplayer) logRewriteFailure() {
	if r == nil || r.rewriteFailure == nil {
		return
	}
	failure := r.rewriteFailure
	logger.L().Warn("gateway.openai_stream_continuation_rewrite_failed",
		zap.String("component", "service.openai_stream_continuation"),
		zap.String("frame_type", failure.frameType),
		zap.String("path", failure.path),
		zap.Error(failure.err),
	)
}

// setOpenAIStreamJSON 返回改写后的帧与 sjson 的原始错误；调用方决定降级策略。
func setOpenAIStreamJSON(payload []byte, path string, value any) ([]byte, error) {
	rewritten, err := sjson.SetBytes(payload, path, value)
	if err != nil {
		return payload, err
	}
	return rewritten, nil
}

func setOpenAIStreamRawJSON(payload []byte, path string, raw []byte) ([]byte, error) {
	rewritten, err := sjson.SetRawBytes(payload, path, raw)
	if err != nil {
		return payload, err
	}
	return rewritten, nil
}
