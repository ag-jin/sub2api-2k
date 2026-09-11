package service

// SanitizeStoredCredentials strips secrets that must never be persisted on the
// account credentials map after conversion to OAuth tokens (Grok Web SSO / password).
// Call from admin create/update/import/apply-oauth paths.
//
// Cookie is always stripped: bulk paths may pass an empty platform label, and
// session-jar residue must never sit next to OAuth tokens on any platform.
// The platform argument is retained for call-site clarity / future scrubbing.
func SanitizeStoredCredentials(platform string, creds map[string]any) map[string]any {
	if creds == nil {
		return nil
	}
	// CodeBuddy：auth JSON 归一化（白名单字段式，丢弃 allAccounts/accounts 等桌面端
	// 易变键），enterpriseId 为 None 时归一为空串（R3-M1）。
	if platform == PlatformCodeBuddy {
		if normalized := NormalizeCodeBuddyCredentials(creds); normalized != nil {
			return normalized
		}
		return creds
	}
	for _, key := range []string{
		"password", "sso_token", "sso", "sso-rw", "clearTextPassword", "cookie",
	} {
		delete(creds, key)
	}
	return creds
}
