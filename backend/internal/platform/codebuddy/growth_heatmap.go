package codebuddy

// 活跃地图热力格（A6 批 P4）。
//
// 补签判据链的第一环：读昨日格 score，`score == 0` 即"漏签"。
// 纯逻辑放平台包（不依赖 service 类型），调用方在 service 侧。

// HeatmapCell 活跃地图热力格（一日一格）。
type HeatmapCell struct {
	// Date 形如 2006-01-02（上游带时区后缀时取前 10 位比较）。
	Date string `json:"date"`
	// Score 当日活跃计分（0 = 漏签）。
	Score int `json:"score"`
}

// HeatmapDayScore 返回 cells 中某日的 score；**无该日格返回 ok=false**。
//
// `ok=false` 与 `score=0` 语义完全不同：前者是"活跃地图没覆盖这一天，
// 无从判断漏没漏签"→ 不该补签；后者是"确实漏了"→ 可以考虑补。
// 把两者混为一谈会导致对没有数据的日期盲目补签（消耗补签卡）。
func HeatmapDayScore(cells []HeatmapCell, date string) (int, bool) {
	target := dateCSTDay(date)
	if target == "" {
		return 0, false
	}
	for _, c := range cells {
		if dateCSTDay(c.Date) == target {
			return c.Score, true
		}
	}
	return 0, false
}

// dateCSTDay 把上游给的日期串归一成 `2006-01-02`（取前 10 位）。
//
// 上游可能返回带时区的完整时间戳；短于 10 字符的视为无判据（返回空串）。
func dateCSTDay(raw string) string {
	if len(raw) < 10 {
		return ""
	}
	return raw[:10]
}
