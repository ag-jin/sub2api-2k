package kiro

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// ---- MCP request/response types (web_search) ----

// mcpArgMeta mirrors the _meta block real Kiro 0.12.x sends inside web_search
// arguments (IDE form-completion state). Values are constant in practice.
type mcpArgMeta struct {
	IsValid        bool       `json:"_isValid"`
	ActivePath     []string   `json:"_activePath"`
	CompletedPaths [][]string `json:"_completedPaths"`
}
type mcpArguments struct {
	Query string      `json:"query"`
	Meta  *mcpArgMeta `json:"_meta,omitempty"`
}
type mcpParams struct {
	Name      string       `json:"name"`
	Arguments mcpArguments `json:"arguments"`
}
type mcpRequest struct {
	ID      string    `json:"id"`
	JSONRPC string    `json:"jsonrpc"`
	Method  string    `json:"method"`
	Params  mcpParams `json:"params"`
}
type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type mcpResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError"`
}
type mcpResponse struct {
	Error  *mcpError  `json:"error"`
	ID     string     `json:"id"`
	Result *mcpResult `json:"result"`
}

// WebSearchResult is a single search hit.
type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// WebSearchResults is the parsed MCP web_search payload.
type WebSearchResults struct {
	Results      []WebSearchResult `json:"results"`
	TotalResults int               `json:"totalResults,omitempty"`
	Query        string            `json:"query,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// HasWebSearchTool reports whether the request is a pure web_search request
// (exactly one tool, named "web_search"). Mirrors kiro.rs has_web_search_tool.
func HasWebSearchTool(req *AnthropicRequest) bool {
	return len(req.Tools) == 1 && req.Tools[0].Name == "web_search"
}

// ExtractSearchQuery pulls the latest user text to use as the search query.
func ExtractSearchQuery(req *AnthropicRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		q := strings.TrimSpace(textFromContent(req.Messages[i].Content))
		if q != "" {
			return q
		}
	}
	return ""
}

func textFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func randAlphaNum(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		idx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		b[i] = charset[idx.Int64()]
	}
	return string(b)
}

// mcpURL returns the Kiro MCP endpoint for a credential's region.
//
// New Kiro 0.12.x serves MCP (web_search) from runtime.{region}.kiro.dev/mcp,
// same host as generateAssistantResponse (old: q.{region}.amazonaws.com/mcp).
func mcpURL(cred *KiroCredential) string {
	return fmt.Sprintf("https://runtime.%s.kiro.dev/mcp", cred.EffectiveAPIRegion())
}

// callMCP performs the web_search MCP JSON-RPC call.
func callMCP(ctx context.Context, cred *KiroCredential, cfg KiroConfig, query string) (*WebSearchResults, string, error) {
	toolUseID := fmt.Sprintf("web_search_tooluse_%s_%d_%s",
		randAlphaNum(22), time.Now().UnixMilli(), randAlphaNum(8))

	// Request body matches a real Kiro 0.12.301 /mcp tools/call: the JSON-RPC id
	// is "web_search_tooluse_" + 22 random chars, and arguments carry a _meta
	// block describing the IDE form-completion state.
	reqObj := mcpRequest{
		ID:      "web_search_tooluse_" + randAlphaNum(22),
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: mcpParams{
			Name: "web_search",
			Arguments: mcpArguments{
				Query: query,
				Meta: &mcpArgMeta{
					IsValid:        true,
					ActivePath:     []string{"query"},
					CompletedPaths: [][]string{{"query"}},
				},
			},
		},
	}
	bodyBytes, _ := json.Marshal(reqObj)

	host := fmt.Sprintf("runtime.%s.kiro.dev", cred.EffectiveAPIRegion())
	mid := cred.EffectiveMachineID()
	userAgent := fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/%s lang/js md/nodejs#%s api/codewhispererstreaming#%s m/N KiroIDE-%s-%s",
		AwsSdkVersionAPI, cred.EffectiveSystemVersion(cfg), cfg.NodeVersion, AwsSdkVersionAPI, cfg.KiroVersion, mid,
	)
	amzUA := fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s-%s", AwsSdkVersionAPI, cfg.KiroVersion, mid)

	client, err := buildHTTPClient(cred.ProxyURL, 120*time.Second)
	if err != nil {
		return nil, toolUseID, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL(cred), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, toolUseID, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-amz-user-agent", amzUA)
	req.Header.Set("user-agent", userAgent)
	req.Header.Set("host", host)
	req.Header.Set("amz-sdk-invocation-id", newUUID())
	req.Header.Set("amz-sdk-request", "attempt=1; max=3")
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	if cred.ProfileArn != "" {
		req.Header.Set("x-amzn-kiro-profile-arn", cred.ProfileArn)
	}
	req.Header.Set("Connection", "close")

	resp, err := client.Do(req)
	if err != nil {
		return nil, toolUseID, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, toolUseID, fmt.Errorf("mcp web_search failed: %d %s", resp.StatusCode, string(respBody))
	}

	var mr mcpResponse
	if err := json.Unmarshal(respBody, &mr); err != nil {
		return nil, toolUseID, fmt.Errorf("decode mcp response: %w", err)
	}
	if mr.Error != nil {
		return nil, toolUseID, fmt.Errorf("mcp error %d: %s", mr.Error.Code, mr.Error.Message)
	}

	results := parseSearchResults(&mr)
	return results, toolUseID, nil
}

// parseSearchResults extracts WebSearchResults from the MCP result content.
func parseSearchResults(mr *mcpResponse) *WebSearchResults {
	if mr.Result == nil || len(mr.Result.Content) == 0 {
		return nil
	}
	for _, c := range mr.Result.Content {
		if c.Type == "text" && c.Text != "" {
			var wsr WebSearchResults
			if json.Unmarshal([]byte(c.Text), &wsr) == nil {
				return &wsr
			}
		}
	}
	return nil
}

// HandleWebSearch executes a web_search request: refresh-aware token must be set,
// calls the MCP endpoint, and writes a synthesized Anthropic SSE stream to w.
func HandleWebSearch(ctx context.Context, w SSEWriter, cred *KiroCredential, cfg KiroConfig, req *AnthropicRequest, inputTokens int) error {
	query := ExtractSearchQuery(req)
	if query == "" {
		query = "search"
	}
	results, toolUseID, err := callMCP(ctx, cred, cfg, query)
	if err != nil {
		// Even on MCP failure, emit a well-formed (empty-result) stream so the
		// client gets a valid response instead of a hard error.
		return emitWebSearchEvents(w, req.Model, query, toolUseID, nil, inputTokens)
	}
	return emitWebSearchEvents(w, req.Model, query, toolUseID, results, inputTokens)
}

func emitWebSearchEvents(w SSEWriter, model, query, toolUseID string, results *WebSearchResults, inputTokens int) error {
	messageID := "msg_" + strings.ReplaceAll(newUUID(), "-", "")
	if len(messageID) > 28 {
		messageID = messageID[:28]
	}

	emit := func(ev string, data map[string]interface{}) error { return w.WriteSSE(ev, data) }

	// 1. message_start
	if err := emit("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": messageID, "type": "message", "role": "assistant", "model": model,
			"content": []interface{}{}, "stop_reason": nil,
			"usage": map[string]interface{}{
				"input_tokens": inputTokens, "output_tokens": 0,
				"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
			},
		},
	}); err != nil {
		return err
	}

	// 2. text block (index 0): search decision
	emit("content_block_start", map[string]interface{}{"type": "content_block_start", "index": 0,
		"content_block": map[string]interface{}{"type": "text", "text": ""}})
	emit("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": fmt.Sprintf("I'll search for \"%s\".", query)}})
	emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})

	// 3-4. server_tool_use (index 1)
	emit("content_block_start", map[string]interface{}{"type": "content_block_start", "index": 1,
		"content_block": map[string]interface{}{"id": toolUseID, "type": "server_tool_use",
			"name": "web_search", "input": map[string]interface{}{"query": query}}})
	emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 1})

	// 5-6. web_search_tool_result (index 2)
	var searchContent []map[string]interface{}
	if results != nil {
		for _, r := range results.Results {
			searchContent = append(searchContent, map[string]interface{}{
				"type": "web_search_result", "title": r.Title, "url": r.URL,
				"encrypted_content": r.Snippet, "page_age": nil,
			})
		}
	}
	if searchContent == nil {
		searchContent = []map[string]interface{}{}
	}
	emit("content_block_start", map[string]interface{}{"type": "content_block_start", "index": 2,
		"content_block": map[string]interface{}{"type": "web_search_tool_result", "content": searchContent}})
	emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 2})

	// 7-9. summary text (index 3)
	emit("content_block_start", map[string]interface{}{"type": "content_block_start", "index": 3,
		"content_block": map[string]interface{}{"type": "text", "text": ""}})
	summary := generateSearchSummary(query, results)
	runes := []rune(summary)
	const chunk = 100
	for i := 0; i < len(runes); i += chunk {
		end := i + chunk
		if end > len(runes) {
			end = len(runes)
		}
		emit("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": 3,
			"delta": map[string]interface{}{"type": "text_delta", "text": string(runes[i:end])}})
	}
	emit("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 3})

	// 10. message_delta
	outputTokens := (len(summary) + 3) / 4
	emit("message_delta", map[string]interface{}{"type": "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn"},
		"usage": map[string]interface{}{"output_tokens": outputTokens,
			"server_tool_use": map[string]interface{}{"web_search_requests": 1}}})

	// 11. message_stop
	return emit("message_stop", map[string]interface{}{"type": "message_stop"})
}

func generateSearchSummary(query string, results *WebSearchResults) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Here are the search results for \"%s\":\n\n", query)
	if results != nil && len(results.Results) > 0 {
		for i, r := range results.Results {
			fmt.Fprintf(&b, "%d. **%s**\n", i+1, r.Title)
			if r.Snippet != "" {
				sn := r.Snippet
				rs := []rune(sn)
				if len(rs) > 200 {
					sn = string(rs[:200]) + "..."
				}
				fmt.Fprintf(&b, "   %s\n", sn)
			}
			fmt.Fprintf(&b, "   Source: %s\n\n", r.URL)
		}
	} else {
		b.WriteString("No results found.\n")
	}
	b.WriteString("\nPlease note that these are web search results and may not be fully accurate or up-to-date.")
	return b.String()
}
