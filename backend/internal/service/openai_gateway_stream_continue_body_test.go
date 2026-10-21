//go:build unit

package service

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 05 — 续跑 SSE body 拼接/重写器的字节级单测。
// 纯字节流，不启动上游、不经过 handler。

func contBodyTestSnapshot(partial string) *openAIStreamContinuationSnapshot {
	return &openAIStreamContinuationSnapshot{
		responseID:      "resp_first",
		createdAt:       1700000000,
		priorUsage:      OpenAIUsage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 2},
		partialText:     partial,
		openTextItemID:  "msg_first",
		openTextIndex:   1,
		openTextContent: 0,
		nextOutputIndex: 2,
		lastSequence:    10,
		keepOriginalID:  true,
	}
}

type contBodyTrackingSource struct {
	reader io.Reader
	closes atomic.Int32
}

func (s *contBodyTrackingSource) Read(p []byte) (int, error) { return s.reader.Read(p) }

func (s *contBodyTrackingSource) Close() error {
	s.closes.Add(1)
	return nil
}

func contBodyRun(t *testing.T, upstream string, snapshot *openAIStreamContinuationSnapshot, state *openAIStreamContinuationState, maxLineSize int) string {
	t.Helper()
	source := &contBodyTrackingSource{reader: strings.NewReader(upstream)}
	var body io.ReadCloser
	if state != nil {
		body = newOpenAIStreamContinuationBody(source, snapshot, maxLineSize, state)
	} else {
		body = newOpenAIStreamContinuationBody(source, snapshot, maxLineSize)
	}
	output, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	require.Eventually(t, func() bool {
		return source.closes.Load() == 1
	}, time.Second, 5*time.Millisecond, "source must be closed exactly once")
	require.Equal(t, int32(1), source.closes.Load())
	return string(output)
}

func contBodyFrames(t *testing.T, output string) []gjson.Result {
	t.Helper()
	frames := make([]gjson.Result, 0, 8)
	for _, line := range strings.Split(output, "\n") {
		data, ok := extractOpenAISSEDataLine(line)
		if !ok {
			continue
		}
		if data == "[DONE]" {
			continue
		}
		require.True(t, gjson.Valid(data), "invalid JSON frame downstream: %s", data)
		frames = append(frames, gjson.Parse(data))
	}
	return frames
}

func contBodySSE(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func TestOpenAIStreamContinuationBodySplicesFirstMessage(t *testing.T) {
	snapshot := contBodyTestSnapshot("Hello wor")
	upstream := contBodySSE(
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_second"},"sequence_number":1}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_second","type":"message","role":"assistant","content":[]},"sequence_number":2}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","item_id":"msg_second","output_index":0,"content_index":0,"delta":"ld!"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_second","created_at":1700000999,"status":"completed","output":[{"id":"msg_second","type":"message","role":"assistant","content":[{"type":"output_text","text":"ld!"}]}],"usage":{"input_tokens":3,"output_tokens":2}},"sequence_number":4}`,
		"",
	)

	output := contBodyRun(t, upstream, snapshot, nil, 0)

	require.NotContains(t, output, `"type":"response.created"`)
	require.NotContains(t, output, `"type":"response.in_progress"`)
	require.NotContains(t, output, `"type":"response.queued"`)
	require.NotContains(t, output, `"type":"response.output_item.added"`)
	require.Equal(t, 1, strings.Count(output, `"type":"response.completed"`))

	frames := contBodyFrames(t, output)
	require.Len(t, frames, 2)

	delta := frames[0]
	require.Equal(t, "response.output_text.delta", delta.Get("type").String())
	require.Equal(t, "msg_first", delta.Get("item_id").String())
	require.Equal(t, int64(1), delta.Get("output_index").Int())
	require.Equal(t, int64(0), delta.Get("content_index").Int())
	require.Equal(t, "ld!", delta.Get("delta").String())

	for index, frame := range frames {
		require.Equal(t, snapshot.lastSequence+1+int64(index), frame.Get("sequence_number").Int(),
			"sequence_number must be strictly increasing from lastSequence+1")
	}
}

func TestOpenAIStreamContinuationBodyRewritesTerminalAndUsage(t *testing.T) {
	snapshot := contBodyTestSnapshot("Hello wor")
	snapshot.completedItems = []json.RawMessage{json.RawMessage(`{"id":"rs_1","type":"reasoning","summary":[]}`)}
	upstream := contBodySSE(
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_second"},"sequence_number":1}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_second","type":"message","role":"assistant","content":[]},"sequence_number":2}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","item_id":"msg_second","output_index":0,"content_index":0,"delta":"ld!","sequence_number":3}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_second","created_at":1700000999,"output":[],"usage":{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":4}},"sequence_number":4}`,
		"",
	)

	output := contBodyRun(t, upstream, snapshot, nil, 0)
	frames := contBodyFrames(t, output)
	require.Len(t, frames, 2)

	completed := frames[1]
	require.Equal(t, "response.completed", completed.Get("type").String())
	require.Equal(t, "resp_first", completed.Get("response.id").String())
	require.Equal(t, int64(1700000000), completed.Get("response.created_at").Int())

	// response.output = 第 1 次已完成 items + 合并后的文本 message。
	items := completed.Get("response.output").Array()
	require.Len(t, items, 2)
	require.Equal(t, "reasoning", items[0].Get("type").String())
	require.Equal(t, "message", items[1].Get("type").String())
	require.Equal(t, "msg_first", items[1].Get("id").String())
	require.Equal(t, "Hello world!", items[1].Get("content.0.text").String())

	usage := completed.Get("response.usage")
	require.Equal(t, int64(13), usage.Get("input_tokens").Int())
	require.Equal(t, int64(7), usage.Get("output_tokens").Int())
	require.Equal(t, int64(6), usage.Get("cache_read_input_tokens").Int())
}

// S2: 重叠裁剪必须落在完整 rune 边界上，否则会向下游切出非法 UTF-8。
func TestOpenAIStreamContinuationOverlapKeepsRuneBoundary(t *testing.T) {
	// prior 尾部是两个残缺字节（上游异常流），旧实现会匹配出 k=2，
	// 恰好把 prefix 首个多字节 rune（世 = e4 b8 96）切在中间。
	require.Equal(t, 0, openAIStreamContinuationOverlap("abc\xe4\xb8", "世ok"))
}

func TestOpenAIStreamContinuationOverlapMatchesWholeRunes(t *testing.T) {
	prior := "你好世界"
	prefix := "世界！"
	require.Equal(t, len("世界"), openAIStreamContinuationOverlap(prior, prefix))
}

func TestOpenAIStreamContinuationBodyTrimsOverlap(t *testing.T) {
	snapshot := contBodyTestSnapshot("Hello wor")
	state := &openAIStreamContinuationState{}
	upstream := contBodySSE(
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_second"},"sequence_number":1}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_second","type":"message","role":"assistant","content":[]},"sequence_number":2}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","item_id":"msg_second","output_index":0,"content_index":0,"delta":"Hello ","sequence_number":3}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","item_id":"msg_second","output_index":0,"content_index":0,"delta":"world!","sequence_number":4}`,
		"",
		"event: response.output_text.done",
		`data: {"type":"response.output_text.done","item_id":"msg_second","output_index":0,"content_index":0,"text":"Hello world!","sequence_number":5}`,
		"",
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_second","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello world!","annotations":[]}]},"sequence_number":6}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_second","output":[],"usage":{"input_tokens":1,"output_tokens":1}},"sequence_number":7}`,
		"",
	)

	output := contBodyRun(t, upstream, snapshot, state, defaultMaxLineSize)

	require.NotContains(t, output, `"delta":"Hello `)
	require.Contains(t, output, `"delta":"ld!"`)
	require.Equal(t, 9, state.OverlapTrimmedBytes())

	frames := contBodyFrames(t, output)
	var textDone, itemDone, completed gjson.Result
	for _, frame := range frames {
		switch frame.Get("type").String() {
		case "response.output_text.done":
			textDone = frame
		case "response.output_item.done":
			itemDone = frame
		case "response.completed":
			completed = frame
		}
	}
	require.True(t, textDone.Exists())
	require.True(t, itemDone.Exists())
	require.True(t, completed.Exists())

	require.Equal(t, "Hello world!", textDone.Get("text").String())
	require.Equal(t, "msg_first", textDone.Get("item_id").String())
	require.Equal(t, int64(1), textDone.Get("output_index").Int())
	require.Equal(t, int64(0), textDone.Get("content_index").Int())

	require.Equal(t, "Hello world!", itemDone.Get("item.content.0.text").String())
	require.Equal(t, "msg_first", itemDone.Get("item.id").String())
	require.Equal(t, int64(1), itemDone.Get("output_index").Int())

	require.Equal(t, "Hello world!", completed.Get("response.output.0.content.0.text").String())
}

func TestOpenAIStreamContinuationBodyMergesTerminalOutputItems(t *testing.T) {
	snapshot := contBodyTestSnapshot("Hello wor")
	snapshot.openTextIndex = 0
	snapshot.completedItems = nil
	upstream := contBodySSE(
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_second"},"sequence_number":1}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_second","type":"message","role":"assistant","content":[]},"sequence_number":2}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","item_id":"msg_second","output_index":0,"content_index":0,"delta":"ld!","sequence_number":3}`,
		"",
		"event: response.output_text.done",
		`data: {"type":"response.output_text.done","item_id":"msg_second","output_index":0,"content_index":0,"text":"ld!","sequence_number":4}`,
		"",
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_second","type":"message","role":"assistant","content":[{"type":"output_text","text":"ld!"}]},"sequence_number":5}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","name":"lookup","arguments":""},"sequence_number":6}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{}","sequence_number":7}`,
		"",
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","name":"lookup","arguments":"{}"},"sequence_number":8}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_second","output":[],"usage":{"input_tokens":1,"output_tokens":1}},"sequence_number":9}`,
		"",
	)

	output := contBodyRun(t, upstream, snapshot, nil, 0)

	fcAdded := false
	for _, frame := range contBodyFrames(t, output) {
		if frame.Get("type").String() == "response.output_item.added" {
			fcAdded = true
			require.Equal(t, "fc_1", frame.Get("item.id").String())
			require.Equal(t, int64(2), frame.Get("output_index").Int(), "new items start at snapshot.nextOutputIndex")
		}
		if frame.Get("type").String() == "response.function_call_arguments.delta" {
			require.Equal(t, int64(2), frame.Get("output_index").Int())
			require.Equal(t, "fc_1", frame.Get("item_id").String())
		}
	}
	require.True(t, fcAdded)

	completed := contBodyFrames(t, output)[len(contBodyFrames(t, output))-1]
	items := completed.Get("response.output").Array()
	require.Len(t, items, 2)
	require.Equal(t, "message", items[0].Get("type").String())
	require.Equal(t, "Hello world!", items[0].Get("content.0.text").String())
	require.Equal(t, "function_call", items[1].Get("type").String())
	require.Equal(t, "fc_1", items[1].Get("id").String())
}

func TestOpenAIStreamContinuationBodyDropsReasoningEvents(t *testing.T) {
	snapshot := contBodyTestSnapshot("Hello wor")
	state := &openAIStreamContinuationState{}
	upstream := contBodySSE(
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_second"},"sequence_number":1}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_2","type":"reasoning","summary":[]},"sequence_number":2}`,
		"",
		"event: response.reasoning_summary_text.delta",
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_2","output_index":0,"delta":"thinking","sequence_number":3}`,
		"",
		"event: response.reasoning_text.delta",
		`data: {"type":"response.reasoning_text.delta","item_id":"rs_2","output_index":0,"delta":"more","sequence_number":4}`,
		"",
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_2","type":"reasoning","summary":[]},"sequence_number":5}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_second","type":"message","role":"assistant","content":[]},"sequence_number":6}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","item_id":"msg_second","output_index":1,"content_index":0,"delta":"ld!","sequence_number":7}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_second","output":[],"usage":{"input_tokens":1,"output_tokens":1}},"sequence_number":8}`,
		"",
	)

	output := contBodyRun(t, upstream, snapshot, state, 0)

	require.NotContains(t, output, "reasoning")
	require.Equal(t, 1, strings.Count(output, `"type":"response.completed"`))
	require.Equal(t, 4, state.DroppedReasoningEvents())
	require.Contains(t, output, `"delta":"ld!"`)
}

func TestOpenAIStreamContinuationBodyDropsRefusal(t *testing.T) {
	snapshot := contBodyTestSnapshot("Hello wor")
	upstream := contBodySSE(
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_second"},"sequence_number":1}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_second","type":"message","role":"assistant","content":[]},"sequence_number":2}`,
		"",
		"event: response.content_part.added",
		`data: {"type":"response.content_part.added","item_id":"msg_second","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":""},"sequence_number":3}`,
		"",
		"event: response.refusal.delta",
		`data: {"type":"response.refusal.delta","item_id":"msg_second","output_index":0,"content_index":0,"delta":"I cannot","sequence_number":4}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","item_id":"msg_second","output_index":0,"content_index":0,"delta":"ld!","sequence_number":5}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_second","output":[],"usage":{"input_tokens":1,"output_tokens":1}},"sequence_number":6}`,
		"",
	)

	output := contBodyRun(t, upstream, snapshot, nil, 0)

	require.NotContains(t, output, "refusal")
	require.NotContains(t, output, "I cannot")
	completed := contBodyFrames(t, output)[len(contBodyFrames(t, output))-1]
	require.Equal(t, "Hello world!", completed.Get("response.output.0.content.0.text").String())
}

func TestOpenAIStreamContinuationBodyForwardsErrorAndFailed(t *testing.T) {
	snapshot := contBodyTestSnapshot("Hello wor")
	const message = "Our servers are currently overloaded. Please try again later."
	upstream := contBodySSE(
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_second"},"sequence_number":1}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_second","type":"message","role":"assistant","content":[]},"sequence_number":2}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","item_id":"msg_second","output_index":0,"content_index":0,"delta":"ld!","sequence_number":3}`,
		"",
		"event: error",
		`data: {"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"sequence_number":4}`,
		"",
		"event: response.failed",
		`data: {"type":"response.failed","response":{"id":"resp_second","status":"failed","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}},"sequence_number":5}`,
		"",
	)

	output := contBodyRun(t, upstream, snapshot, nil, 0)

	require.Equal(t, 1, strings.Count(output, `"type":"error"`))
	require.Equal(t, 1, strings.Count(output, `"type":"response.failed"`))
	require.Contains(t, output, "server_is_overloaded")
	require.Contains(t, output, message)
	require.NotContains(t, output, "resp_second")
	require.Contains(t, output, `"id":"resp_first"`)

	frames := contBodyFrames(t, output)
	require.Equal(t, "server_is_overloaded", frames[1].Get("error.code").String())
	require.Equal(t, "resp_first", frames[2].Get("response.id").String())
}

func TestOpenAIStreamContinuationBodyForwardsDoneOnceAndKeepsComments(t *testing.T) {
	snapshot := contBodyTestSnapshot("Hello wor")
	upstream := contBodySSE(
		": keepalive",
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_second"},"sequence_number":1}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","item_id":"msg_second","output_index":0,"content_index":0,"delta":"ld!","sequence_number":2}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_second","output":[],"usage":{"input_tokens":1,"output_tokens":1}},"sequence_number":3}`,
		"",
		"data: [DONE]",
		"",
		"data: [DONE]",
		"",
		": trailing comment",
		"",
	)

	output := contBodyRun(t, upstream, snapshot, nil, 0)

	require.Equal(t, 1, strings.Count(output, "[DONE]"))
	require.Contains(t, output, ": keepalive")
	require.Contains(t, output, ": trailing comment")
}

func TestOpenAIStreamContinuationBodyForwardsUnparseableData(t *testing.T) {
	snapshot := contBodyTestSnapshot("Hello wor")
	upstream := contBodySSE(
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_second"},"sequence_number":1}`,
		"",
		"data: not-json-payload",
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_second","output":[],"usage":{"input_tokens":1,"output_tokens":1}},"sequence_number":2}`,
		"",
	)

	output := contBodyRun(t, upstream, snapshot, nil, 0)

	require.Contains(t, output, "data: not-json-payload")
}

// S3: sjson 改写失败时必须保留原帧（降级），并把失败（帧类型 + 原始错误）
// 上报给收尾的单条 WARN 日志，而不是静默吞掉。
func TestSetOpenAIStreamJSONReportsRewriteError(t *testing.T) {
	payload := []byte(`[1,2]`)
	out, err := setOpenAIStreamJSON(payload, "sequence_number", 1)
	require.Error(t, err)
	require.Equal(t, string(payload), string(out), "failed rewrite keeps the original frame")
}

func TestSetOpenAIStreamRawJSONReportsRewriteError(t *testing.T) {
	payload := []byte(`[1,2]`)
	out, err := setOpenAIStreamRawJSON(payload, "response.output", []byte(`[]`))
	require.Error(t, err)
	require.Equal(t, string(payload), string(out), "failed rewrite keeps the original frame")
}

func TestOpenAIStreamContinuationReplayerRecordsFirstRewriteFailure(t *testing.T) {
	r := newOpenAIStreamContinuationReplayer(contBodyTestSnapshot("Hello"), nil, bufio.NewWriter(io.Discard))
	r.frameType = "response.completed"

	out := r.setJSON([]byte(`[1,2]`), "sequence_number", 9)
	require.Equal(t, "[1,2]", string(out))
	require.NotNil(t, r.rewriteFailure)
	require.Equal(t, "response.completed", r.rewriteFailure.frameType)
	require.Equal(t, "sequence_number", r.rewriteFailure.path)
	require.Error(t, r.rewriteFailure.err)

	r.setRawJSON([]byte(`[1,2]`), "response.output", []byte(`[]`))
	require.Equal(t, "sequence_number", r.rewriteFailure.path,
		"only the first failure is kept so the stream logs exactly one WARN")
}

func TestOpenAIStreamContinuationBodyCloseStopsWriter(t *testing.T) {
	snapshot := contBodyTestSnapshot("Hello wor")
	source := &contBodyTrackingSource{reader: strings.NewReader(strings.Repeat(": keepalive\n", 1024))}
	body := newOpenAIStreamContinuationBody(source, snapshot, 0)

	require.NoError(t, body.Close())
	require.Eventually(t, func() bool {
		return source.closes.Load() == 1
	}, time.Second, 5*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, int32(1), source.closes.Load(), "Close must close the source exactly once")
}
