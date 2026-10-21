package inwire

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cn-maul/rosetta"

	"github.com/cn-maul/rosetta-gateway/internal/effort"
)

// OpenAI Responses 协议的入站解码（POST /v1/responses）。
//
// 覆盖面与取舍：
//   - input 收字符串与 item 数组两种形态；message 的 content 收字符串与
//     分块数组（input_text / output_text / input_image / input_file /
//     input_audio）。
//   - function_call / function_call_output item 映射为 rosetta 的工具调用与
//     工具结果；reasoning item 剥除（历史思考不属于对话内容）。
//   - instructions 映射为 System；developer/system 角色 item 映射为 RoleSystem。
//   - reasoning.effort 映射为 rosetta 的 ThinkingConfig（SDK 负责跨协议翻译）。
//   - 不支持的 item 类型（item_reference / web_search 等内建工具）**显式 400**，
//     不静默丢弃 —— 静默丢弃正是本网关反复修过的「无声吞内容」。
//   - tools 里的 namespace（Codex 的工具打包形态）在 ToRosetta 展平成平级
//     function，见 flattenNamespaceTools。
//   - previous_response_id / store：网关不托管会话状态（DESIGN §1 明确不做），
//     两者被忽略；Codex 等客户端在 store:false 下随请求带全量历史，天然兼容。
//   - text.format 映射到 openai-chat 上游的 response_format（json_object 直映、
//     json_schema 翻译外壳）；responses 上游经 Extra 原样回传；anthropic 上游
//     没有对应物 —— tool_choice 仍做等价翻译（any/tool），结构化输出这类
//     硬约束则由网关层拒绝（见 RequiresStructuredOutput 与 handleIngress）。

type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stream          bool            `json:"stream,omitempty"`

	Tools             []ResponsesTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Reasoning         *struct {
		Effort string `json:"effort,omitempty"`
	} `json:"reasoning,omitempty"`

	Store              *bool           `json:"store,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Text               json.RawMessage `json:"text,omitempty"` // {"format": {...}}

	// flattenedToolChoice 是 ToRosetta 展平 namespace 后**重写过的**
	// tool_choice（指向 namespace 内工具的会被换成展平后的名字）。
	//
	// 为什么存在请求结构体上而不是现算：tool_choice 的翻译发生在
	// ApplyUpstreamExtras（按目标协议），那是逐 attempt 调用、且此时已
	// 拿不到 tools 的原始形态。展平映射表只在 ToRosetta 里有意义，而
	// ToRosetta 同样是逐 attempt 调用的工厂 —— 两者在同一个 attempt 里
	// 先后执行，所以把结果记在结构体上传下去是唯一不重复计算的走法。
	//
	// nil = 未重写过，沿用 ToolChoice 原值。
	flattenedToolChoice json.RawMessage `json:"-"`

	// downgradedTools 记录被降级成 function 的工具：工具名 → 原生类型
	// （"custom" / "local_shell"）。出站据此把模型对它的调用还原成
	// custom_tool_call / local_shell_call，否则 Codex 收到的是一个
	// 它不认识的普通 function_call（参数还套着 JSON 外壳），执行器
	// 找不到对应的实现。
	//
	// 只记降级过的：普通 function 不进表，出站零开销地走原路径。
	//
	// 在 **decode** 阶段算好（而不是 ToRosetta 里）：出站的 sink 与
	// 非流式编码器在第一次 attempt 之前就被构造好，那一刻还没人调过
	// ToRosetta，于是表必须是 nil —— 而 nil 表会让所有降级工具以普通
	// function_call 的形态发出去，Codex 按 item.type 分派时找不到自己的
	// 执行器。这个映射只由 tools 的字面内容决定，与目标上游无关，因此
	// 「每个 attempt 重算」没有理由，decode 时算一次即可。
	downgradedTools map[string]string `json:"-"`

	// RawEffort 是 reasoning.effort 的原样取值（与 OpenAIChatRequest.RawEffort
	// 同义）：数据面要在 ToRosetta 之后按目标模型支持的档位做夹取，需要的是
	// 归一到三档**之前**的那一档。空串 = 请求没提 effort。
	RawEffort string `json:"-"`

	// metadata / service_tier / safety_identifier 等未建模字段由 encoding/json 忽略。
}

type ResponsesTool struct {
	Type        string          `json:"type"` // function | namespace（内建工具无法代理到任意上游）
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`

	// Tools 是 namespace 形态的内层工具表。namespace 本身不是可调用工具，
	// 它只是把一组工具打包、用检索代替全量声明；见 flattenNamespaceTools。
	Tools []ResponsesTool `json:"tools,omitempty"`

	// Format 是 custom 工具的语法声明（{"type":"text"} 等）。本网关把
	// custom 降级成「接受一个字符串参数的 function」，所以只记下它存在，
	// 不解释其内容 —— 未知形态也不该让请求失败，见 validateResponsesTool。
	Format json.RawMessage `json:"format,omitempty"`
}

type responsesItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`

	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"` // function_call_output

	ID string `json:"id,omitempty"` // item_reference

	// Input 是 custom_tool_call 的自由文本入参（与 function_call 的
	// arguments 字符串是同一个位置上的两种形态：前者是无 schema 的裸文本，
	// 后者是 JSON 字符串）。
	Input string `json:"input,omitempty"`

	// Action 是 local_shell_call 的调用动作（{"type":"exec","command":[…]}）。
	// 与 Input 分开建模：两者是不同 item 类型的不同字段，共用一个结构体
	// 时只能并列声明。
	Action json.RawMessage `json:"action,omitempty"`

	// reasoning item 的续接载体。EncryptedContent 是 OpenAI 要求原样回传的
	// 不透明载荷；Summary 是它在 Responses 上的可读文本。ID 是 item 自身的
	// id（`rs_…`），与 encrypted_content 共同构成跨轮可寻址的状态。
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	Summary          json.RawMessage `json:"summary,omitempty"`
}

type responsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
	// input_file / input_audio 的载荷。三者的 url 形态各异：
	//   file_url  / audio_url  —— 远端引用（http(s)）
	//   file_data / audio_data —— 内联 base64（可带 data: 前缀）
	//   file_id                —— provider 侧已上传的文件引用
	// 都不设为指针：空串就是「这一项没给」，与显式空值是同一回事。
	FileURL   string `json:"file_url"`
	FileData  string `json:"file_data"`
	FileID    string `json:"file_id"`
	Filename  string `json:"filename"`
	AudioURL  string `json:"audio_url"`
	AudioData string `json:"audio_data"`
	// format 是 input_audio 的容器格式（"wav" / "mp3"）。OpenAI 只认这两个。
	Format string `json:"format"`
}

// responsesSupportedItemTypes 是 input 数组里能代理到上游的 item 类型。
//
// 两类不在其中，各有明确理由：
//
//   - **服务端内建工具**（web_search_call / file_search_call /
//     code_interpreter_call …）：它们由 OpenAI **服务端**执行，结果不是
//     可转译的消息内容，任何第三方上游都无法承接。客户端应改用
//     function tool 自己实现。
//   - reasoning 之外的 thinking 变体（部分客户端的私有形态）：无法验证
//     续接语义，宁可明确失败也不猜。
//
// Codex 的一族在这里全部放行（local_shell_call / custom_tool_call …）：
// 它们的执行方始终是**客户端自己**，与 web_search 相反，第三方上游只需
// 看到「模型想调用某个工具」即可，转译成普通工具调用/结果即可承接。
var responsesSupportedItemTypes = map[string]bool{
	"":                     true, // type 缺失的裸 {role, content} 简写
	"message":              true,
	"function_call":        true,
	"function_call_output": true,
	"reasoning":            true,
	// Codex 的 local_shell：客户端执行 shell，网关转成 function_call /
	// function_call_output 走同一套。
	"local_shell_call":        true,
	"local_shell_call_output": true,
	// Codex 的 custom（自由文本工具，如 apply_patch / exec）。
	"custom_tool_call":        true,
	"custom_tool_call_output": true,
}

// validateResponsesInputItems 检查 input 数组里的 item 类型是否可代理。
//
// # 为什么报错而不是跳过
//
// 静默丢弃一个 item 会让上游看到一段**缺了内容**的对话，而模型对缺失
// 浑然不觉，照样接着往下答 —— 那比明确失败危险得多：客户端以为它的
// 搜索结果参与了推理，实际没有。
//
// # 为什么带上位置与可读文案
//
// 「input[3] 的 item 类型 web_search_call 不受支持」能直接指到请求里的
// 哪一个元素；而原先 rosetta 报的 unknown block type
// "responses_item:web_search_call" 是一个内部编码，客户端无从下手。
func validateResponsesInputItems(input json.RawMessage) error {
	var items []responsesItem
	if err := json.Unmarshal(input, &items); err != nil {
		// 不是数组（字符串简写或非法 JSON）—— 由调用方与 ToRosetta 各自处置。
		return nil
	}
	for i, item := range items {
		if !responsesSupportedItemTypes[item.Type] {
			return fmt.Errorf("input[%d]: 不支持的 item 类型 %q"+
				"（内建工具由服务端执行，无法转译给上游；请改用 function 工具自行实现）",
				i, item.Type)
		}
	}
	return nil
}

// validateResponsesTool 校验一条 tools 项，并可递归校验 namespace 的内层表。
//
// # 为什么放行 namespace
//
// Codex 等客户端会把工具**打包**成 namespace（`{"type":"namespace",
// "name":"shell","tools":[…]}`），用「先声明一个命名空间、再按需检索
// 展开」代替每轮全量声明工具表。这是 OpenAI 官方 Responses 后端的能力，
// 而任何只认 `type:"function"` 的第三方上游都没有对应概念 —— 网关必须
// 在入站展平成平级 function（见 flattenNamespaceTools）才能代理。
//
// 拒绝它（此前的行为）会让 Codex 完全无法使用本网关：不是某个功能缺失，
// 是整条接入路径 400。展平的代价（工具表变长）由前缀缓存吸收 ——
// 实测同一份工具表连续三次请求，4531 token 里 4530 命中缓存。
func validateResponsesTool(t ResponsesTool, path string) error {
	switch t.Type {
	case "function":
		if t.Name == "" {
			return fmt.Errorf("%s: function tool requires name", path)
		}
		return nil
	case "custom":
		// Codex 的自由文本工具（apply_patch / exec 等）。它没有 parameters，
		// 入参是一段原始文本，所以降级成「接受一个字符串参数的 function」。
		// 必须有名字：展平成 function 后名字就是模型唯一的调用凭据。
		if t.Name == "" {
			return fmt.Errorf("%s: custom tool requires name", path)
		}
		return nil
	case "local_shell":
		// Codex 的本地 shell 工具。同理降级成一个 function，参数是
		// {"command":["…"]}（exec 形态）与 timeout_ms。
		//
		// # 为什么放行
		//
		// 它与 web_search 不同：web_search 由 OpenAI **服务端**执行，第三方
		// 上游无法承接；而 local_shell 的执行方始终是客户端自己 —— 网关只需
		// 把「模型想跑 shell」这件事表达成上游认识的工具调用形状。此前把它
		// 当内建工具 400 掉，整条 Codex 接入路径就断了（用户实测：其他
		// 编程工具都能用，只有 Codex 起不来）。
		return nil
	case "namespace":
		// namespace 自身不可调用，但它需要名字：展平时用名字当前缀，
		// 没有名字就无法保证展平后的工具名唯一可追溯。
		if t.Name == "" {
			return fmt.Errorf("%s: namespace tool requires name", path)
		}
		if len(t.Tools) == 0 {
			return fmt.Errorf("%s: namespace %q has no tools", path, t.Name)
		}
		for i, inner := range t.Tools {
			// 嵌套 namespace 不放行：展平是一次性的，两层嵌套在真实
			// 客户端里不出现，而递归支持它会让前缀拼接规则变得不可预测。
			if inner.Type == "namespace" {
				return fmt.Errorf("%s.tools[%d]: nested namespace is not supported", path, i)
			}
			if err := validateResponsesTool(inner, fmt.Sprintf("%s.tools[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%s: unsupported tool type %q "+
			"(only \"function\" and \"namespace\" can be proxied to upstreams)",
			path, t.Type)
	}
}

// DecodeResponsesRequest 解析 /v1/responses 请求体。超限以 *http.MaxBytesError
// 报出（调用方映射 413），与其他入口同一套纪律。
func DecodeResponsesRequest(r *http.Request, maxBytes int64) (*ResponsesRequest, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, &http.MaxBytesError{Limit: maxBytes}
	}

	var req ResponsesRequest
	if err := decodeTopLevelObject(body, &req); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if len(req.Input) == 0 {
		return nil, fmt.Errorf("input is required")
	}
	if req.Reasoning != nil {
		req.RawEffort = strings.TrimSpace(req.Reasoning.Effort)
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens <= 0 {
		return nil, fmt.Errorf("max_output_tokens: must be a positive integer")
	}
	// input 里的 item 类型白名单。放在 decode 而不是 ToRosetta：
	// ToRosetta 是逐 attempt 调用的工厂（见 main.go 的 buildRosetta），
	// 而类型不支持是**请求本身**的问题，第一次就该报，不该等走到某个
	// 上游才由 rosetta validate 说出来。
	//
	// 顺带解决可读性：原先这类 item 被转成 BlockType("responses_item:xxx")，
	// 最终由 rosetta 报 "unknown block type \"responses_item:web_search_call\"" ——
	// 客户端看到的是一个内部编码，既不知道那是什么、也不知道该改什么。
	if err := validateResponsesInputItems(req.Input); err != nil {
		return nil, err
	}
	for _, t := range req.Tools {
		if err := validateResponsesTool(t, "tools"); err != nil {
			return nil, err
		}
	}
	// 降级表在这里就建好（而不是 ToRosetta 里）：出站的 sink 与非流式编码器
	// 在第一次 attempt 之前构造，那一刻还没人调过 ToRosetta。
	req.downgradedTools = responsesDowngradedTools(req.Tools)
	if len(req.ToolChoice) > 0 {
		var s string
		if json.Unmarshal(req.ToolChoice, &s) != nil {
			var obj struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(req.ToolChoice, &obj) != nil {
				return nil, fmt.Errorf("tool_choice: must be a string or {type,name} object")
			}
			// namespace 也接受：客户端可以用命名空间强制「在这个组里选」，
			// 展平后这个约束由 rewriteToolChoice 兑现（组内唯一的匹配会被
			// 改写成展平名）。多于一个匹配时保持原值发出，不猜。
			if (obj.Type == "function" || obj.Type == "namespace") && obj.Name == "" {
				return nil, fmt.Errorf("tool_choice: type=%s requires name", obj.Type)
			}
		}
	}

	return &req, nil
}

// flattenNamespaceTools 把 namespace 形态的工具表展平成平级 function，
// 并把 Codex 的两种内建调用工具降级成上游认识的 function。
//
// # 展平规则
//
// namespace 里的每个 function 变成 `父名__原名`（如 `shell__ls`）：
//
//   - **必须加前缀**：namespace 的意义是分组，而分组名对上游没有意义。
//     展平后两个不同 namespace 里可能有同名工具（`shell__ls` 与
//     `git__ls`），不加前缀它们会在上游的 tools 表里撞名，模型看到两个
//     同名不同定义的 function，调用哪个全靠猜。
//   - **前缀用 `__` 而不是 `.`**：部分上游（含 Anthropic）对工具名有字符
//     限制，`.` 在某些实现里被当作路径分隔符；`__` 在 MCP 生态里是既有的
//     命名空间惯例，客户端看到展平后的名字也能认出来源。
//   - **不重写 description**：客户端为每个工具写的 description 已经完整，
//     再拼上命名空间前缀只会挤占模型的注意力预算。
//
// # 为什么不试图保留分组语义
//
// 上游协议里没有「工具组」这个概念，保留它需要把分组编码进工具名或
// description —— 两者都是在用模型的理解力去模拟一个协议层结构。
// 展平后模型看到的是一份平铺的、可直接调用的工具表，这正是上游能理解的
// 唯一形态。
//
// # 代价
//
// 工具声明变长（实测每个工具约 145 token）。这个代价由上游的前缀缓存
// 吸收：工具表每轮字节相同，实测 30 工具 / 4531 token 的请求连续三次，
// 每次 4530 token 命中缓存。没有缓存的上游才会真付这笔钱。
func flattenNamespaceTools(in []ResponsesTool) []rosetta.ToolDefinition {
	tools := make([]rosetta.ToolDefinition, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, t := range in {
		if t.Type != "namespace" {
			def, name := responsesPlainTool(t)
			tools = append(tools, def)
			seen[name] = true
			continue
		}
		for _, inner := range t.Tools {
			name := t.Name + "__" + inner.Name
			// 撞名不静默覆盖：两个 namespace 展平出同名工具时，
			// 保留先出现的会让后一个工具**凭空消失**（模型永远调不到它），
			// 而这正是本网关反复修的「无声吞内容」。
			if seen[name] {
				// 展平后重复：要么是客户端给的重名，要么是两个 namespace
				// 恰好产生了同名。前缀规则已经保证了后者不会发生。
				tools = append(tools, responsesToolDefinition(name, inner.Description, inner.Parameters))
				continue
			}
			seen[name] = true
			tools = append(tools, responsesToolDefinition(name, inner.Description, inner.Parameters))
		}
	}
	return tools
}

// responsesPlainTool 把一条非 namespace 的工具项变成 function 定义，
// 并返回它在工具表里的最终名字。
//
// 三种情形：
//
//   - function：原样通过，名字就是客户端给的名字。
//   - custom（Codex 的自由文本工具，如 apply_patch）：降级成一个接受
//     **单个字符串参数** 的 function，参数名固定为 "input"。这是
//     OpenAI 自己描述 freeform 工具的等价形态，也是 new-api 等网关的
//     通行做法：上游协议里没有「无 schema 的自由文本参数」这个概念，
//     把它包装成单字符串参数是唯一既不失真又能被模型调用的表达。
//   - local_shell（Codex 的本地 shell）：降级成一个接受
//     {"command": ["…"], "timeout_ms": 毫秒} 的 function。
//
// # 为什么不静默丢弃这些工具
//
// 丢弃的后果不是「少一个功能」，而是模型**看不到 shell 工具**却仍被要求
// 干活，于是改用文本解释「你应该运行 ls」——用户看到的是一台不会用工具的
// 模型。降级有损（自由文本被套上 JSON 外壳），但对话仍然成立。
func responsesPlainTool(t ResponsesTool) (rosetta.ToolDefinition, string) {
	switch t.Type {
	case "custom":
		desc := t.Description
		if desc == "" {
			desc = "Send a freeform text input."
		}
		// 说明参数形态。客户端写的 description 面向的是懂它的模型，
		// 而降级后的参数名 "input" 是本网关起的名字，不说的话模型只能靠猜。
		desc += " Provide the raw text as the value of the \"input\" argument."
		return responsesToolDefinition(t.Name, desc, customToolParameters()), t.Name

	case "local_shell":
		// Codex 用这个名字声明它的 shell 工具，展平后必须沿用：
		// 客户端按名字把自己的工具实现与模型的调用对上。
		return responsesToolDefinition(localShellToolName,
			"Run a shell command on the client and return its output. "+
				"Provide the full command line as a single-element \"command\" array.",
			localShellToolParameters()), localShellToolName

	default:
		return responsesToolDefinition(t.Name, t.Description, t.Parameters), t.Name
	}
}

// customToolParameters 是 custom 工具降级后的参数 schema：单个字符串。
func customToolParameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"input":{"type":"string","description":` +
		`"The raw freeform input for this tool."}},"required":["input"]}`)
}

// localShellToolParameters 是 local_shell 降级后的参数 schema。
//
// command 用数组而非字符串：Codex 自己的 local_shell_call 用
// {"type":"exec","command":["ls","-la"]} 的数组形态，参数 schema 与实际
// 调用形态对齐，模型少一次形态转换，出的 JSON 也更稳。
func localShellToolParameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"array",` +
		`"items":{"type":"string"},"description":"The command and its arguments."},` +
		`"timeout_ms":{"type":"number","description":"Optional timeout in milliseconds."}},` +
		`"required":["command"]}`)
}

// localShellToolName 是 local_shell 降级后在工具表里的名字。
//
// 必须与客户端自己声明的 shell 工具名一致才有意义 —— 但客户端并没有给
// local_shell 起名字（它是 Responses 的内建类型，{"type":"local_shell"}
// 就是全部声明）。所以名字由本网关定；Codex 侧按响应里的 name 找到自己的
// shell 实现。
const localShellToolName = "local_shell"

// customToolInputArg 是 custom 工具降级后的参数名。写成一个常量是因为
// schema 与历史回放两处必须用同一个值。
const customToolInputArg = "input"

// responsesLocalShellArgs 把 local_shell_call 的 action 对象转成降级工具
// 的参数 JSON。
//
// action 的形态是 {"type":"exec","command":["ls","-la"],"timeout_ms":5000}，
// 与降级后 schema 的属性名一一对应，所以只需剥掉 "type" 判别字段。
// 解析不出来时返回 "{}"：一个空参数的 tool_call 仍是合法历史（模型能看到
// 「这里调过 shell」），而空字符串会让 rosetta validate 拒掉整个请求。
func responsesLocalShellArgs(action json.RawMessage) string {
	if len(action) == 0 {
		return "{}"
	}
	var obj map[string]any
	if json.Unmarshal(action, &obj) != nil {
		return "{}"
	}
	delete(obj, "type")
	if len(obj) == 0 {
		return "{}"
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// responsesCustomToolArgs 把 custom_tool_call 的裸文本包成降级工具的参数。
//
// 与 responsesLocalShellArgs 同一个道理：降级后的 schema 声明了
// {"input": "…"}，历史回放就必须用同一形状，否则模型看到上一轮传的是
// 字符串、这一轮工具要对象。
func responsesCustomToolArgs(input string) string {
	if input == "" {
		return "{}"
	}
	out, err := json.Marshal(map[string]any{customToolInputArg: input})
	if err != nil {
		return "{}"
	}
	return string(out)
}

func responsesToolDefinition(name, description string, params json.RawMessage) rosetta.ToolDefinition {
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	return rosetta.ToolDefinition{Name: name, Description: description, Parameters: params}
}

// responsesDowngradedTools 收集被降级的工具名与其原生类型。
//
// 与 flattenNamespaceTools 走同一份输入、同一套命名规则，但只关心
// 「哪些工具不是原生 function」。namespace 内的项已是 function，不进表。
func responsesDowngradedTools(in []ResponsesTool) map[string]string {
	var out map[string]string
	for _, t := range in {
		if t.Type == "custom" || t.Type == "local_shell" {
			if out == nil {
				out = make(map[string]string, len(in))
			}
			// 键必须是**展平后的名字**，与工具表里的名字一致 —— 出站按
			// 模型返回的 name 反查这张表。local_shell 的声明里没有 name
			// （{"type":"local_shell"} 就是全部），展平时才定下
			// localShellToolName；用 t.Name 会把映射落到空串键上，
			// 出站查不到，调用就以普通 function_call 的形态发出去。
			_, name := responsesPlainTool(t)
			out[name] = t.Type
		}
	}
	return out
}

// DowngradedTools 返回被降级过的工具表（工具名 → 原生 Responses 类型
// "custom" / "local_shell"），供出站还原 item 形态。
//
// 返回内部 map 而非副本：调用方（codec.Decode）只读一次填进
// ingressRequest，之后不再改。
func (a *ResponsesRequest) DowngradedTools() map[string]string {
	return a.downgradedTools
}

// rewriteToolChoice 在 namespace 展平后重写 tool_choice。
//
// # 为什么需要
//
// 客户端可以指定 `{"type":"function","name":"ls"}` 强制调用某个具体工具。
// 当这个 `ls` 原本住在 namespace `shell` 里时，展平后的名字是 `shell__ls`
// —— 不改的话，上游收到「强制调用 `ls`」而工具表里只有 `shell__ls`，
// 要么 400 要么被静默忽略（退回 auto）。后者更糟：客户端要的是强制，
// 实际拿到的是模型的自由选择，且没有任何信号。
//
// # 匹配规则
//
// 只有「展平后表里恰好存在 `父名__原名`」才改名。这既处理了命名空间内的
// 工具，也保证顶层平铺工具（本来就没被改名）原样通过 —— 后者是绝大多数
// 请求，不该为它们做任何额外判断。
//
// 返回 nil 表示不需要重写，调用方沿用原值。
func (a *ResponsesRequest) rewriteToolChoice(tools []ResponsesTool, flat []rosetta.ToolDefinition) json.RawMessage {
	if len(a.ToolChoice) == 0 {
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(a.ToolChoice, &obj) != nil || obj.Name == "" {
		return nil // 字符串形态或非 function 选择，与展平无关
	}
	if obj.Type == "namespace" {
		return a.rewriteNamespaceToolChoice(obj.Name, flat)
	}
	if obj.Type != "function" {
		return nil
	}
	exists := false
	for _, t := range flat {
		if t.Name == obj.Name {
			exists = true
			break
		}
	}
	if exists {
		return nil // 原名就在展平后的表里，无需改写
	}
	// 找 `任意命名空间__原名` 的唯一匹配。
	var match string
	for _, ns := range tools {
		if ns.Type != "namespace" {
			continue
		}
		cand := ns.Name + "__" + obj.Name
		for _, t := range flat {
			if t.Name != cand {
				continue
			}
			if match != "" && match != cand {
				// 两个命名空间都有同名工具：客户端的原名在展平后有歧义。
				// 不猜 —— 返回 nil 让 tool_choice 原样发出，上游会按它
				// 自己的规则处理（多半拒绝），比静默挑一个安全。
				return nil
			}
			match = cand
		}
	}
	if match == "" {
		return nil
	}
	out, err := json.Marshal(map[string]any{"type": "function", "name": match})
	if err != nil {
		return nil
	}
	return out
}

// rewriteNamespaceToolChoice 兑现 `{"type":"namespace","name":"shell"}`
// 形态的 tool_choice（2026-10-21 修 A2-P1-6）。
//
// # 为什么要单独处理
//
// decode 阶段**明确接受** namespace 形态（见 ToolChoice 字段的注释：
// 「namespace 也接受：客户端可以用命名空间强制『在这个组里选』，
// 展平后这个约束由 rewriteToolChoice 兑现」）。但修复前 rewriteToolChoice
// 只处理 `obj.Type == "function"`，namespace 落进 `return nil` ——
// **约束从未被兑现**，原始 `{"type":"namespace","name":"shell"}` 被原样
// 发给上游，而上游的工具表里只有展平后的 `shell__ls`，没有名为 `shell`
// 的工具。
//
// 结果要么 400，要么**静默退回 auto** —— 后者正是 DESIGN §7.3 明令要避免的
// 「客户端要强制，实际拿到模型自由选择，且无任何信号」。
//
// # 兑现规则
//
// 在展平后的表里找所有 `ns.Name + "__"` 前缀的候选：
//   - 恰好一个 → 改写成 `{"type":"function","name":"<展平名>"}`；
//   - 多于一个 → 保持原值（歧义不猜，多半会被上游拒绝）；
//   - 一个都没有 → 保持原值（约束无对应物，原样发出让上游自己决定）。
func (a *ResponsesRequest) rewriteNamespaceToolChoice(nsName string, flat []rosetta.ToolDefinition) json.RawMessage {
	prefix := nsName + "__"
	var match string
	for _, t := range flat {
		if !strings.HasPrefix(t.Name, prefix) {
			continue
		}
		if match != "" && match != t.Name {
			// 命名空间内不止一个工具：无法确定「强制哪一个」，
			// 不猜 —— 原样发出。
			return nil
		}
		match = t.Name
	}
	if match == "" {
		return nil
	}
	out, err := json.Marshal(map[string]any{"type": "function", "name": match})
	if err != nil {
		return nil
	}
	return out
}

// ToRosetta 组装统一请求。input 是字符串时整体作为一条 user 消息。
func (a *ResponsesRequest) ToRosetta() *rosetta.ChatRequest {
	req := &rosetta.ChatRequest{
		Model:       a.Model,
		Temperature: a.Temperature,
		TopP:        a.TopP,
		System:      a.Instructions,
	}
	if a.MaxOutputTokens != nil {
		req.MaxOutputTokens = *a.MaxOutputTokens
	}
	if len(a.Tools) > 0 {
		req.Tools = flattenNamespaceTools(a.Tools)
		// namespace 展平后，指向内层工具的 tool_choice 必须跟着改名，
		// 否则上游会去找一个不存在的工具名（或干脆拒绝）。
		a.flattenedToolChoice = a.rewriteToolChoice(a.Tools, req.Tools)
	}
	// minimal / xhigh / max 是 OpenAI 为 gpt-5 系列新增的档位（比 low 更省、
	// 比 high 更强），直连上游时合法。旧实现只认 low/medium/high，这些档位
	// 落空 → req.Thinking 保持 nil → 客户端以为自己开了推理模式，实际静默
	// 不思考。归一口径由 internal/effort 单一提供（2026-10-21 修 P0-12）。
	if a.Reasoning != nil && a.Reasoning.Effort != "" {
		if lvl, ok := effort.ToThinkingLevel(effort.Parse(a.Reasoning.Effort)); ok {
			req.Thinking = &rosetta.ThinkingConfig{Effort: rosetta.Effort(lvl)}
		}
	}

	// 字符串形态：整段就是一条 user 消息。
	var s string
	if err := json.Unmarshal(a.Input, &s); err == nil {
		req.Messages = append(req.Messages, rosetta.User(s))
		return req
	}

	var items []responsesItem
	if err := json.Unmarshal(a.Input, &items); err != nil {
		// 非法 input 在 decode 侧就该报（这里补一道：ToRosetta 可被逐 attempt 调用）。
		return req
	}
	for _, item := range items {
		switch item.Type {
		case "", "message":
			req.Messages = append(req.Messages, convertResponsesMessage(item)...)
		case "function_call":
			args := item.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			req.Messages = append(req.Messages, rosetta.Message{
				Role:   rosetta.RoleAssistant,
				Blocks: []rosetta.Block{rosetta.ToolCall(item.CallID, item.Name, args)},
			})
		case "local_shell_call":
			// Codex 让模型直接发起 shell 调用。与 function_call 的区别只在
			// 入参形态：local_shell_call 用 {"type":"exec","command":["…"]}
			// 的 action 对象，而不是 {"command":[…]}。展平成与降级后的
			// local_shell 工具（localShellToolParameters）完全一致的参数形状，
			// 模型下一轮看到自己的工具 schema 与历史调用就能对上。
			req.Messages = append(req.Messages, rosetta.Message{
				Role:   rosetta.RoleAssistant,
				Blocks: []rosetta.Block{rosetta.ToolCall(item.CallID, localShellToolName, responsesLocalShellArgs(item.Action))},
			})
		case "custom_tool_call":
			// custom 工具降级成「接受一个 input 字符串参数的 function」，
			// 所以回放历史时要把裸文本包成同一个外壳，否则上一轮模型看到
			// 的是 freeform 文本、这一轮工具 schema 却要一个对象。
			req.Messages = append(req.Messages, rosetta.Message{
				Role: rosetta.RoleAssistant,
				Blocks: []rosetta.Block{rosetta.ToolCall(item.CallID, item.Name,
					responsesCustomToolArgs(item.Input))},
			})
		case "local_shell_call_output", "custom_tool_call_output":
			// 与 function_call_output 同一条路径：客户端执行完工具，把结果
			// 喂回来。三者的区别只在 output 的内部形态（local_shell 是
			// {"stdout":…} 对象、custom 是裸文本），而 responsesItemToolOutput
			// 对字符串与分块数组都处理，结构对象交给 JSON 兜底。
			text, media := responsesItemToolOutput(item)
			if text != "" || len(media) == 0 {
				req.Messages = append(req.Messages, rosetta.ToolResult(item.CallID, "", text))
			}
			if len(media) > 0 {
				blocks := append([]rosetta.Block{{
					Type: rosetta.BlockText,
					Text: responsesToolMediaNotice(len(media)),
				}}, media...)
				req.Messages = append(req.Messages, rosetta.Message{
					Role: rosetta.RoleUser, Blocks: blocks,
				})
			}

		case "function_call_output":
			// 工具结果可能带**媒体块**（截图、PDF、音频）—— agent 的典型用法：
			// 让模型读一张图、读一份文档、听一段录音。
			//
			// 修复前只读 output_text，媒体块被静默丢弃。后果与 sealed reasoning
			// 同类：客户端以为那些内容参与了推理，实际没有，且没有任何一方能察觉。
			//
			// 媒体块不能塞进 tool_result —— 那个块的 Content 是纯文本语义
			// （rosetta.ToolResult 只接受字符串），硬塞会让上游解不出结构。
			// 所以拆成紧随其后的一条 user 消息，并在文本里留一行说明它被搬去了哪。
			// 这与 cc-switch 的 TOOL_RESULT_MEDIA_MOVED_MARKER 同一思路：
			// 不丢内容，也不假装它还在原来的位置。
			text, media := responsesItemToolOutput(item)
			if text != "" || len(media) == 0 {
				req.Messages = append(req.Messages, rosetta.ToolResult(item.CallID, "", text))
			}
			if len(media) > 0 {
				blocks := append([]rosetta.Block{{
					Type: rosetta.BlockText,
					Text: responsesToolMediaNotice(len(media)),
				}}, media...)
				req.Messages = append(req.Messages, rosetta.Message{
					Role: rosetta.RoleUser, Blocks: blocks,
				})
			}
		case "reasoning":
			// reasoning item 是 reasoning 会话的续接载体，不能剥除。
			//
			// 修复前这里按「历史思考不是对话内容」整个丢弃，与 OpenAI chat
			// 适配器对 thinking 块的处置看齐。但 Responses 协议下丢的不只是
			// 可读文本，还有 encrypted_content —— 那是 OpenAI 要求原样回传的
			// 状态，丢了它下一轮回放会报 "Item with id 'rs_…' not found"，
			// 客户端多轮 reasoning 会话直接断在这里。
			//
			// 现在原样搬进统一模型的 Sealed/SealedBy，由 rosetta 的
			// SealedBy 门闩决定写回哪个协议族：openai-responses 上游完整
			// 回放，chat / anthropic 上游自动剥除（那两个协议没有对应概念，
			// 客户端本应自带全量历史）。
			//
			// 边界：两者都空时整项跳过。空 summary（`"summary":[]`）是
			// 常见形态，此时 reasoning item 不带任何可搬运的状态，
			// 硬造一个 thinking block 只会被 rosetta validate 以
			// "thinking block is empty" 拒掉，把本来能跑的请求变成 400。
			summary := responsesItemReasoningText(item)
			if item.EncryptedContent == "" && summary == "" {
				continue
			}
			// ReasoningID 单独设：SealedThinking 只填 summary/sealed/sealedBy，
			// 而 item id 是下一轮寻址的依据（SDK 的 Responses 适配器按
			// Block.ReasoningID 写出 "id"），漏了它等于带着载荷却没有地址。
			blk := rosetta.SealedThinking(summary, item.EncryptedContent, "openai-responses")
			blk.ReasoningID = item.ID
			req.Messages = append(req.Messages, rosetta.Message{
				Role:   rosetta.RoleAssistant,
				Blocks: []rosetta.Block{blk},
			})
		default:
			// 兜底分支。正常路径下走不到这里 —— decode 阶段的
			// validateResponsesInputItems 已经用带位置与原因的错误拦掉了
			// （见那里的注释）。留着它是因为 ToRosetta 是逐 attempt 的工厂，
			// 不能假设 input 一定经过过 validate（测试与内部调用可能直接构造）。
			//
			// 仍然不静默吞：留一个可识别的空块让 rosetta validate 拒绝。
			req.Messages = append(req.Messages, rosetta.Message{
				Role:   rosetta.RoleUser,
				Blocks: []rosetta.Block{{Type: rosetta.BlockType("responses_item:" + item.Type)}},
			})
		}
	}
	return req
}

func convertResponsesMessage(item responsesItem) []rosetta.Message {
	role := item.Role
	switch role {
	case "system", "developer":
		return []rosetta.Message{rosetta.System(responsesContentText(item.Content, false))}
	case "assistant":
		// assistant 的正文在 output_text 分块里。
		return []rosetta.Message{rosetta.Assistant(responsesContentText(item.Content, true))}
	case "user":
		// 显式 user：只认 input_text。output_text 是 assistant 侧的标记，
		// 混进来等于把模型自己的输出当成用户说的话。
		return userMessageFromContent(item.Content)
	default: // role 缺失 / 未知角色
		// 拿不准时**只**采信 input_text。
		//
		// 修复前这一支把 input_text 与 output_text 并集，于是「role 缺失
		// 的 message 项」里的模型输出被当成人话发给上游。实测：
		//   输入 role:user{input_text:"QUESTION"} + role 缺失{output_text:"SECRET-ASSISTANT"}
		//   -> 上游收到 messages[1] = {role:"user", content:"SECRET-ASSISTANT"}
		// 而同一请求里显式写 role:"assistant" 时，输出落在 assistant 轮。
		// 丢弃 output_text 是更安全的错法：少一段历史，而不是把模型的
		// 上一句回答伪装成用户的新指令（那会真的改变它的行为）。
		return userMessageFromContent(item.Content)
	}
}

// userMessageFromContent 把一个 content 变成单条 user 消息。
//
// 文本分块收成一段，图片分块保留 —— 与修复前的并集逻辑相比，
// 唯一的行为变化是 user 侧不再吃 output_text。
func userMessageFromContent(content json.RawMessage) []rosetta.Message {
	// content 可能是纯字符串（最常见的 `{"role":"user","content":"你好"}`）。
	// decodeResponsesParts 只认分块数组，字符串会得到空结果 —— 修复前
	// 这里因此把那条消息整条丢掉，user 的第一句话凭空消失。
	if s, ok := plainContentText(content); ok {
		if s == "" {
			return nil
		}
		return []rosetta.Message{rosetta.User(s)}
	}
	blocks := make([]rosetta.Block, 0, 2)
	for _, p := range decodeResponsesParts(content) {
		switch p.Type {
		case "input_text":
			blocks = append(blocks, rosetta.Block{Type: rosetta.BlockText, Text: p.Text})
		case "input_image":
			if p.ImageURL != "" {
				blocks = append(blocks, rosetta.Block{Type: rosetta.BlockImage, ImageURL: p.ImageURL})
			}
		// file / audio 与 image 同属「用户发来的附件」。原先只认 image，
		// 于是「让模型读这份 PDF」在 user 侧同样被静默丢弃 —— 与
		// function_call_output 那次修复是同一个漏点，只是另一个入口。
		case "input_file":
			if b, ok := responsesFileBlock(p); ok {
				blocks = append(blocks, b)
			}
		case "input_audio":
			if b, ok := responsesAudioBlock(p); ok {
				blocks = append(blocks, b)
			}
		}
	}
	// 没有任何可采信的分块时**整条丢弃**。
	//
	// 不能塞一个空文本块：rosetta 的校验会判「message has no content blocks」
	// （空串不算内容）→ 整个请求 400，那比「把 assistant 输出当用户输入」还糟
	// —— 至少后者请求还能通。实测：role 缺失且只带 output_text 的项
	// 直接把请求打成 400。
	//
	// 丢掉之后剩下的项照常转发，客户端拿到的是「少了一段它自己没标角色的
	// 历史」，而不是整个请求失败。
	if len(blocks) == 0 {
		return nil
	}
	return []rosetta.Message{{Role: rosetta.RoleUser, Blocks: blocks}}
}

// plainContentText 报告 content 是否是纯字符串形态，并返回其值。
// 分块数组返回 ok=false（调用方走 decodeResponsesParts）。
func plainContentText(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}

// responsesContentText 把 message 的 content（字符串或分块数组）读成纯文本。
// includeOutputText 区分两侧：assistant 的 output_text 才是正文，user 侧只认
// input_text —— 读错方向会把对端协议的标记当正文传给上游。
func responsesContentText(raw json.RawMessage, includeOutputText bool) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var sb strings.Builder
	for _, p := range decodeResponsesParts(raw) {
		if (includeOutputText && p.Type == "output_text") || (!includeOutputText && p.Type == "input_text") {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func decodeResponsesParts(raw json.RawMessage) []responsesContentPart {
	if len(raw) == 0 {
		return nil
	}
	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	return parts
}

// responsesItemOutputText 展平 function_call_output 的 output（字符串或分块）。
func responsesItemOutputText(item responsesItem) string {
	var s string
	if err := json.Unmarshal(item.Output, &s); err == nil {
		return s
	}
	var sb strings.Builder
	for _, p := range decodeResponsesParts(item.Output) {
		if p.Type == "output_text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// responsesItemToolOutput 把 function_call_output 拆成「文本」与「媒体块」两份。
//
// 为什么要拆：tool_result 的 Content 是纯文本语义（rosetta.ToolResult 只收
// 字符串），而 Responses 的工具输出可以带 input_image / input_file /
// input_audio 分块 —— agent 让模型读图、读文档、听录音都走这里。
//
// 文本部分保持原口径（只认 output_text），媒体部分转成 rosetta 的
// image / file / audio 块，由调用方搬去紧随其后的 user 消息。
func responsesItemToolOutput(item responsesItem) (string, []rosetta.Block) {
	// 字符串形态：整体就是文本，没有媒体可拆。
	if s, ok := plainContentText(item.Output); ok {
		return s, nil
	}
	// local_shell_call_output 的形态是 {"stdout":"…","stderr":"…",
	// "metadata":{"exit_code":0}} 这类结构对象：它没有分块数组，展平成
	// JSON 文本喂给模型。若按分块数组解会得到空串，模型的工具结果就成了
	// 「执行成功但没有输出」。
	if len(item.Output) > 0 && item.Output[0] == '{' {
		return string(item.Output), nil
	}
	var text strings.Builder
	var media []rosetta.Block
	for _, p := range decodeResponsesParts(item.Output) {
		switch p.Type {
		case "output_text":
			text.WriteString(p.Text)
		case "input_image":
			if p.ImageURL != "" {
				media = append(media, rosetta.Block{Type: rosetta.BlockImage, ImageURL: p.ImageURL})
			}
		case "input_file":
			if b, ok := responsesFileBlock(p); ok {
				media = append(media, b)
			}
		case "input_audio":
			if b, ok := responsesAudioBlock(p); ok {
				media = append(media, b)
			}
		}
	}
	return text.String(), media
}

// responsesFileBlock 把 input_file 分块转成 rosetta 的 file 块。
//
// 三种载荷形态都支持（与 rosetta.Block.FileData 的口径一致）：远端 URL、
// 内联 base64（可带 data: 前缀）、provider 侧已上传的 file id。
// 三者皆空时返回 false —— 那一项什么都没说，造一个空 file 块会被
// rosetta validate 拒掉，把本来能跑的请求变成 400。
//
// # 三种形态互斥（2026-10-21 修 A2-P1-5）
//
// 修复前这里用 firstNonEmpty 三选一取值，然后**同时**写
// `FileData: data` 与 `FileID: p.FileID`。当载荷只带 `file_id`（OpenAI 官方
// Responses 的合法形态：文件已在 provider 侧上传、这里只引用其 id）时，
// data == p.FileID == "file-abc"，于是产生一个「两个源都有」的块，
// 被 rosetta 的 validate 拒掉：
//
//	file block has both FileData and FileID; set exactly one source
//
// 症状特别难查：入站解码**静默通过**，400 来自 SDK 内部的措辞，
// 客户端无从知道自己少发了 file_data。
//
// 现在按形态**互斥**赋值，与 rosetta.Block.validate() 的
// 「exactly one source」对齐：
//   - file_id 形态 → 只填 FileID（FileData 留空）；
//   - file_data / file_url 形态 → 只填 FileData（FileID 留空）。
func responsesFileBlock(p responsesContentPart) (rosetta.Block, bool) {
	// file_id 形态最优先：它是「引用 provider 侧已存在的文件」，
	// 与 file_data / file_url 是两种不同语义，不能互相顶替。
	if p.FileID != "" {
		return rosetta.Block{
			Type:     rosetta.BlockFile,
			FileName: p.Filename,
			FileID:   p.FileID,
		}, true
	}
	data := firstNonEmpty(p.FileData, p.FileURL)
	if data == "" {
		return rosetta.Block{}, false
	}
	// data: URL 只保留 base64 部分 —— rosetta 的 FileData 注释明确
	// 「raw base64, data: URL or http(s) URL」，带前缀它也认，但剥掉前缀
	// 能让 SDK 侧不必反复解析同一件事。
	if idx := strings.Index(data, ","); strings.HasPrefix(data, "data:") && idx > 0 {
		data = data[idx+1:]
	}
	return rosetta.Block{
		Type:     rosetta.BlockFile,
		FileName: p.Filename,
		FileData: data,
	}, true
}

// responsesAudioBlock 把 input_audio 分块转成 rosetta 的 audio 块。
// AudioData 同样剥掉 data: 前缀；format 只认 wav / mp3（OpenAI 的口径），
// 其它值原样带上，由上游决定是否拒绝。
func responsesAudioBlock(p responsesContentPart) (rosetta.Block, bool) {
	data := firstNonEmpty(p.AudioData, p.AudioURL)
	if data == "" {
		return rosetta.Block{}, false
	}
	if idx := strings.Index(data, ","); strings.HasPrefix(data, "data:") && idx > 0 {
		data = data[idx+1:]
	}
	return rosetta.Block{
		Type:        rosetta.BlockAudio,
		AudioData:   data,
		AudioFormat: p.Format,
	}, true
}

// responsesToolMediaNotice 是搬走媒体块后留在 tool_result 前的那行说明。
//
// 为什么必须留：模型要知道「有东西被搬走了」。不留的话，它看到一条
// 只有文字（甚至没有文字）的工具结果，会以为工具什么都没返回 ——
// 而真相是内容在紧跟着的下一条消息里。一行说明把这两件事接上。
func responsesToolMediaNotice(n int) string {
	return fmt.Sprintf("[工具结果中的 %d 个附件已移至下一条消息]", n)
}

// firstNonEmpty 返回第一个非空串；全空返回 ""。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// responsesItemReasoningText 展平 reasoning item 的 summary（字符串或分块）。
// 只认 summary_text —— reasoning_text 属于 content，与 summary 是同一份
// 思考的不同视图，两个都读会把正文重复一遍。读不到就返回空串：summary
// 只是附带的可读视图，续接真正依赖的是 encrypted_content。
func responsesItemReasoningText(item responsesItem) string {
	var s string
	if err := json.Unmarshal(item.Summary, &s); err == nil {
		return s
	}
	var sb strings.Builder
	for _, p := range decodeResponsesParts(item.Summary) {
		if p.Type == "summary_text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// ApplyUpstreamExtras 按上游协议挂 Responses 私有字段。
//
// anthropic 分支（2026-10-07 P1-8）：此前对 anthropic 整个跳过、把 tool_choice
// 静默丢掉 —— 故障转移前后同一请求行为不一致。现在能等价翻译的 tool_choice
// 必须翻译；text.format 在 Anthropic 无对应物，属硬约束，由网关层
// requiresStructuredOutput 拒绝，不会带着丢失的约束放行。
func (a *ResponsesRequest) ApplyUpstreamExtras(req *rosetta.ChatRequest, protocol string) {
	switch protocol {
	case "anthropic":
		// tool_choice 等价翻译：auto→{"type":"auto"}、required→{"type":"any"}、
		// 命名函数→{"type":"tool"}；"none" 在 Anthropic 无对应物（无法表达
		// 「工具照常声明但本轮禁止调用」），丢弃 —— 见 responsesToolChoiceAnthropic。
		if tc := responsesToolChoiceAnthropic(a.effectiveToolChoice()); tc != nil {
			mergeExtra(req, map[string]any{"tool_choice": tc})
		}
		// text.format 在 Anthropic 无对应物。json_object / json_schema 是硬约束，
		// 不允许「200 但约束丢失」—— 这类请求在 handleIngress 就被
		// requiresStructuredOutput 挡在 anthropic 候选之外，到不了这里。
		// parallel_tool_calls 亦无开关对应物，丢弃不改变正确性。
		return

	case "", "openai-chat", "auto":
		// "auto" 与 ""/"openai-chat" 同组：auto 下SDK 自动探测，落点几乎总是
		// OpenAI 方言，漏掉它等于让最常用的协议配置静默丢字段。
		extra := map[string]any{}
		if v := responsesToolChoiceOpenAI(a.effectiveToolChoice()); v != nil {
			extra["tool_choice"] = v
		}
		if f := a.textFormatMap(); f != nil {
			if rf := textFormatToResponseFormat(f); rf != nil {
				extra["response_format"] = rf
			}
		}
		if a.ParallelToolCalls != nil {
			extra["parallel_tool_calls"] = *a.ParallelToolCalls
		}
		if len(extra) > 0 {
			mergeExtra(req, extra)
		}
	case "openai-responses":
		extra := map[string]any{}
		if tc := a.effectiveToolChoice(); len(tc) > 0 {
			extra["tool_choice"] = json.RawMessage(tc)
		}
		// text 是 {"format": {...}} 的外壳，所以这里要**拆开**再放回 format。
		//
		// 修复前是把整个 a.Text 当成 format 的值，出站变成
		//   "text":{"format":{"format":{"type":"json_schema",…}}}
		// 上游看到的 format 对象里没有 type、schema 埋在重复的 format 键下，
		// 于是结构化输出**静默不生效**，而客户端以为它生效了
		// （实测抓到的出站 body 就是双层）。同函数上面的 openai-chat 分支
		// 用 textFormatMap() 正确拆包 —— 两个分支的处理方式本就该一致。
		if f := a.textFormatMap(); f != nil {
			extra["text"] = map[string]any{"format": f}
		}
		// text 里除 format 之外的键（verbosity 等）不能跟着丢，原样带上。
		if rest := a.textPassthroughKeys(); len(rest) > 0 {
			textObj, _ := extra["text"].(map[string]any)
			if textObj == nil {
				textObj = map[string]any{}
				extra["text"] = textObj
			}
			for k, v := range rest {
				if _, taken := textObj[k]; !taken {
					textObj[k] = v
				}
			}
		}
		if a.ParallelToolCalls != nil {
			extra["parallel_tool_calls"] = *a.ParallelToolCalls
		}
		// previous_response_id / store 交给 openai-responses 上游承接。
		//
		// 修复前这两个字段解析完就被丢弃：客户端拿它续会话，收到的却是
		// 「只含本次 input」的 200，模型完全不知道前文。SDK 没把
		// previous_response_id 列入 openai-responses 的保留键，走 Extra 合法。
		// 非 openai-responses 上游没有对应概念（chat/anthropic 都是无状态），
		// 继续按原样丢弃 —— 那条路径客户端本就该自带全量历史。
		if a.PreviousResponseID != "" {
			extra["previous_response_id"] = a.PreviousResponseID
		}
		if a.Store != nil {
			extra["store"] = *a.Store
		}
		if len(extra) > 0 {
			mergeExtra(req, extra)
		}
	}
}

// effectiveToolChoice 返回本次 attempt 该用的 tool_choice 原值。
//
// ToRosetta 是逐 attempt 调用的工厂，展平与 tool_choice 重写都在里面发生；
// ApplyUpstreamExtras 在同一个 attempt 里紧随其后执行，所以它读到的
// flattenedToolChoice 一定对应**本 attempt 的那份**展平结果，不存在跨
// attempt 串味（buildRosetta 不复用同一个实例，见 ingressRequest 的注释）。
func (a *ResponsesRequest) effectiveToolChoice() json.RawMessage {
	if len(a.flattenedToolChoice) > 0 {
		return a.flattenedToolChoice
	}
	return a.ToolChoice
}

func (a *ResponsesRequest) textFormatMap() map[string]any {
	if len(a.Text) == 0 {
		return nil
	}
	var wrapper struct {
		Format map[string]any `json:"format"`
	}
	if json.Unmarshal(a.Text, &wrapper) != nil || wrapper.Format == nil {
		return nil
	}
	return wrapper.Format
}

// textPassthroughKeys 取出 text 对象里除 format 之外的键（verbosity 等）。
//
// format 由 textFormatMap 单独处理；其余键我们没有建模，但原样带给上游
// 比静默丢弃更接近「透传」的本意，也避免修 format 时把别的字段一起弄丢。
func (a *ResponsesRequest) textPassthroughKeys() map[string]any {
	if len(a.Text) == 0 {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal(a.Text, &obj) != nil {
		return nil
	}
	delete(obj, "format")
	if len(obj) == 0 {
		return nil
	}
	return obj
}

// responsesToolChoiceOpenAI 把 Responses 的 tool_choice 翻译成 openai-chat 形状。
func responsesToolChoiceOpenAI(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "none", "required":
			return s
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	switch obj.Type {
	case "auto", "none", "required":
		return obj.Type
	case "function":
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
	}
	return nil
}

// responsesToolChoiceAnthropic 把 Responses 的 tool_choice 翻译成 Anthropic 形状。
//
//	"auto" / {"type":"auto"}       → {"type":"auto"}
//	"required" / {"type":"required"} → {"type":"any"}（Anthropic 的「必须调一个」）
//	{"type":"function","name":…}   → {"type":"tool","name":…}
//	"none"                          → nil（丢弃）
//
// "none" 丢弃的原因与 chatToolChoiceAnthropic 相同：Anthropic 无法表达
// 「工具照常声明、但本轮禁止调用」。decode 已把 tool_choice 收敛为字符串或
// {type,name} 两种形状，未知取值返回 nil 丢弃（防御）。
func responsesToolChoiceAnthropic(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		default: // "none" 等：无对应物，丢弃。
			return nil
		}
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	switch obj.Type {
	case "auto":
		return map[string]any{"type": "auto"}
	case "required":
		return map[string]any{"type": "any"}
	case "function":
		if obj.Name == "" {
			return nil
		}
		return map[string]any{"type": "tool", "name": obj.Name}
	}
	return nil
}

// textFormatToResponseFormat 把 Responses 的 text.format 翻译成 openai-chat 的
// response_format。text（默认）返回 nil（无需表达）；json_object 直映；
// json_schema 翻译外壳（Responses 平铺 name/schema/strict，chat 嵌套在
// json_schema 子对象里）。未知类型返回占位错误形态的 map 由 SDK 拒绝 ——
// 不在这里静默吞。
func textFormatToResponseFormat(f map[string]any) map[string]any {
	switch t, _ := f["type"].(string); t {
	case "", "text":
		return nil
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		inner := map[string]any{"type": "json_schema"}
		for _, k := range []string{"name", "schema", "strict", "description"} {
			if v, ok := f[k]; ok {
				inner[k] = v
			}
		}
		return map[string]any{"type": "json_schema", "json_schema": inner}
	default:
		return map[string]any{"type": t}
	}
}

// responseFormatToTextFormat 把 openai-chat 的 response_format 翻译回 Responses
// 的 text.format —— textFormatToResponseFormat 的反向（chat 入口打到
// responses 上游时用）。json_object 直映；json_schema 把 chat 嵌套在
// json_schema 子对象里的 name/schema/strict/description 摊平回 Responses 的
// 平铺形状；"text"/缺省是两协议共同的默认值，返回 nil 不显式表达；未知类型
// 摊平原样透传，交给上游 SDK 拒绝 —— 不在这里静默吞。
func responseFormatToTextFormat(raw json.RawMessage) map[string]any {
	var f map[string]any
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	switch t, _ := f["type"].(string); t {
	case "", "text":
		return nil
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		inner, _ := f["json_schema"].(map[string]any)
		out := map[string]any{"type": "json_schema"}
		for _, k := range []string{"name", "schema", "strict", "description"} {
			if v, ok := inner[k]; ok {
				out[k] = v
			}
		}
		return out
	default:
		return map[string]any{"type": t}
	}
}

// RequiresStructuredOutput 报告该请求是否带**硬性**结构化输出约束
// （text.format.type 为 json_object / json_schema）。与 chat 入口的同名方法
// 同一用途：Anthropic 上游没有对应物，网关据此把 anthropic 候选从链上滤掉
// 而不是放行后让约束静默失效（见 handleIngress）。format 缺失或为默认的
// "text" 时返回 false。
func (a *ResponsesRequest) RequiresStructuredOutput() bool {
	f := a.textFormatMap()
	if f == nil {
		return false
	}
	t, _ := f["type"].(string)
	return t == "json_object" || t == "json_schema"
}
