package service

import (
	"strconv"
	"strings"
	"time"
)

// 503 流式断开自动续跑的重试预算/节奏（v1.2 设置化）。
//
// v1/v1.1 的预算是包内常量（2 次 / 300+900ms 两段退避 / 90s 时间窗）。生产实证
// （2026-10-21：触发 113 / 救回 48 / 预算耗尽 22，reason=budget 且 elapsed ~5s，
// 全部为推理期上游 server_is_overloaded 秒级闪断）表明次数预算先于时间窗耗尽，
// 上游容量闪断可抢。v1.2 把预算做成 gateway 设置并加大默认值：同模型加大重试
// 力度，不做模型回退、不做并行竞速。
//
// 越界语义：字段缺失 / 非数字 / ≤0 / 越硬限 → 该字段回默认值（回退，不是钳制），
// 避免"填 20 次实际按 10 次跑"的静默钳制；各字段独立判定，互不影响。

const (
	// 默认值（v1.2 加大：抢上游秒级容量闪断）。
	openAIStream503ContinueDefaultMaxContinuations = 6
	openAIStream503ContinueDefaultBackoffInitial   = 150 * time.Millisecond
	openAIStream503ContinueDefaultBackoffMax       = 2000 * time.Millisecond
	openAIStream503ContinueDefaultTotalBudget      = 30 * time.Second

	// 硬限：配置越界视为非法，整字段回默认。全部在原始 int 毫秒上判定，
	// 避免 time.Duration 换算回绕；backoff_initial 的上界只是防溢出护栏，
	// 允许 initial > max（封顶语义）。
	openAIStream503ContinueHardMaxContinuations    = 10
	openAIStream503ContinueHardMinBackoffInitialMs = 50
	openAIStream503ContinueHardMaxBackoffInitialMs = 120000
	openAIStream503ContinueHardMaxBackoffMaxMs     = 10000
	openAIStream503ContinueHardMaxTotalBudgetMs    = 120000

	// delay 指数左移的防御上限：预算硬上限 10 次时最大移位为 9，16 已远超所需。
	openAIStream503ContinueMaxBackoffShift = 16
)

// openAIStream503ContinueBudget 是一次 Forward 请求内冻结的重试预算。
// 由 newOpenAIStreamContinuationState 创建 state 时从设置缓存解析一次，
// 单次请求内各续跑 attempt 使用同一份预算。
type openAIStream503ContinueBudget struct {
	maxContinuations int
	backoffInitial   time.Duration
	backoffMax       time.Duration
	totalBudget      time.Duration
}

func defaultOpenAIStream503ContinueBudget() openAIStream503ContinueBudget {
	return openAIStream503ContinueBudget{
		maxContinuations: openAIStream503ContinueDefaultMaxContinuations,
		backoffInitial:   openAIStream503ContinueDefaultBackoffInitial,
		backoffMax:       openAIStream503ContinueDefaultBackoffMax,
		totalBudget:      openAIStream503ContinueDefaultTotalBudget,
	}
}

// normalized 补齐零值字段：测试或历史代码直接构造的 state 零值预算回退默认，
// 保证与"设置未配置"行为一致。
func (b openAIStream503ContinueBudget) normalized() openAIStream503ContinueBudget {
	def := defaultOpenAIStream503ContinueBudget()
	if b.maxContinuations <= 0 {
		b.maxContinuations = def.maxContinuations
	}
	if b.backoffInitial <= 0 {
		b.backoffInitial = def.backoffInitial
	}
	if b.backoffMax <= 0 {
		b.backoffMax = def.backoffMax
	}
	if b.totalBudget <= 0 {
		b.totalBudget = def.totalBudget
	}
	return b
}

// delay 返回第 attempt 次续跑（1-based；调用点 st.continuations 已自增）前的退避
// 基值：initial 起每次翻倍，封顶 backoffMax。±20% 抖动由调用方（state.Backoff）叠加。
// initial > max 时延迟恒为 max（封顶语义，不算非法配置）。
func (b openAIStream503ContinueBudget) delay(attempt int) time.Duration {
	b = b.normalized()
	if attempt <= 1 {
		return b.backoffInitial
	}
	shift := attempt - 1
	if shift > openAIStream503ContinueMaxBackoffShift {
		shift = openAIStream503ContinueMaxBackoffShift
	}
	capped := b.backoffInitial << shift
	if capped <= 0 || capped > b.backoffMax {
		return b.backoffMax
	}
	return capped
}

// normalizeOpenAIStream503ContinueBudget 把 GetMultiple 读到的原始设置值解析为
// 生效预算；未配置的字段保持新默认值。界限判定在原始 int 毫秒上做，
// 先比较后换算，杜绝超大输入的 time.Duration 回绕。
func normalizeOpenAIStream503ContinueBudget(values map[string]string) openAIStream503ContinueBudget {
	budget := defaultOpenAIStream503ContinueBudget()
	if n, ok := parseOpenAIStream503ContinueInt(values[SettingKeyOpenAIStream503MaxContinuations]); ok &&
		n >= 1 && n <= openAIStream503ContinueHardMaxContinuations {
		budget.maxContinuations = n
	}
	if ms, ok := parseOpenAIStream503ContinueInt(values[SettingKeyOpenAIStream503BackoffInitialMs]); ok &&
		ms >= openAIStream503ContinueHardMinBackoffInitialMs && ms <= openAIStream503ContinueHardMaxBackoffInitialMs {
		budget.backoffInitial = time.Duration(ms) * time.Millisecond
	}
	if ms, ok := parseOpenAIStream503ContinueInt(values[SettingKeyOpenAIStream503BackoffMaxMs]); ok &&
		ms > 0 && ms <= openAIStream503ContinueHardMaxBackoffMaxMs {
		budget.backoffMax = time.Duration(ms) * time.Millisecond
	}
	if ms, ok := parseOpenAIStream503ContinueInt(values[SettingKeyOpenAIStream503TotalBudgetMs]); ok &&
		ms > 0 && ms <= openAIStream503ContinueHardMaxTotalBudgetMs {
		budget.totalBudget = time.Duration(ms) * time.Millisecond
	}
	return budget
}

// parseOpenAIStream503ContinueInt 解析设置原始字符串；缺失/空白/非数字 → (0,false)。
func parseOpenAIStream503ContinueInt(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}

// openAIStream503ContinueBudgetFromSystemSettings 把面板解析后的 SystemSettings
// 字段重新过一遍硬限归一。面板层不做硬限校验（service 层统一裁决），
// 落库值必须可直接生效：0/负数/越界在写入前就被归一为默认。
func openAIStream503ContinueBudgetFromSystemSettings(settings *SystemSettings) openAIStream503ContinueBudget {
	if settings == nil {
		return defaultOpenAIStream503ContinueBudget()
	}
	return normalizeOpenAIStream503ContinueBudget(map[string]string{
		SettingKeyOpenAIStream503MaxContinuations: strconv.Itoa(settings.OpenAIStream503MaxContinuations),
		SettingKeyOpenAIStream503BackoffInitialMs: strconv.Itoa(settings.OpenAIStream503BackoffInitialMs),
		SettingKeyOpenAIStream503BackoffMaxMs:     strconv.Itoa(settings.OpenAIStream503BackoffMaxMs),
		SettingKeyOpenAIStream503TotalBudgetMs:    strconv.Itoa(settings.OpenAIStream503TotalBudgetMs),
	})
}
