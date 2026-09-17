package service

// openCodeUpstreamUserAgent 是 opencode GO 出站的自有 UA 兜底。
//
// opencode GO 文档（where-can-i-use-it）要求客户端"用自己的 agent 名，而不是通用
// SDK / HTTP 库名"；Go 栈默认 Go-http-client/1.1 会被其滥用策略盯上。上游主干只
// 实现了强制会话头，未补 UA，本仓沿用 card 血统的兜底：由
// applyOpenCodeSessionHeader 在调用方未指定 UA 时注入。
const openCodeUpstreamUserAgent = "sub2api/1.0"
