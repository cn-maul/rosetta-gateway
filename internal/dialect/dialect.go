// Package dialect 定义「厂商思考方言」的唯一口径。
//
// # 为什么需要它
//
// 每个厂商表达「开/关思考、几档强度」的字段都不一样，而且**互不兼容**：
//
//	OpenAI      reasoning_effort: "none" | "low" | ... | "max"（7 档，无 thinking 字段）
//	Anthropic   thinking:{type:enabled, budget_tokens} / output_config.effort
//	DeepSeek    thinking:{type:enabled|disabled} + reasoning_effort: low|high|max
//	GLM         thinking:{type:enabled|disabled} + reasoning_effort: max|high|low
//	Qwen        enable_thinking: bool（不是 thinking 对象）
//	MiniMax     thinking:{type:adaptive|disabled} + reasoning_effort
//	Kimi K3     reasoning_effort: low|high|max，**始终思考、不能关**
//
// 把其中任何一家的写法无条件下发给另一家，轻则静默无效（钱照花、思考照跑），
// 重则直接 400（GLM-5.3 与 MiniMax-M3.1-Flash 都不接受 disabled）。
//
// 所以方言是**按模型**配置的事实，不是按协议：同一个中转站下可以同时卖
// GPT、DeepSeek 和 GLM，三家得各说各话。协议（chat/anthropic/responses）
// 只决定「字段放在哪一层」，方言决定「字段叫什么、取值是什么」。
package dialect

import (
	"strings"

	"github.com/cn-maul/rosetta-gateway/internal/effort"
)

// Dialect 是一个厂商的思考字段写法。
//
// 用字符串而不是枚举：它要原样出现在管理接口、配置导出文件和库表里，
// 而厂商集合还会增加 —— 枚举会让每加一家都要改一次类型与迁移。
type Dialect string

const (
	// Auto 表示「没手工指定」。它必须与「指定成某一家」可区分：前者允许
	// 网关按模型名推断，后者是管理员的明确决定，推断不能覆盖它。
	//
	// 用空串而不是 "auto"：库里这一列是可空的，NULL 与 "" 都自然落成
	// 空值，少一层「字符串 'auto' 和 SQL NULL 哪个才算自动」的换算。
	Auto Dialect = ""

	OpenAI    Dialect = "openai"
	Anthropic Dialect = "anthropic"
	DeepSeek  Dialect = "deepseek"
	GLM       Dialect = "glm"
	Qwen      Dialect = "qwen"
	MiniMax   Dialect = "minimax"
	Kimi      Dialect = "kimi"

	// Passthrough 表示「只发协议标准字段，绝不发明厂商私有字段」。
	//
	// 它是认不出方言时的兜底，也是管理员对「这个中转站乱改了字段」的
	// 显式选择。它保证的是**不把请求打挂**，而不是保证思考开关生效。
	Passthrough Dialect = "passthrough"
)

// All 是管理界面可选的方言（不含 Auto —— 界面上用「自动」单独表达）。
var All = []Dialect{OpenAI, Anthropic, DeepSeek, GLM, Qwen, MiniMax, Kimi, Passthrough}

// Label 返回给管理员看的名字。
func (d Dialect) Label() string {
	switch d {
	case Auto:
		return "自动"
	case OpenAI:
		return "OpenAI"
	case Anthropic:
		return "Anthropic"
	case DeepSeek:
		return "DeepSeek"
	case GLM:
		return "GLM（智谱）"
	case Qwen:
		return "Qwen（通义）"
	case MiniMax:
		return "MiniMax"
	case Kimi:
		return "Kimi（月之暗面）"
	case Passthrough:
		return "原样透传"
	default:
		return string(d)
	}
}

// Parse 校验一个外部传入的方言字符串；空串是合法的 Auto。
func Parse(s string) (Dialect, bool) {
	d := Dialect(strings.ToLower(strings.TrimSpace(s)))
	switch d {
	case Auto, Passthrough:
		return d, true
	}
	for _, known := range All {
		if d == known {
			return d, true
		}
	}
	return Auto, false
}

// Source 说明当前生效的方言是怎么来的。
type Source string

const (
	// SourceManual 是管理员在界面上明确选的。
	SourceManual Source = "manual"
	// SourceDetected 是按模型名推断的。
	SourceDetected Source = "detected"
	// SourceFallback 是既没配也推断不出来，退回原样透传。
	SourceFallback Source = "fallback"
)

// Resolve 把「库里的配置 + 模型名」解析成真正要用的方言。
//
// 手工配置永远优先：推断只是省一次点击，不能改变管理员的决定。
func Resolve(pin Dialect, modelID string) (Dialect, Source) {
	if pin != Auto {
		return pin, SourceManual
	}
	if d, ok := Detect(modelID); ok {
		return d, SourceDetected
	}
	return Passthrough, SourceFallback
}

// Detect 按模型名推断方言。
//
// # 为什么按名字而不是按端点
//
// 中转站普遍把多个厂商的模型挂在同一个 base URL 下，端点域名证明不了
// 里面是哪个模型；而模型名是上游真正认识的那个标识（网关转发时用的就是
// 它）。名字里带厂商前缀（`deepseek/deepseek-chat`）或直接是厂商命名
// （`glm-5.3`、`qwen3.8-max`）都能认出来。
//
// 认不出来时返回 false，由调用方退回 Passthrough —— 猜错的代价是把请求
// 打挂，不猜的代价只是开关没生效（且日志里看得见）。
func Detect(modelID string) (Dialect, bool) {
	name := strings.ToLower(strings.TrimSpace(modelID))
	// 去掉 `provider/` 前缀：中转站常用它标来源，但真正转发的是后半段。
	if i := strings.LastIndex(name, "/"); i >= 0 && i+1 < len(name) {
		name = name[i+1:]
	}
	if name == "" {
		return Auto, false
	}

	// 顺序有讲究：先认前缀最独特的，再认可能作为子串出现的。
	switch {
	case strings.Contains(name, "claude"):
		return Anthropic, true
	case strings.Contains(name, "deepseek"):
		return DeepSeek, true
	case strings.Contains(name, "glm"), strings.Contains(name, "zhipu"), strings.Contains(name, "chatglm"):
		return GLM, true
	case strings.Contains(name, "minimax"), strings.HasPrefix(name, "abab"):
		return MiniMax, true
	case strings.Contains(name, "kimi"), strings.Contains(name, "moonshot"):
		return Kimi, true
	case strings.Contains(name, "qwen"), strings.Contains(name, "qwq"):
		return Qwen, true
	case strings.Contains(name, "gpt"), strings.Contains(name, "chatgpt"):
		return OpenAI, true
	}
	// o 系列（o1/o3/o4-mini）没有厂商前缀，只有 o<数字> 这一种写法。
	if len(name) > 1 && name[0] == 'o' && name[1] >= '0' && name[1] <= '9' {
		return OpenAI, true
	}
	return Auto, false
}

// forcedThinkingNames 是**文档明确说不能关闭思考**的模型。
//
// 这些模型收到 disabled 不是「被忽略」，而是**直接 400**：
//   - GLM-5.3 / GLM-5.3-FLASH / GLM-5.3-FLASHX：「不再支持关闭思考
//     （API 请求中 thinking.type 传 disabled 将会报错）」
//   - MiniMax-M3.1-Flash：传 disabled 返回 HTTP 400，effort 传 none 也 400
//   - Kimi K3 系列：always enables thinking；K2.7-Code 固定开启
//   - DeepSeek R1 / reasoner 蒸馏模型：仅思考模式
//
// 命中这些模型时，客户的「关思考」只能降级成「用最低档思考」，并且
// 必须记一条日志 —— 否则用户以为关了，实际在按最高档烧钱。
func forcedThinking(modelID string) bool {
	name := strings.ToLower(strings.TrimSpace(modelID))
	if i := strings.LastIndex(name, "/"); i >= 0 && i+1 < len(name) {
		name = name[i+1:]
	}
	switch {
	case strings.Contains(name, "glm-5.3"):
		return true
	case strings.Contains(name, "m3.1-flash"):
		return true
	case strings.Contains(name, "kimi-k3"), strings.Contains(name, "k2.7-code"):
		return true
	case strings.Contains(name, "reasoner"), strings.Contains(name, "-r1"), strings.HasSuffix(name, "r1"):
		return true
	}
	return false
}

// CanDisable 报告该模型能否真正关闭思考。
//
// # 为什么需要一个 override 参数
//
// forcedThinking 是一张**按厂商文档**写死的表（GLM-5.3、MiniMax-M3.1-Flash、
// Kimi K3 不能关）。它描述的是「直连厂商端点」时的行为。
//
// 但中转站会做归一化：同一批模型挂在同一个 base URL 下，由中转自己把请求
// 翻译成各家协议。实测 opencode.ai 就是这样 —— 它的 glm-5.3 / kimi-k3.1-pro
// 直连时能接受 thinking:{type:disabled}，而网关按文档表把它们降级成「最低档」，
// 于是用户明明点了「关」，上游却一直在思考、一直在计费。
//
// 表是默认值，不是事实。override 就是管理员按实测结果纠正它的入口：
// nil = 按厂商文档表（默认），true/false = 以实测为准。
func CanDisable(d Dialect, modelID string, override *bool) bool {
	if override != nil {
		return *override
	}
	if d == Auto || d == Passthrough {
		return false // 不确定：不下发关闭，只记日志
	}
	return !forcedThinking(modelID)
}

// CannotDisableByDocs 报告厂商文档是否明确说了「这个模型不能关」。
//
// 与 CanDisable 的区别只在**方言未知**时有用：那时 CanDisable 一律返回 false
// （不冒险），但调用方仍需区分两种情况：
//   - 模型可能能关，只是方言没认出来 → 可以试一下标准写法；
//   - 文档明说传关闭就 400（GLM-5.3 / MiniMax-M3.1-Flash / Kimi K3）
//     → 绝不能试，试了就是把一个合法请求打成 400。
//
// 这个区分正是 passthrough「保证不把请求打挂」所需要的信息。
func CannotDisableByDocs(modelID string) bool {
	return forcedThinking(modelID)
}

// MinEffort 是「想关但关不掉」时能退到的最低档。
//
// 各家最低档不同：GLM-5.3 是 low，MiniMax-M3.1-Flash 是 low，Kimi K3 是 low。
// 统一用 low 是因为这几家文档里 low 都是合法且最省的一档。
func MinEffort(d Dialect, modelID string) effort.Level {
	return effort.Low
}

// WireEffort 把统一档位翻成该方言真正接受的取值。
//
// 第二个返回值是 false 时表示**这个方言没有强度字段**，调用方不应下发
// 任何强度参数（发了就是给上游塞一个它不认识的键）。
//
// override 与 CanDisable 同义：nil = 按厂商文档表，true/false = 以实测为准。
// 声称「能关」的模型收到 none 时必须原样往下走，不能再被 forced 分支改写成
// low —— 否则「能关」这个纠正只影响判断、不影响真正发出的字段。
func WireEffort(d Dialect, modelID string, override *bool, l effort.Level) (string, bool) {
	if l == effort.Unset {
		return "", false
	}
	// 只有「最终判定不能关」时才把 none 改写成最低档。管理员把它纠正成
	// 「能关」之后，none 必须原样发出去 —— 否则纠正只影响判断、不影响字节。
	if !CanDisable(d, modelID, override) && l == effort.None {
		l = MinEffort(d, modelID)
	}

	switch d {
	case OpenAI:
		// 官方 SDK 的 ReasoningEffort 就是这 7 个值，原样发。
		return string(l), l != effort.Unset

	case DeepSeek:
		// 文档：none 关闭，low/high/max 开启；minimal→low，
		// medium/xhigh→high（为兼容旧软件而接受，但真实档位只有三档）。
		switch l {
		case effort.None, effort.Low, effort.High, effort.Max:
			return string(l), true
		case effort.Minimal:
			return "low", true
		case effort.Medium, effort.XHigh:
			return "high", true
		}
		return "", false

	case GLM:
		// GLM-5.3 只认 max/high/low，其余值**报错**（不是映射）。
		// GLM-5.2 及以下认全 7 档并自行映射。
		if isGLM53(modelID) {
			switch l {
			case effort.None:
				// 5.3 的 effort 没有 none 这一档。管理员按实测把它纠正成
				// 「能关」后，关闭走 thinking:{type:"disabled"}，强度字段
				// 应当整个不下发 —— 不能在这里发明一个不存在的取值。
				return "", false
			case effort.Minimal, effort.Low:
				return "low", true
			case effort.Medium, effort.High:
				return "high", true
			case effort.XHigh, effort.Max:
				return "max", true
			}
			return "", false
		}
		return string(l), true

	case Qwen:
		// 百炼接受全部 7 档并按模型自行映射（qwen3.8 系列：max/high→xhigh，
		// minimal→low，none→enable_thinking=false）。
		return string(l), true

	case MiniMax:
		// M3.1-Flash 的 effort 只有 low/medium/high/xhigh/max，**没有 none**
		// （传 none 返回 400）。关闭只能靠 thinking.type，而它又不许关，
		// 所以 none 在上面的 forced 分支已被换成 low。
		switch l {
		case effort.None:
			return "", false
		case effort.Minimal, effort.Low:
			return "low", true
		}
		return string(l), true

	case Kimi:
		// K3 系列：low/high/max，其余值不在文档里。K2.x 只认 thinking 开关。
		if isKimiK3(modelID) {
			switch l {
			case effort.Minimal, effort.Low:
				return "low", true
			case effort.Medium, effort.High:
				return "high", true
			case effort.XHigh, effort.Max:
				return "max", true
			}
			return "", false
		}
		return "", false

	case Anthropic:
		// 真 Claude 的 Chat 兼容层没有 effort 字段；Anthropic 协议侧由
		// SDK 用 budget_tokens 表达，不走这里。
		return "", false

	default: // Auto, Passthrough
		return "", false
	}
}

func isGLM53(modelID string) bool {
	return strings.Contains(strings.ToLower(modelID), "glm-5.3")
}

func isKimiK3(modelID string) bool {
	name := strings.ToLower(modelID)
	return strings.Contains(name, "kimi-k3") || strings.Contains(name, "-k3")
}

// IsKimiK3 报告该模型是否属于 Kimi K3 系列。
//
// 官方 API 参考（platform.kimi.com/docs/api/chat）把 K3 与 K2.x 拆成了两个
// 请求 schema：K3 的 KimiK3ChatRequestBase 里**只有** reasoning_effort，
// 没有 thinking 字段；K2.x 的 ChatRequestBase 才有 thinking。所以给 K3 发
// thinking 不是「无效但被忽略」，而是请求体里多了一个该 schema 不认识的键。
// 客户端如果按 K2.x 的写法发了 thinking，网关必须替它去掉。
func IsKimiK3(modelID string) bool { return isKimiK3(modelID) }

// AcceptsReasoningEffort 报告该模型是否接受 reasoning_effort。
//
// 官方文档（platform.kimi.com/docs/guide/use-thinking-models）的对照表写得很
// 明确：K3 系列有 reasoning_effort、没有 thinking；K2.6 / K2.7-code 反过来，
// 只有 thinking、`reasoning_effort` 标为「不支持」。K2.7-code 还额外要求
// thinking.type 只能是 enabled。
func AcceptsReasoningEffort(d Dialect, modelID string) bool {
	return !(d == Kimi && !isKimiK3(modelID))
}
