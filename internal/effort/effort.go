// Package effort 定义「思考挡位」的唯一口径。
//
// # 为什么需要它
//
// 客户端（尤其是接入本网关的 agent 工具）会按模型文档选挡位，而各家的档位
// 集合并不相同：OpenAI 的 o 系列是 low/medium/high，GPT-5 一代加了 minimal 与
// xhigh，Anthropic 只有 budget_tokens 而没有 effort 概念。而 rosetta 的统一
// 抽象只认 low/medium/high 三档（见 rosetta.Effort）—— 客户端发 xhigh 会在
// inwire.parseEffort 里被压成 high，于是「选了最强档」这件事在链路上被静默
// 降级，且**没有任何一方能看见**：客户端不知道本网关支持什么，网关也不知道
// 这个模型真正支持什么。
//
// 这里给出三件东西：
//   - KnownLevels：各家在文档里出现过的挡位全集（供管理界面勾选）；
//   - Parse / Normalize：把任意写法归一到有序强度刻度上的某一档；
//   - Clamp：按「某个模型支持的子集」把请求的挡位夹到最近的一档。
//
// # 为什么是「夹取」而不是「丢弃」
//
// 丢弃（对不支持的挡位退回到上游默认）会让用户显式选择的强度凭空消失，
// 而且是无声的。夹取保留了「用户要更强/更弱思考」这个意图，在子集里取**最接近**
// 的一档，是唯一不撒谎也不浪费的处置 —— 它可能不是上游的原生档位，但结果落在
// 该模型真实支持的范围内。
package effort

import (
	"sort"
	"strings"
)

// Level 是一个挡位名（小写、无空白）。用字符串而不是 int 枚举：这些值要原样
// 出现在 API 响应与 /v1/models 里（agent 工具据此生成可选列表），而强度刻度
// 的成员集合会随各家模型演进而增补 —— 枚举会让每加一档都要改一次类型。
type Level string

// 强度刻度。**顺序即强度**，递增；夹取与比较全部依赖这个顺序。
//
// 这里的取值范围覆盖了目前已知的所有写法：OpenAI 的 minimal（比 low 更弱）、
// 常规 low/medium/high、GPT-5 系列的 xhigh（比 high 更强），以及部分国产
// 上游在 xhigh 之上再给一档 max（比 xhigh 更强）。默认档 Default
// 对应 rosetta 的 EffortMedium —— rosetta 在「请求思考但没给档位」时也取它
// （rosetta.ChatRequest.effort），保持同一口径。
//
// # 为什么 max 是独立档而不是 xhigh 的别名（2026-10-14）
//
// 原来 normalizeAlias 把 `max` 归到 XHigh。实测某个真实上游
// （/v1/models 返回 reasoning_supported_efforts = [low,medium,high,xhigh,max]
// 且 reasoning_default_effort = max）之后，这条别名有两个实际后果：
//
//  1. 配置写 `max` 存进库里会变成 `xhigh`，与上游的原始取值不再一致 ——
//     往返一次就丢掉了「这其实是最高那一档」这层信息，也让导出的配置
//     与上游对不上账。
//  2. 上游默认档是 `max`，客户端不指定等级时上游按 max 跑；而网关披露的
//     集合里只有 xhigh，工具以为最高就是 xhigh。
//
// 强度刻度上两者相邻，夹取行为几乎不变（要 max 而只有 xhigh 时仍夹到
// xhigh），但**披露与存储**保真了 —— 而这正是 effort_levels 存在的意义。
const (
	Unset   Level = ""
	None    Level = "none"
	Minimal Level = "minimal"
	Low     Level = "low"
	Medium  Level = "medium"
	High    Level = "high"
	XHigh   Level = "xhigh"
	Max     Level = "max"
)

// Default 是未指定时的默认档，与 rosetta 的 effort() 兜底一致。
const Default = Medium

// KnownLevels 是管理界面提供勾选的候选集合（按强度升序）。
//
// 刻意**不含** Unset 与 None：它们表达的是「不思考」而不是「某档思考强度」，
// 与本列的语义不同轴，混进来会让「这个模型不思考」和「这个模型思考很弱」在
// 界面上长得一样。
var KnownLevels = []Level{Minimal, Low, Medium, High, XHigh, Max}

// rankOf 返回档位在强度刻度上的位置；不在刻度上返回 -1。
func rankOf(l Level) int {
	for i, k := range KnownLevels {
		if k == l {
			return i
		}
	}
	return -1
}

// normalizeAlias 把各家的历史/别名写法映射到刻度上的规范档名。
//
// 需要别名表的原因：客户端的实际写法比文档更杂 —— 大小写混用、首尾空白、
// 以及把 high 拼成 xhigh 的手写（这两者在 GPT-5 出现之前是同一个意思）。
func normalizeAlias(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none", "off", "disabled":
		return None
	case "minimal", "min":
		return Minimal
	case "low":
		return Low
	case "medium", "mid", "default":
		return Medium
	case "high":
		return High
	case "xhigh", "x-high", "x_high", "extra_high", "very_high":
		return XHigh
	case "max", "maximum":
		// 曾经这里是 `return XHigh`（把 max 当 xhigh 的别名）。现在 Max 是
		// 独立档，见 KnownLevels 上面的说明。
		return Max
	default:
		return Unset
	}
}

// Parse 把任意写法解析为一个规范档名；无法识别时返回 Unset。
//
// 调用方据此决定「退回默认」还是「夹到最接近的一档」—— 两者语义不同：
// 未识别通常是拼写错误（该退回），而 Clamp 处理的是**已识别但不支持**。
func Parse(s string) Level { return normalizeAlias(s) }

// ParseLevels 解析一串逗号分隔的挡位，返回规范名与已知档位两部分。
//
// known 是结果真正可用于夹取的子集（去重、保序）；unknown 是原样保留的其它
// 非空写法 —— 它们必须**回传**给调用方，因为这些是上游可能认得的厂商私有值，
// 网关不理解不等于上游不理解，直接扔掉会让「配了但不生效」无从查起。
//
// 顺序按强度升序重排：CSV 是人手抄的，写 "high,low" 与 "low,high" 指的是同一
// 组挡位，不该让数据库里出现两种等价表示。
func ParseLevels(csv string) (known []Level, unknown []string) {
	seen := make(map[Level]bool)
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lv := normalizeAlias(part)
		if lv == Unset {
			if !contains(unknown, part) {
				unknown = append(unknown, part)
			}
			continue
		}
		if lv == None || seen[lv] {
			continue
		}
		seen[lv] = true
		known = append(known, lv)
	}
	sort.Slice(known, func(i, j int) bool { return rankOf(known[i]) < rankOf(known[j]) })
	return known, unknown
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// FormatLevels 把已知档位与未知原文拼回 CSV（供回写数据库）。
func FormatLevels(known []Level, unknown []string) string {
	parts := make([]string, 0, len(known)+len(unknown))
	for _, k := range known {
		parts = append(parts, string(k))
	}
	parts = append(parts, unknown...)
	return strings.Join(parts, ",")
}

// Clamp 把 wanted 夹到 supported 之内；supported 为空时原样返回 wanted。
//
// 取**绝对距离最近**的一档，同距时取**更强**的那一档。
//
// # 为什么同距向上（2026-10-17 改）
//
// 原实现同距取更弱，理由是「宁可少想一点安全」。但客户端显式选一个档位，
// 表达的意图是「我要这么强的思考」；夹到更弱等于把那个意图打折，而且是无声的
// —— 用户看到的是「我选了 high，怎么答得这么浅」，没有任何线索指向网关。
//
// 两个独立实现在这一点上一致：cc-switch 的 zen 模式只取「不小于请求的最近
// 合法档」，Magpie 的 NearestEffort 注释写得最直白 ——
// 「a tie going up — xhigh on a model of high and max is max」。
// 我们是被反驳的那一方，故改。
//
// 典型场景：模型支持 [low, high]，客户端要 medium。原实现给 low，
// 现在给 high —— 后者才是「要 medium 而 medium 没有」时更贴近的选择。
//
// # None 永远不作为候选
//
// rankOf(None) 是 -1（KnownLevels 刻意不含它，见那里的注释），而旧代码用
// rankOf(s) - w 再取绝对值，-1 会变成一个**很大的距离**参与竞争 ——
// 某些支持集下 None 会被选中，结果是「模型不思考」。
// None 表达的是「不思考」而不是「某档强度」，与 supported 不同轴。
//
// supported 为空返回 wanted 是刻意的 —— 「未配置」与「一个都不支持」语义不同：
// 前者意为「本网关不管这件事」，此时把客户端的值原样交回给 rosetta 的三档
// 归一，与本功能落地前的行为完全一致（零回归）。
func Clamp(wanted Level, supported []Level) Level {
	if len(supported) == 0 || wanted == Unset {
		return wanted
	}
	w := rankOf(wanted)
	if w < 0 {
		return wanted // 不认识的挡位：不猜，让上游自己决定
	}
	best, bestDist, found := Level(""), -1, false
	for _, s := range supported {
		r := rankOf(s)
		if r < 0 {
			continue // None 与一切未知写法：不当候选
		}
		d := r - w
		if d < 0 {
			d = -d
		}
		if !found || d < bestDist || (d == bestDist && r > rankOf(best)) {
			best, bestDist, found = s, d, true
		}
	}
	// 支持集里一个合法档都没有（全是 None 或未知值）→ 原样返回，
	// 与 supported 为空同口径：这不是「一个都不支持」，是「没告诉我们有用的信息」。
	if !found {
		return wanted
	}
	return best
}

// Supports 报告 supported 是否包含 wanted（用于日志与响应里的说明，不做夹取）。
//
// supported 为空返回 true，与 Clamp 同口径：空支持集意味着「未配置」，
// 而未配置是「网关不干预」，不是「这个值不合法」。两个函数对空集的判断必须
// 一致 —— 否则调用方会在「本不该夹取」的分支里打出一条并不存在的降级日志，
// 反而污染了真正需要留痕的那一条。
func Supports(wanted Level, supported []Level) bool {
	if wanted == Unset || len(supported) == 0 {
		return true
	}
	for _, s := range supported {
		if s == wanted {
			return true
		}
	}
	return false
}

// ToRosettaLevel 把挡位映射成 rosetta 唯一认识的 low/medium/high。
//
// 这是本包与 rosetta 的**唯一**接缝：rosetta 只认三档（其 validate 会拒其它值），
// 所以无论客户端要的是 minimal 还是 xhigh，最终都落到这三档之一。三档之外的
// 语义靠 Clamp 在**模型支持子集**这一层消化，而不是让 rosetta 去理解它。
//
// # High 必须显式列出（2026-10-14 修）
//
// 原来只写了 `case XHigh, High: return "high"`，High 是靠那一条命中的。
// 把 Max 加进刻度后如果只补 `case XHigh, Max:` 而漏掉 High，
// `High` 会掉进 default 分支拿到 "medium" —— 强度**静默降一档**，而且
// 没有任何错误。这类「靠相邻 case 顺带覆盖」的写法很脆：
// 每个刻度上的档位都必须有自己的 case，编译器管不了 switch 的完备性，
// 所以下面用 default 兜成 high（刻度上 medium 之上只剩 high/xhigh/max）。
func ToRosettaLevel(l Level) string {
	switch Clamp(l, KnownLevels) {
	case Minimal, Low:
		return "low"
	// XHigh 与 Max 都落到 rosetta 的 high：rosetta 只认三档，这个接缝处
	// 必然有损。真实档位靠 EffortRaw 原样透传（见 main.go 的
	// applyThinkingCapability），所以「客户端要 max、上游收 max」这条
	// 路径不受这里的折叠影响 —— 折叠只发生在 anthropic 那种没有 effort
	// 字段、必须用 token 预算表达的协议上。
	case High, XHigh, Max:
		return "high"
	default:
		return "medium"
	}
}

// ToThinkingLevel 把挡位映射成「要不要思考 + 用哪一档」。
//
// # 为什么需要它（2026-10-21 修 P0-12）
//
// inwire 曾自己维护一张 "原始写法 → rosetta 三档" 的 switch
// （inwire.parseEffort），而本包已经有一份更完整的别名表
// （normalizeAlias）。两张表对同一输入给出**相反**答案：
//
//	"max"       → inwire 判为 Unset（不设 Thinking = 完全不思考）
//	             → effort.Parse 判为 Max（刻度上最高档）
//	"very_high" → inwire 判为 Unset
//	             → effort.Parse 判为 XHigh
//	"min" / "mid" / "default" → inwire 判为 Unset
//	             → effort.Parse 判为 Minimal / Medium / Medium
//
// 后果不是「降一档」而是**把用户的显式意图整个抹掉**：网关在
// /v1/models 里向客户端披露了 max 这一档，转头却把客户端发的 max 丢成
// 「不思考」，且日志无痕。
//
// 与 ToRosettaLevel 的区别是**保留「不思考」这个语义**：
//   - None（off / disabled）→ enabled=false（显式不思考）；
//   - 其它已识别档位        → enabled=true + 三档之一；
//   - Unset（不认识）        → enabled=false（退回上游默认强度）。
//
// 之所以要有「None → 不思考」这一档：客户端显式说「这次别思考」，
// 网关把它当成「未知挡位」而退回上游默认强度，恰好与请求相反。
//
// 返回的 level 是 rosetta 认得的 low/medium/high 三档之一 —— 与
// ToRosettaLevel 同源（都走 Clamp 到 KnownLevels），只是这里额外
// 把「不思考」与「未知」区分开。
//
// ok=false 表示「不要设 Thinking」：调用方收到它时必须**不建** Thinking
// 配置（而不是建一个空的），否则 rosetta 会认为「请求思考但没给档位」
// 并按 EffortMedium 兜底 —— 那正是「客户端要 max 却变成 medium」的
// 另一种形态。
//
// 刻意不返回 rosetta.Effort：本包不 import rosetta（rosetta 是
// 外部依赖，而 internal/store 也依赖本包 —— 把 rosetta 拖进存储层
// 的依赖图没有好处）。调用方（inwire）做最后一次字符串 → 枚举的转换。
func ToThinkingLevel(l Level) (level string, ok bool) {
	switch l {
	case None:
		// 显式「不思考」：不映射成任何思考档位。
		return "", false
	case Minimal, Low:
		return "low", true
	case Medium:
		return "medium", true
	case High, XHigh, Max:
		return "high", true
	default:
		// Unset = 不认识的写法：退回上游默认强度（不干预）。
		return "", false
	}
}
