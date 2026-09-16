package service

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CodeBuddy 出站请求体指纹净化（对齐参考实现 workbuddy2api internal/upstream/sanitize.go）。
//
// 背景：上游按**逐字精确匹配**（非语义审核）拦截第三方客户端在提示词里注入的固定模板句/
// 键值段，命中即整条请求 HTTP 400（业务码 11128）。一字改动即可绕过。
//
// 三层处理：
//  1. 改写层 codeBuddyFingerprintRewrites：整句只改一个词/一小段，语义不变；
//  2. 剥离层 codeBuddyBillingHeaderRe：`x-anthropic-billing-header: …;` 键值段整段删除
//     （匹配不看值，只看键名，大小写不敏感）；
//  3. 剥离层 codeBuddyBareKVRe：尾随裸键值 `cc_xxx=…;` 循环清理（前缀被上层改写后可能暴露新的
//     裸键值，故循环到稳定）。
//
// 覆盖范围：**所有角色**消息的 content（字符串或 parts 数组的 text）以及
// assistant.tool_calls[].function.arguments（字符串化 JSON——历史盲区：工具轮 content 常为 null，
// 早期实现整条消息跳过，导致写进工具参数的被拦文本原样漏出）。
//
// 与「角色判定」的关系：本仓 2026-09-13 实测该上游的**提示词指纹**判定以 system/assistant 内容为主
// （user 正文里的同一句当时放行）；但参考实现对本网络实测的抗探测规则（裸 11128）为**上下文无关**，
// 且实测确认 `x-anthropic-billing-header` 与 Anthropic 反馈句在本端点同样整单拦截（2026-09-14）。
// 为不因角色差异漏拦，净化统一覆盖全部角色：代价是用户/工具内容里的命中串也会被改写，
// 这是参考实现明确接受的取舍（"这串数字出现在请求里本身就是拦截条件，不改写必然失败"）。
var codeBuddyFingerprintRewrites = [][2]string{
	{
		// 身份句匹配串**不带结尾标点**：CLI 版以句号收尾（"…for Claude."），
		// 桌面版（claude-desktop-3p / Agent SDK）以逗号接后继内容，带标点会漏一种形态。
		// 仍要求 "You are Claude Code, " 前缀，不做更宽的子串替换以免误伤零散文本。
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		// 常见 agent 环境块里的注入指令句（ZCode 的 gitStatus 形态触发过生产 400）。
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		// Codex CLI instructions 首段（防御性：本端点暂未复现拦截）。
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		// Anthropic 反馈句：整句带 github 仓库链接，上游按整句拦截（2026-09-14 本端点实测 400/11128）。
		// give→provide 一词之差即可绕过，语义不变。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// 上游反探测：请求体里出现裸数字 11128 即可能整单拦截（这正是本类拦截自身的错误码，
		// 上游据此识别"在讨论/回显其内部错误码"的请求）。本端点暂未复现，属防御性对齐
		// （客户端把报错原文贴回对话时会带入该串）。插入连字符保留可读性与指代。
		"11128",
		"11-128",
	},
}

// codeBuddySanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求零分配原样返回）。截断前缀即可命中即可（如 "Main branch (" 覆盖带冒号/不带冒号两种收尾）。
var codeBuddySanitizeFeatures = []string{
	"You are Claude Code", // 身份句
	"Main branch (",       // 注入指令句
	"You are a coding agent running in the Codex CLI", // Codex instructions 首段
	"github.com/anthropics/",                          // 反馈句里的仓库链接
	"x-anthropic-billing-header",                      // 计费头键名（小写形态）
	"cc_",                                             // 裸键值前缀
	"11128",                                           // 上游反探测错误码
}

var (
	// codeBuddyBillingHeaderRe 剥离 `x-anthropic-billing-header: …;` 键值段（键名大小写不敏感）。
	codeBuddyBillingHeaderRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)
	// codeBuddyBareKVRe 剥离尾随裸键值 `cc_xxx=…;`。
	codeBuddyBareKVRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)
)

// sanitizeCodeBuddyFingerprintText 单段文本净化。未命中 → 原串原样返回（不 trim，保持字节不变）。
func sanitizeCodeBuddyFingerprintText(text string) string {
	if !codeBuddyTextHasFingerprint(text) {
		return text
	}
	out := text
	for _, rw := range codeBuddyFingerprintRewrites {
		out = strings.ReplaceAll(out, rw[0], rw[1])
	}
	if codeBuddyBillingHeaderRe.MatchString(out) {
		out = codeBuddyBillingHeaderRe.ReplaceAllString(out, "")
	}
	if strings.Contains(out, "cc_") {
		prev := ""
		for prev != out { // 前缀被改写后可能暴露新的裸键值，循环到稳定
			prev = out
			out = codeBuddyBareKVRe.ReplaceAllString(out, "")
		}
	}
	if out == text {
		return text
	}
	return strings.TrimSpace(out)
}

// codeBuddyTextHasFingerprint 特征预检：Contains 快速路径 + 计费头键名的大小写变体正则兜底。
func codeBuddyTextHasFingerprint(text string) bool {
	for _, f := range codeBuddySanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return codeBuddyBillingHeaderRe.MatchString(text)
}

// codeBuddyBodyMayContainFingerprint 请求体（原始 JSON 字节）级预检：不命中则整条请求跳过净化。
func codeBuddyBodyMayContainFingerprint(body []byte) bool {
	for _, f := range codeBuddySanitizeFeatures {
		if strings.Contains(string(body), f) {
			return true
		}
	}
	return codeBuddyBillingHeaderRe.MatchString(string(body))
}

// sanitizeCodeBuddyBodyFingerprints 净化消息体：所有角色的 content（字符串/parts 数组）与
// tool_calls[].function.arguments。无命中时原字节返回。
func sanitizeCodeBuddyBodyFingerprints(body []byte) ([]byte, error) {
	if !codeBuddyBodyMayContainFingerprint(body) {
		return body, nil
	}
	arr := gjson.GetBytes(body, "messages")
	if !arr.IsArray() {
		return body, nil
	}
	out := body
	for i, msg := range arr.Array() {
		var err error
		if out, err = sanitizeCodeBuddyMessageContent(out, msg, i); err != nil {
			return nil, err
		}
		if out, err = sanitizeCodeBuddyToolCallArguments(out, msg, i); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// sanitizeCodeBuddyMessageContent 净化单条消息的 content（字符串或 parts 数组的 text）。
// 角色不参与判定（见文件头注释）。
func sanitizeCodeBuddyMessageContent(out []byte, msg gjson.Result, idx int) ([]byte, error) {
	content := msg.Get("content")
	switch {
	case content.Type == gjson.String:
		scrubbed := sanitizeCodeBuddyFingerprintText(content.String())
		if scrubbed == content.String() {
			return out, nil
		}
		updated, err := sjson.SetBytes(out, fmt.Sprintf("messages.%d.content", idx), scrubbed)
		if err != nil {
			return nil, fmt.Errorf("sanitize codebuddy message content: %w", err)
		}
		return updated, nil
	case content.IsArray():
		for j, part := range content.Array() {
			text := part.Get("text")
			if text.Type != gjson.String {
				continue
			}
			scrubbed := sanitizeCodeBuddyFingerprintText(text.String())
			if scrubbed == text.String() {
				continue
			}
			updated, err := sjson.SetBytes(out, fmt.Sprintf("messages.%d.content.%d.text", idx, j), scrubbed)
			if err != nil {
				return nil, fmt.Errorf("sanitize codebuddy message content part: %w", err)
			}
			out = updated
		}
		return out, nil
	default:
		return out, nil
	}
}

// sanitizeCodeBuddyToolCallArguments 净化 assistant.tool_calls[].function.arguments。
// arguments 是字符串化 JSON，按文本走净化即可；content 为 null 的工具轮也必须处理（历史盲区）。
func sanitizeCodeBuddyToolCallArguments(out []byte, msg gjson.Result, idx int) ([]byte, error) {
	calls := msg.Get("tool_calls")
	if !calls.IsArray() {
		return out, nil
	}
	for k, call := range calls.Array() {
		args := call.Get("function.arguments")
		if args.Type != gjson.String {
			continue
		}
		scrubbed := sanitizeCodeBuddyFingerprintText(args.String())
		if scrubbed == args.String() {
			continue
		}
		updated, err := sjson.SetBytes(out, fmt.Sprintf("messages.%d.tool_calls.%d.function.arguments", idx, k), scrubbed)
		if err != nil {
			return nil, fmt.Errorf("sanitize codebuddy tool call arguments: %w", err)
		}
		out = updated
	}
	return out, nil
}
