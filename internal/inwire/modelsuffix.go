package inwire

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/dialect"
	"github.com/cn-maul/rosetta-gateway/internal/effort"
)

// ModelSuffix 是客户端写在 model 名字尾部的**单次请求**覆盖：
//
//	deepseek-chat@thinking:off
//	gpt-5@effort:high
//	glm-4.7@dialect:glm
//	claude-sonnet-4-6@thinking:8192
//
// # 为什么需要它
//
// 方言表是「这个上游模型平时怎么说话」的默认值，管理员在后台配一次就固定了。
// 但中转站的怪脾气是按模型、甚至按批次的：同一个渠道里今天这个模型不认
// `thinking`、明天那个模型只认 `enable_thinking`。为了试一次就进后台改配置，
// 既慢又容易把别的请求带偏。
//
// 后缀是这次请求的临时覆盖，不进库、不影响并发请求，并且会出现在日志和
// 计费身份里 —— 出问题时能看出「这一条是特意改了方言的」。
//
// # 为什么只认三个键
//
// 只解析**尾部连续**的 `@thinking:` / `@effort:` / `@dialect:`。别的 `@`
// 一律当成模型名的一部分：真实模型名里有 `@sha256:...`、`@2024-08` 这类
// 写法，宽松匹配会把它们吃掉，变成一个上游不认识的模型名。
//
// # 优先级
//
// 后缀 > 请求体字段。名字是调用方对这次请求最明确的声明，也是计费身份；
// 两者打架时按名字执行，并在数据面日志里留下记录。
type ModelSuffix struct {
	// Set 表示 model 里至少出现过一个可识别的后缀。
	Set bool
	// Raw 是剥下来的后缀原文（含前导 @），仅用于日志与诊断。
	Raw string

	// ThinkingOff 是 @thinking:off / @effort:none 的归一结果。
	ThinkingOff bool
	// ThinkingOn 表示后缀要求开启思考（含 @thinking:on 这种不带档位的写法）。
	ThinkingOn bool
	// Budget 是 @thinking:<正整数> 给出的 token 预算。
	Budget int
	// Adaptive 是 @thinking:adaptive。
	Adaptive bool
	// Effort 是归一后的档位名（low/medium/high/xhigh/max）。
	Effort string
	// Level 是 Effort（或预算折算出的档位）在统一强度刻度上的取值。
	// 关思考时为 None；只写 @thinking:on 时为 Unset（不指定强度）。
	Level effort.Level
	// Dialect 是 @dialect:xxx 的取值；空串表示没指定。
	Dialect string
}

// DialectOverride 把后缀里的方言转成数据面用的类型。
func (s ModelSuffix) DialectOverride() dialect.Dialect {
	if s.Dialect == "" {
		return dialect.Auto
	}
	return dialect.Dialect(s.Dialect)
}

// ParseModelSuffix 剥离 model 尾部的覆盖后缀，返回真正的模型名与覆盖内容。
//
// 值非法时返回 error，由调用方转成 400：静默忽略会让调用方以为覆盖生效了，
// 而实际发出去的是完全不同的字段 —— 这类「成功但没生效」比报错难查得多。
func ParseModelSuffix(model string) (string, ModelSuffix, error) {
	base := model
	var sfx ModelSuffix

	// 从右往左剥：只有**连续**的尾部片段才算后缀，
	// `org/model@2024-08@thinking:off` 里的 `@2024-08` 会终止解析。
	for {
		at := strings.LastIndexByte(base, '@')
		if at <= 0 {
			break
		}
		key, value, ok := splitSuffixSegment(base[at+1:])
		if !ok {
			break
		}

		// 同一个键出现多次时最右边的那次生效（与请求体字段的覆盖方向一致）。
		switch key {
		case "thinking":
			if sfx.ThinkingOff || sfx.ThinkingOn || sfx.Budget > 0 || sfx.Adaptive {
				break
			}
			if err := parseThinkingSuffix(&sfx, value); err != nil {
				return "", ModelSuffix{}, err
			}
			sfx.Set = true
		case "effort":
			if sfx.Effort != "" || sfx.ThinkingOff {
				break
			}
			level := effort.Parse(value)
			if level == effort.Unset {
				return "", ModelSuffix{}, fmt.Errorf(
					"model suffix @effort: %q is not a known reasoning level (use none|minimal|low|medium|high|xhigh|max)", value)
			}
			sfx.Set = true
			if level == effort.None {
				sfx.ThinkingOff = true
				sfx.ThinkingOn = false
				sfx.Level = effort.None
			} else {
				sfx.Effort = string(level)
				sfx.Level = level
				sfx.ThinkingOn = true
			}
		case "dialect":
			if sfx.Dialect != "" {
				break
			}
			d, ok := dialect.Parse(value)
			if !ok {
				return "", ModelSuffix{}, fmt.Errorf(
					"model suffix @dialect: %q is not a known dialect (%s)", value, dialectNames())
			}
			sfx.Set = true
			if d != dialect.Auto {
				sfx.Dialect = string(d)
			}
		}
		base = base[:at]
	}

	// 关思考是更强的意图：同时写了 @effort:high 与 @thinking:off 时按关处理，
	// 但保留 Effort 字段会让下游以为「既开又关」，所以在这里清掉。
	sfx.Raw = model[len(base):]
	if sfx.ThinkingOff {
		sfx.ThinkingOn = false
		sfx.Budget = 0
		sfx.Adaptive = false
		sfx.Effort = ""
		sfx.Level = effort.None
	}
	return base, sfx, nil
}

func splitSuffixSegment(seg string) (key, value string, ok bool) {
	colon := strings.IndexByte(seg, ':')
	if colon <= 0 {
		return "", "", false
	}
	key = strings.ToLower(seg[:colon])
	switch key {
	case "thinking", "effort", "dialect":
		return key, seg[colon+1:], true
	default:
		return "", "", false
	}
}

func parseThinkingSuffix(sfx *ModelSuffix, raw string) error {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "on", "true", "enabled", "yes":
		sfx.ThinkingOn = true
		return nil
	case "off", "false", "disabled", "no", "none":
		sfx.ThinkingOff = true
		return nil
	case "adaptive":
		sfx.Adaptive = true
		sfx.ThinkingOn = true
		return nil
	}
	budget, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf(
			"model suffix @thinking: %q is invalid (use on|off|adaptive or a positive token budget)", raw)
	}
	switch {
	case budget < 0:
		return fmt.Errorf("model suffix @thinking: budget must be >= 0, got %d", budget)
	case budget == 0:
		sfx.ThinkingOff = true
	default:
		sfx.Budget = budget
		sfx.Level = budgetLevel(budget)
		sfx.ThinkingOn = true
	}
	return nil
}

func dialectNames() string {
	names := make([]string, 0, len(dialect.All)+1)
	names = append(names, "auto")
	for _, d := range dialect.All {
		names = append(names, string(d))
	}
	return strings.Join(names, "|")
}

// budgetLevel 与 rosetta 的 effortFromBudget 同一分界：预算与档位不可逆
// 互转，但方向是确定的，跨协议时只能按这几个区间折算。
func budgetLevel(budget int) effort.Level {
	switch {
	case budget <= 0:
		return effort.None
	case budget <= 4096:
		return effort.Low
	case budget <= 16384:
		return effort.Medium
	default:
		return effort.High
	}
}

// anthropicBudgetFor 是档位 → Anthropic 预算，取值与 rosetta SDK 完全一致：
// 换一个数字会让同一次请求在跨协议时算出不同的思考量，而客户端并不知道
// 自己实际买到的是哪一档。
func anthropicBudgetFor(level effort.Level) int {
	switch level {
	case effort.Minimal, effort.Low:
		return 2048
	case effort.Medium:
		return 8192
	default:
		return 32768
	}
}
