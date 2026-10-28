package gateway

// 时间窗模型条目（time-windowed model entry）。
//
// model_name_mapping 的 replacement 与 routing.modelCapabilities 的列表项
// 支持 "[HH-HH]model" 时间窗语法：当前小时落在窗内时该条目优先级最高，
// 裸条目（未配置时间窗的）次之，带窗但当前不命中的条目与裸条目同级、
// 按配置顺序参与排序。窗口按本地小时解释：[01-08] 覆盖 01:00:00–08:59:59；
// start > end 表示跨午夜（[22-06] 覆盖 22 点与次日 06 点前，含两端）。
// 一个模型可挂多个窗口（重复 "[HH-HH]" 前缀），任一窗口命中即生效。
//
// 示例（model_name_mapping）：
//
//	'agent-tester:[01-08]hy4-preview,[09-23]step-5-preview'
//	→ 1~8 点优先 hy4-preview，9~23 点优先 step-5-preview，其余小时
//	  无裸条目可退，按配置顺序取第一条（hy4-preview）。
//
//	'agent-reviewer:[01-08]hy4-preview, [18-08][12-14]deepseek-v4.1-flash'
//	→ 1~8 点优先 hy4-preview；18 点到次日 08 点与 12~14 点优先
//	  deepseek-v4.1-flash（两个窗口共享一个模型），其余小时无裸条目
//	  可退，按配置顺序取第一条（hy4-preview）。

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// timeWindowNowFunc 是可替换时钟，测试用它驱动时间切换（与
// dailyTierNowFunc 同一模式）。
var timeWindowNowFunc = time.Now

// timeWindowEntry 是一条带可选时间窗的模型条目。Windows 为空表示裸条目；
// 非空时每个元素是一个 [start,end] 窗口（小时 0-23，含两端，start > end
// 表示跨午夜），任一窗口命中当前小时即视为条目命中。
type timeWindowEntry struct {
	Windows [][2]int
	Model   string
}

// hasWindow 报告条目是否携带时间窗。
func (e timeWindowEntry) hasWindow() bool {
	return len(e.Windows) > 0
}

// timeWindowRe 匹配单个 "[HH-HH]" 前缀；小时允许 1~2 位数字
// （[1-8] == [01-08]）。多窗口条目由 parseTimeWindowEntry 循环剥前缀解析。
var timeWindowRe = regexp.MustCompile(`^\[(\d{1,2})-(\d{1,2})\]`)

// parseTimeWindowEntry 解析单条 "…[HH-HH]model" 或裸 "model"：任意个
// "[HH-HH]" 前缀组成窗口列表，剩余部分是模型名。窗口间允许空白。
func parseTimeWindowEntry(s string) (timeWindowEntry, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return timeWindowEntry{}, fmt.Errorf("empty model entry")
	}
	var windows [][2]int
	rest := s
	for {
		rest = strings.TrimLeft(rest, " \t")
		m := timeWindowRe.FindStringSubmatch(rest)
		if m == nil {
			break
		}
		start, err1 := strconv.Atoi(m[1])
		end, err2 := strconv.Atoi(m[2])
		if err1 != nil || err2 != nil {
			return timeWindowEntry{}, fmt.Errorf("invalid hour in %q", s)
		}
		if start > 23 || end > 23 {
			return timeWindowEntry{}, fmt.Errorf("hour out of range [00-23] in %q", s)
		}
		windows = append(windows, [2]int{start, end})
		rest = rest[len(m[0]):]
	}
	if len(windows) > 0 {
		model := strings.TrimSpace(rest)
		if model == "" {
			return timeWindowEntry{}, fmt.Errorf("missing model name in %q", s)
		}
		if strings.HasPrefix(model, "[") {
			return timeWindowEntry{}, fmt.Errorf("malformed time window in %q", s)
		}
		return timeWindowEntry{Windows: windows, Model: model}, nil
	}
	if strings.HasPrefix(s, "[") {
		return timeWindowEntry{}, fmt.Errorf("malformed time window in %q", s)
	}
	return timeWindowEntry{Model: s}, nil
}

// parseTimeWindowEntries 解析逗号分隔的条目列表（model_name_mapping 的
// replacement）。空列表报错；任一条格式错误整体报错，调用方（validate）
// 在配置保存时拦截。
func parseTimeWindowEntries(s string) ([]timeWindowEntry, error) {
	parts := strings.Split(s, ",")
	entries := make([]timeWindowEntry, 0, len(parts))
	for _, p := range parts {
		e, err := parseTimeWindowEntry(p)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no model entries in %q", s)
	}
	return entries, nil
}

// hourInTimeWindow 报告 hour（0-23）是否落在 [start,end] 窗内（含两端；
// start > end 表示跨午夜）。
func hourInTimeWindow(hour, start, end int) bool {
	if start <= end {
		return hour >= start && hour <= end
	}
	return hour >= start || hour <= end
}

// rank 返回条目在 now 时刻的优先级组：0 = 任一窗口命中（最高），
// 1 = 裸条目或全部窗口不命中（fallback，按配置顺序）。
func (e timeWindowEntry) rank(now time.Time) int {
	hour := now.Hour()
	for _, w := range e.Windows {
		if hourInTimeWindow(hour, w[0], w[1]) {
			return 0
		}
	}
	return 1
}

// pickTimeWindowModel 按时间优先级从条目列表选出目标模型：
//  1. 窗命中的条目，配置顺序取第一条（最高优先级）；
//  2. 无命中时取第一条裸条目（显式 fallback）；
//  3. 全部带窗且均不命中时取第一条（配置顺序当优先级）。
//
// entries 非空由调用方保证（validate 拒绝空列表）。
func pickTimeWindowModel(entries []timeWindowEntry, now time.Time) string {
	for _, e := range entries {
		if e.rank(now) == 0 {
			return e.Model
		}
	}
	for _, e := range entries {
		if !e.hasWindow() {
			return e.Model
		}
	}
	return entries[0].Model
}
