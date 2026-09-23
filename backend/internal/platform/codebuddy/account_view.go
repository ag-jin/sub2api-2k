package codebuddy

import (
	"reflect"
	"strings"
)

// CodeBuddyAccount 是本包对"一个 CodeBuddy 账号"的最小视图。
//
// 为什么不直接用 service.Account：service 包会调用本包的规范化/净化函数，
// 若本包反向依赖 service 就形成 import cycle。用这个窄接口把依赖方向固定成
// service → platform/codebuddy（单向）。
//
// service.Account 天然满足本接口（GetCredential/GetOpenAIBaseURL 均已存在），
// 无需为它写适配器。
type CodeBuddyAccount interface {
	// GetCredential 读取账号凭据字段（"realm" / "domain" 等）。
	GetCredential(key string) string
	// GetOpenAIBaseURL 返回账号配置的上游 base_url。
	GetOpenAIBaseURL() string
}

// RealmKind 账号所属品牌域。
type RealmKind string

const (
	// RealmCN 国内版（copilot.tencent.com / www.codebuddy.cn）。
	RealmCN RealmKind = "cn"
	// RealmGlobal 国际版（www.workbuddy.ai）。
	RealmGlobal RealmKind = "global"
)

// ResolveRealm 判定账号所属品牌域。
//
// 判定顺序（与 service 包的 codeBuddyAccountRealm 保持一致，勿单方面改动）：
//  1. 凭据里的显式 realm；
//  2. 凭据 domain 或 base_url 含 "workbuddy" → global；
//  3. 缺省 CN。
//
// nil 账号按 CN 处理（与既有行为一致）。
//
// ⚠️ 必须用 isNilAccount 而不是 `a == nil`：调用方传进来的常是
// `(*service.Account)(nil)`，装进接口后**接口本身非 nil**，`a == nil` 为假，
// 随后调用方法会在 GetCredential 里空指针 panic（2026-09-22 实测踩到）。
func ResolveRealm(a CodeBuddyAccount) RealmKind {
	if isNilAccount(a) {
		return RealmCN
	}
	switch strings.ToLower(strings.TrimSpace(a.GetCredential("realm"))) {
	case string(RealmGlobal):
		return RealmGlobal
	case string(RealmCN):
		return RealmCN
	}
	haystack := strings.ToLower(a.GetCredential("domain") + " " + a.GetOpenAIBaseURL())
	if strings.Contains(haystack, "workbuddy") {
		return RealmGlobal
	}
	return RealmCN
}

// isNilAccount 判断接口持有的是否为 nil（含"接口非 nil 但内部指针为 nil"）。
func isNilAccount(a CodeBuddyAccount) bool {
	if a == nil {
		return true
	}
	v := reflect.ValueOf(a)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func:
		return v.IsNil()
	default:
		return false
	}
}
