# A3 审计报告：出站协议层 `internal/outwire/`

- 仓库：`rosetta-gateway`
- 基线：`main` = `ea19ce1`（工作区有 `internal/webui/dist` 的未提交改动，与本子域无关，本报告未触碰）
- 审计范围：`internal/outwire/` 全部 6 个源文件 + 9 个测试文件所覆盖的行为
- 审计方式：只读。源码阅读 + grep 定位 + **在 `%TEMP%` 下用独立 Go module 复制 outwire 源码实测复现**（仓库内零写入）
- 依赖基线：`github.com/cn-maul/rosetta@v1.2.0`

## 统计

| 级别 | 数量 |
|---|---|
| P0 | 0 |
| P1 | 6 |
| P2 | 8 |
| P3 | 4 |

---

## P1（确定的功能或安全缺陷）

### P1-1 Anthropic 上游的 content-block index 被直接当作 OpenAI 的 tool_call index 下发

`internal/outwire/openai_chat.go:234`（`td := map[string]any{"index": index}`）经由
`internal/outwire/sink.go:96` 直接透传 `ev.ToolIndex`，而 `ev.ToolIndex` 在 Anthropic
上游语义里是**整个消息的 content block 序号**，不是「第几个工具调用」。

证据（SDK 侧）：`rosetta@v1.2.0/provider_anthropic.go:842` `ToolIndex: ch.Index`（content_block_start）
与 `:868` `ToolIndex: ch.Index`（input_json_delta）—— 两者用的是 Anthropic 的
content block index。

实测复现（`%TEMP%\a3tool`，假 Anthropic 上游：index 0 = 文本块，index 1 = tool_use 块）：

```
type=tool_call  toolIndex=1 toolID="toolu_1" name="get_weather" args=""
```

把同一事件喂给 `openaiSink` 后实际下发的分片（实测输出）：

```
data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"","name":"get_weather"},"id":"toolu_1","index":1,"type":"function"}]},"finish_reason":null,"index":0}],"created":0,"id":"chatcmpl-1","model":"m","object":"chat.completion.chunk"}
```

`"index":1` 而非 0。OpenAI 客户端的常规合并逻辑是「按 index 归并 tool_call」，
第一个工具调用是 index 1 意味着客户端的 tool_call 数组第 0 位为空，且
`finish_reason:"tool_calls"` 与实际调用数对不上。跨协议转换时统一事件流没有对
ToolIndex 做**重编号**，这是根因：Anthropic 的 block index、OpenAI chat 的
tool index、Responses 的 output_index 是三套互不相同的编号空间，而
`rosetta.Event.ToolIndex` 一个字段承载了它们。

同理命中 `internal/outwire/anthropic.go:395`（`s.ensureBlock("tool_use", ev.ToolIndex)`）
与 `internal/outwire/openai_responses.go:419`（`s.toolIdx != ev.ToolIndex`）：当上游是
OpenAI-chat 时 `ToolIndex` 是 tool index（0,1,2…），当上游是 Anthropic 时是 block
index —— 下游 Responses/Anthropic sink 用它做「同一个工具」的判据，跨协议时会误判。

影响面：任何「Anthropic 上游 → OpenAI Chat / Responses 下游」的组合。并行工具调用时
更严重：Anthropic 给出 index 1、3、5，下游看到三个分散的 index。

待确认：是否已有上层在 `rosetta` 事件层做重编号。**已核实没有** ——
`rosetta@v1.2.0` 的 `provider_anthropic.go` 直接把 `ch.Index` 写进 `ToolIndex`，
网关侧 `internal/outwire/` 是唯一消费方。

### P1-2 Responses sink 的 reasoning 状态机不支持多个 reasoning item，思考内容跨 item 串味

`internal/outwire/openai_responses.go:70-75` 只有**单组** reasoning 状态
（`reasonOpen` / `reasonIdx` / `reasonItemID` / `thinkText`），而 `thinkingDelta`
（`:293-341`）只在 `!s.reasonOpen` 时开新 item。

触发条件：上游产出第二个 reasoning item 时，`reasonOpen` 仍为 true（第一个 item
若没有 sealed 载荷就永不收尾），于是第二片的思考文本被写进**第一个** item。

实测（`%TEMP%\a3sink`，直接调用 `outwire.NewResponsesSSE`）：
输入 `thinking{Text:"A1", ReasoningID:"rs_1"}` → `thinking{Text:"B1", ReasoningID:"rs_2"}`
→ `thinking{Sealed:"SEAL_B", SealedBy:"openai-responses", ReasoningID:"rs_2"}`，实际输出：

```
event: response.output_item.added
data: {"item":{"id":"rs_1","status":"in_progress","type":"reasoning"},"output_index":0,...}
event: response.reasoning_summary_text.delta
data: {"delta":"A1","item_id":"rs_1","output_index":0,...}
event: response.reasoning_summary_text.delta
data: {"delta":"B1","item_id":"rs_1","output_index":0,...}      <-- 串进 rs_1
event: response.output_item.done
data: {"item":{"encrypted_content":"SEAL_B","id":"rs_1","status":"completed","summary":[{"text":"A1B1","type":"summary_text"}],"type":"reasoning"},"output_index":0,...}
```

三个问题同时出现：

1. `rs_2` 这个 item **从未被创建**（没有 `output_item.added`，没有自己的
   `output_item.done`）—— 客户端按 item 渲染时 rs_2 不可见。
2. `rs_1` 的 summary 变成 `"A1B1"`（前缀重复），与 `ebdf11b` 想修的
   「逐段变长、前缀重复」症状**同类**，只是这次发生在 item 之间而非 item 内部。
3. `rs_2` 的 sealed 载荷被挂到了 `rs_1` 的 id 上。客户端下一轮回放时按
   `id:"rs_1"` + `encrypted_content` 提交，OpenAI 侧会认为这是 rs_1 的载荷 ——
   **跨轮 reasoning 续接状态被错配**。

根因：`ebdf11b` 把「一个 item 跨多片增量」做对了（同一 item 内不再一片一个），
但把「item 的边界」判定完全押在 `Sealed` 非空上（`:337-341`：`if ev.Sealed == "" { return }`），
没有用 `ev.ReasoningID` 变化来切 item。SDK 侧 `rs_1`/`rs_2` 是两个不同的
`ReasoningID`（实测确认：`rs_1` 与 `rs_2` 分别以 `ReasoningID` 送达），信息是有的，
只是出站层没用。

附带：`s.thinkText.Reset()` 只在开新 item 时执行（`:319`），item 边界错位时
累积的不只是两个 item 的文本。

### P1-3 Responses sink 的 message 文本跨 item 累积，第二个 message item 的文本被污染

`internal/outwire/openai_responses.go:411` `s.text.WriteString(ev.Text)` 写入的是
**跨 item 共享**的 builder，而 `closeMessage`（`:169`）`done := strings.Clone(s.text.String())`
读的是同一个 builder，且**开新 message item 时从不清空**（对比 `thinkText.Reset()`
在 `:319`、`toolArgs.Reset()` 在 `:431` 都有 Reset —— `s.text` 没有）。

实测（text → tool → text 两个 message item）：

```
event: response.output_text.done
data: {"content_index":0,"item_id":"resp_1-msg-0","output_index":0,"text":"AAA",...}
...
event: response.output_text.done
data: {"content_index":0,"item_id":"resp_1-msg-2","output_index":2,"text":"AAABBB",...}
```

第二个 message item 的 `output_text.done` 文本是 `"AAABBB"` —— 第一个 item 的
`"AAA"` 被拼了进去。客户端若按 `output_text.done` 而非按 delta 累积，
第二段文本会变成 `AAABBB`；`response.completed` 内嵌的 `output` 数组同样带这个错值。

触发条件：上游产出多个 message item（Responses 协议允许，OpenAI 官方在
tool-use 循环里就会产出）。实测确认 SDK 会把两个 message item 的文本分别以
`EventTextDelta` 送达（`outputIndex=0` 与 `outputIndex=2`），出站层却合并进了
同一个 builder。

`internal/outwire/openai_responses.go:531-541` 的**非流式**路径已经修过同一类问题
（`msgIdx` 参与 message item 的 id 生成，注释明确写「旧实现恒用 `id + "-msg"`」，
于是「文本 → tool_use → 文本」会产出多个 id 完全相同的 message item，下游按 id
索引时互相覆盖，第二个文本段凭空消失）。**流式侧的同一 bug 未修** ——
这正是「局部修补治不了根因」的典型：同一类 id/文本归属问题在两个文件里各修一半。

### P1-4 Responses sink 忽略 `Event.OutputIndex`，上游 item 顺序与下游 output_index 错位

`internal/outwire/openai_responses.go` 全篇不读 `ev.OutputIndex`。
`rosetta.Event` 明确定义了这个字段（`rosetta@v1.2.0/stream.go:52-55`：「an optional
provider output-item position … It orders items in the materialized content」），
且 Responses 适配器确实填了它（实测：`outputIndex=0` 与 `outputIndex=2`）。

出站的 `output_index` 是**网关自己按到达顺序分配**的（`:316`、`:399`、`:422`
三处 `s.outputIndex++`），不是上游的。后果：

- 上游 item 顺序与下游 `output_index` 不一致（例如上游 reasoning 在 index 0、
  function_call 在 1、message 在 2；网关因为先看到 reasoning 会把它排到 0，
  但 function_call 与 message 的相对顺序在「上游先发 message 再发 function_call」
  时会被对调）。
- 上游跳号（index 0, 2）时下游变成 0, 1 —— 序号不连续。Responses 规范要求
  `output_index` 是严格递增的分配序，**连续性本身是协议要求**，不是美观问题。
- 客户端用 `output_index` 对齐 item 与 delta 时会错位。

影响：`internal/outwire/openai_responses.go:169-186`（`closeMessage`）、
`:193-215`（`closeTool`）、`:316-340`（`thinkingDelta`）、
`:399-415`（message item）、`:422-435`（tool item）全部受影响。

### P1-5 Anthropic 非流式响应在 tool_use 的 `input` 不是合法 JSON 时产出**空响应体 + 200**

`internal/outwire/anthropic.go:150`：`input := json.RawMessage(blk.Arguments)`，
只在 `len(input) == 0` 时补 `{}`。注释断言「rosetta 校验已保证非空时是合法 JSON 对象」
——**这个前提对上游返回的 arguments 不成立**。

`rosetta` 的 `Block.validate()`（`message.go:329-334`）只校验**入站请求**的
arguments；上游返回的 arguments 走 `provider_openai_chat.go:861` 的
`Arguments: tc.Function.Arguments.Value`，而 `jsonx.FlexJSONString`（`jsonx.go:214-231`）
对任意形状都「不失败」，会把 `"not-json"` 原样塞进 `Value`。

实测（`%TEMP%\a3tool`，假上游返回 `arguments:"not-json"`）：

```
block type=tool_call toolID="call_1" name="f" args="not-json"
```

然后走 `WriteAnthropicResponse` 的编码路径，`json.NewEncoder(w).Encode(...)` 直接返回错误：

```
encode err: json: error calling MarshalJSON for type *jsontext.Value: invalid character 'o' in literal null (expecting 'u')
status=200 bodyLen=0 body=""
```

客户端收到的是 **HTTP 200 + 零字节响应体**。这比报错更糟：客户端拿到 200 却解析不出
任何东西，无法区分「上游返回了空答案」与「网关坏了」；而且 `json.Encoder.Encode` 的
错误被**完全丢弃**（`internal/outwire/anthropic.go:172` 与 `:168` 附近的
`json.NewEncoder(w).Encode(...)` 没有检查返回值），服务端也不会有任何日志。

同一函数的 `id` 兜底（`:164-168`）也说明作者知道 `resp.ID` 可能为空，但
`blk.Arguments` 的合法性从未被检查。

对照：OpenAI Chat 非流式侧（`internal/outwire/openai_chat.go:330`）把 arguments
作为**字符串**放进 `OpenAIFunction.Arguments`，非法 JSON 只是字符串内容，不会
破坏编码 —— 同一类上游数据在两条出站路径上的行为不一致。

### P1-6 Responses 非流式响应的 `usage` 缺 `output_tokens_details`（思考 token 无处可查）

`internal/outwire/openai_responses.go:491-495`：

```go
resp["usage"] = map[string]any{
    "input_tokens":  usage.InputTokens,
    "output_tokens": usage.OutputTokens,
    "total_tokens":  usage.TotalTokens,
}
```

只有三个数字。`rosetta.Usage` 里有第四个字段 `ReasoningTokens`
（`rosetta@v1.2.0/usage.go:24`），SDK 的 Responses 适配器**确实**从
`output_tokens_details.reasoning_tokens` 解析它（`provider_openai_responses.go:961-978`），
但出站层把它丢掉了。

对比同一文件的非流式侧（`:608-618`）已经补了 `input_tokens_details.cached_tokens`，
流式侧 `response.completed`（`:491`）却没有 —— 同一份 usage 在
「Responses 非流式」与「Responses 流式」之间字段不对称。

影响：Codex / Responses 客户端按 `output_tokens_details.reasoning_tokens` 区分
「思考 token」与「回答 token」做计费与展示。拿不到该字段时，客户端只能把全部
output token 当作回答 token —— **思考型模型的成本统计与展示系统性失真**。

对照 OpenAI Chat 侧同样缺失 `completion_tokens_details.reasoning_tokens`
（`internal/outwire/openai_chat.go:271-282` 的 `WriteUsage` 与 `:344-350` 的
非流式 usage），这是同一个系统性缺口的两个实例。

---

## P2（架构 / 设计 / 一致性问题，会持续产 bug）

### P2-1 写错误被全部丢弃：SSE 写入失败后状态机继续推进，产出「半个响应」

三个 sink 的写路径都不检查 `fmt.Fprintf` 的返回值：

- `internal/outwire/openai_chat.go:120,122`（`WriteEvent`）、`:136`（`WriteComment`）
- `internal/outwire/anthropic.go:255`（`writeRaw`）、`:277`（`Keepalive`）
- `internal/outwire/openai_responses.go:131`（`writeEvent`）、`:504`（`Keepalive`）

`SSEWriter.WriteEvent` 甚至**恒返回 nil**（`:126`），`WriteToolCallDelta` 等一串
方法的 `error` 返回值因此永远是 nil —— 调用方无从知道写失败了。

这与 `internal/outwire/write_deadline.go` 的设计直接冲突：那一整套机制的存在意义
就是「下游挂死不读时让 Write 自己返回超时错误」（`write_deadline.go:15-26`
的注释明确说「由内核让 Write 自己返回超时错误」）。但**错误被丢弃了**，
于是：

1. 写超时后，sink 继续写下一片，`roleSent` / `blockIndex` / `outputIndex` 等
   状态继续推进 —— 状态机与实际落到线上的字节**永久错位**。
2. `attemptStream`（`cmd/gateway/main.go:2849`）调 `sink.Finish(...)` 时，
   不知道写已经失败，照发 `finish_reason` + `[DONE]`（OpenAI）或
   `message_delta` + `message_stop`（Anthropic）—— 客户端看到的是一个**语法完整
   但内容残缺**的成功流，而不是「异常中断」。这与 DESIGN §8.2「断流不发终止事件」
   的约定直接冲突：断流语义在写失败这条路径上完全失效。
3. 没有任何日志。30 秒写超时保护在生效时表现为「静默截断」。

根因：写错误没有从 `SSEWriter` 传到 `StreamSink` 接口，再到 `attemptStream`。
接口层（`internal/outwire/sink.go:16-43`）的 `Event`/`Finish`/`Keepalive` 全部
返回 void，无处承载这个信号。

### P2-2 `setWriteDeadline` 在写前设置、写后清除，与「单次写预算」的意图相互抵消

`internal/outwire/write_deadline.go:56-60`（`clearWriteDeadline` 的注释）声称
「清掉截止时间，避免连接被复用时带着过期的写截止时间进入下一次请求
（net/http 通常会在下一次写前重置，但这里不赌）」。

问题：清掉之后，**下一次写会重新设**（`setWriteDeadline` 在每次写前调用，
见 `openai_chat.go:117`、`anthropic.go:253`、`openai_responses.go:120`），
所以「连接复用」这条理由不成立 —— 复用连接的下一次请求的第一次写就会设新截止时间。
真正的效果是：**写与写之间的窗口期没有写截止时间保护**。

这在当前调用形态下是安全的（每次写都重新设），但它意味着 `writePerFrameTimeout`
只约束**单次 `Write` 调用**，不约束「一条 SSE 分片从构造到落盘」的完整时间，
也不约束 `Flush()`（`Flush` 不走 `ResponseController` 的写截止时间路径，
Go 的 `http.Flusher.Flush` 不接受 deadline 语义）。

更实际的风险是 P2-1 的组合：写超时 → 错误被丢 → `clearWriteDeadline` 把截止时间
撤掉 → 下一片写又设一个**新的 30 秒** —— 一次挂死的下游可以让 handler 无限期
地每 30 秒推进一片，永远不返回。

### P2-3 `errors.go` 的 `logAPIError` 把上游原文写进日志，且未脱敏

`internal/outwire/errors.go:112-115`：

```go
func logAPIError(e *rosetta.APIError) {
    slog.Warn("upstream returned an error",
        "status", e.StatusCode, "type", e.Type, "message", e.Message)
}
```

`e.Message` 是**上游错误体的原文，未脱敏**。同一个文件 `:167-186` 的
`UpstreamErrorDetail` 明确指出这一点并做了处理：

> SDK 只在 APIError.Error() 拼字符串时调 httpx.MaskSecrets（rosetta 内部包，
> 网关 import 不到），**Message 字段本身是原文未脱敏** —— 上游可能把调用方的
> key 回显在错误里。

但 `logAPIError` 走的是 `slog` 属性，**不经过 `APIError.Error()`**，所以
`httpx.MaskSecrets` 不会生效。落库路径（`UpstreamErrorDetail`）做了 `maskSecrets`，
日志路径没做 —— 同一份原文，**落库的那份脱敏了，写日志的那份没有**。

这是 DESIGN §9 明确要求的：「**出站错误体也要脱敏**：上游错误里的疑似密钥材料
在写日志与返回前掩码」。当前只满足了「返回前」，没满足「写日志前」。

影响：上游（尤其是中转网关）把调用方 key 回显在错误 message 里时，
该明文出现在网关日志。日志通常比 usage_records 的访问控制更宽松，
构成凭据泄漏面。

### P2-4 `maskSecrets` 正则是 SDK 的一份**手工复制**，漂移无感知

`internal/outwire/errors.go:189` 复制了 `rosetta@v1.2.0/internal/httpx/httpx.go:318`
的 `secretRe`。注释自称「正则必须与 SDK 保持一致 —— 若两边漂移，网关落库的文本
可能漏掉 SDK 已经掩掉的某种凭据形状」，但**没有任何机制保证它不漂移**：

- `httpx` 是 `internal/` 包，网关 import 不到（这是真的）。
- 但 SDK 侧 `httpx.go:313-318` 的注释明说这份正则「lives here so the rosetta
  package's error redaction and this transport's log redaction can never drift
  apart」—— SDK 内部有单一事实来源，网关这一份是**第三份副本**。
- 仓库里没有任何测试断言两份正则相同（`internal/outwire/error_detail_test.go:41`
  的 `TestUpstreamErrorDetail_MasksSecrets` 只测网关自己这份）。

SDK 升级时若新增凭据形状（如新的 `sk-` 前缀变体），网关这份不跟着变，
落库的 `error_detail` 就会带着明文密钥。**待确认**：SDK 后续版本是否新增过形状
（当前 v1.2.0 与网关这份逐字相同，暂无实际漂移）。

修法：把正则来源收敛到一处（例如网关自己的 `internal/` 包，SDK 通过 Extra
或公开 API 暴露掩码函数），并加一条与 SDK 版本联动的测试。

### P2-5 `errorTypeFromCode` / `anthropicErrorType` 的 code 覆盖不完整，靠 default 兜底

把网关实际会写出的 code 全集列出来对比（`cmd/gateway/main.go:1330-1375`、
`:1762-1952`、`:3730-3737`，`cmd/gateway/billing.go:704-715`）：

| code | 来源 | `errorTypeFromCode` | `anthropicErrorType` |
|---|---|---|---|
| `invalid_api_key` | main.go:1330 | ✓ authentication_error | ✓ |
| `key_expired` | main.go:1339 | ✗ → **api_error** | ✗ → **api_error** |
| `ip_not_allowed` | main.go:1343 | ✗ → **api_error** | ✗ → **api_error** |
| `admin_cannot_call_model` | main.go:1371 | ✓ permission_error | ✗ → **api_error** |
| `model_not_allowed` | main.go:1952, 3737 | ✗ → **api_error** | ✗ → **api_error** |
| `rate_limit_exceeded` | main.go:1762 | ✓ | ✓ |
| `request_too_large` | main.go:1774 | ✓ | ✓ |
| `invalid_request_error` | main.go:1778 | ✓ | ✓ |
| `insufficient_quota` | main.go:1855,1909 | ✓ | ✓ |
| `insufficient_balance` | billing.go:715 | ✓ | ✓ |
| `model_not_found` | main.go:1925 | ✓ | ✓ |
| `internal_error` | billing.go:704 | ✗ → api_error | ✓ api_error |
| `upstream_auth_error` | errors.go | ✓ | ✓ |
| `upstream_quota_exhausted` | errors.go | ✓ | ✓ |
| `upstream_timeout` | errors.go | ✓ | ✓ |
| `upstream_error` | errors.go | ✓ | ✓ |
| `context_length_exceeded` | errors.go | ✓ | ✓ |

四个 code 掉进 default：`key_expired`、`ip_not_allowed`、`model_not_allowed`、
`admin_cannot_call_model`（Anthropic 通道）。

后果（以 `admin_cannot_call_model` 为例）：`errorTypeFromCode` 特意把它映射成
`permission_error`，理由写得很清楚（「终态的权限边界，不要重试，请改权限」），
但**同一条错误走 Anthropic 通道时类型退化成 `api_error`** ——
Claude Code / Anthropic SDK 对 `api_error` 的处理与 `permission_error` 不同，
会给出「重试 / 上游故障」的错误建议，而真相是「账号角色不对，重试一万次也没用」。
这与 `cmd/gateway/main.go:1356` 的注释「与本文件既有的 key_disabled / key_expired /
ip_not_allowed / model_not_allowed 口径一致」相矛盾。

`model_not_allowed` 尤其值得注意：这是**该 key 无权用这个模型**，语义上最接近
Anthropic 的 `permission_error`，但两条通道都给 `api_error`。

### P2-6 Anthropic 通道不区分「上游 400」与「网关自己造的 400」

`internal/outwire/anthropic.go:19-21` 的注释说「状态码沿用 MapUpstreamError 的
映射结果，不在 Anthropic 通道里重新分类」—— 这本身合理。但结合 P2-5，
`anthropicErrorType` 的 `switch code` 里有 `case "invalid_request_error"`，
它同时匹配两类来源完全不同的错误：

- 上游 400（`internal/outwire/errors.go:83-89`，code=`invalid_request_error`）——
  上游说「你的请求有问题」。
- 网关自己的入站校验失败（`cmd/gateway/main.go:1778`）—— 网关说「你的请求有问题」。

两者对客户端的动作建议不同：前者可能需要改 prompt 长度/格式，后者是网关的
schema 约束。而 `param` 字段在 Anthropic 形状里根本不存在
（`internal/outwire/anthropic.go:27-31`），客户端拿不到定位信息。OpenAI 形状有
`param`（`internal/outwire/errors.go:19-22`）但 `WriteOpenAIError`（`:203-215`）
从不填它 —— `ErrorDetail.Param` 永远是空串。

### P2-7 三条出站路径的 usage 字段口径互不对齐

把三套协议的非流式 usage 字段列全：

| 字段 | OpenAI Chat 非流式 | Anthropic 非流式 | Responses 非流式 | OpenAI Chat 流式 | Responses 流式 |
|---|---|---|---|---|---|
| input/prompt tokens | ✓ `prompt_tokens` | ✓ `input_tokens`（拆分后） | ✓ `input_tokens` | ✓ | ✓ |
| output tokens | ✓ | ✓ | ✓ | ✓ | ✓ |
| total tokens | ✓ | ✗ | ✓ | ✓ | ✓ |
| cached tokens | ✓ `prompt_tokens_details.cached_tokens` | ✓ `cache_read_input_tokens` | ✓ `input_tokens_details.cached_tokens` | ✓ | ✗ **缺** |
| cache creation tokens | ✗ | ✓ `cache_creation_input_tokens` | ✗ | ✗ | ✗ |
| reasoning tokens | ✗ **缺** | ✗ | ✗ **缺** | ✗ **缺** | ✗ **缺** |

三处系统性缺口：

1. **reasoning tokens 全线缺失**（见 P1-6）。`rosetta.Usage.ReasoningTokens` 被
   解析出来、落进了 `usage_records`（`cmd/gateway/main.go:2866`）、
   却从不告诉客户端。**同一份数据进了数据库、没进 API 响应**。
2. **Responses 流式缺 cached tokens**（`internal/outwire/openai_responses.go:491`），
   而 Responses 非流式有（`:614`）。同一协议内流式/非流式不对称 ——
   与 `internal/outwire/openai_chat.go:56-61` 注释里描述的那个已修复 bug
   （「非流式路径却没有 —— 同一个网关、同一次缓存命中，非流式客户端算出的成本
   会系统性偏高一个数量级」）是**完全同型**的问题，只是这次在 Responses 通道。
3. **cache creation tokens 只在 Anthropic 通道出现**。这本身是协议差异
   （OpenAI 无此概念），但由于 `rosetta.Usage.InputTokens` 是 OpenAI 语义
   （含缓存），`internal/outwire/anthropic.go:110-127` 的 `anthropicUsageFields`
   必须做减法拆分 —— 这段拆分逻辑是**三处唯一正确处理了缓存口径的地方**，
   却没有统一到一处。OpenAI Chat 侧的 `prompt_tokens` 直接抄 `InputTokens`
   （含缓存），符合 OpenAI 语义；Responses 侧同理。这个不一致目前是正确的，
   但它靠三份各自独立的代码维持，任何一次新增协议都会重犯。

### P2-8 `openaiSink.Finish` 的 usage 下发条件与「零内容流」的判定口径不一致

`internal/outwire/sink.go:110-114`：

```go
if sendUsage && (usage.InputTokens > 0 || usage.OutputTokens > 0) {
    s.sse.WriteUsage(usage)
}
```

条件是「input 或 output > 0」。但 `rosetta.Usage` 有六个字段
（`rosetta@v1.2.0/usage.go:11-25`），`IsZero()` 的定义把 cache / reasoning
字段也算进去（`:32-35`，注释解释「只报了缓存或只报了思考也是报了 usage」）。

于是存在一个口径裂缝：上游只报了 `cached_tokens`（`CachedInputTokens > 0`，
而 `InputTokens` 为 0 —— 某些上游的 usage 口径就是这样）时，
`Usage.IsZero()` 为 false（网关认为「报了 usage」，`cmd/gateway/main.go:2888`
会走 `rate.commit(lastUsage.TotalTokens)`，而 `TotalTokens` 是 0），
但 `openaiSink` 的条件为假 → **usage chunk 不下发**。

同时 `cmd/gateway/main.go:2888-2892` 的分支是「`IsZero()` 为真 → 只退配额预占、
保留 TPM 预占」，而 `IsZero()` 为假 → `rate.commit(0)`（`TotalTokens` 为 0，
等于全额退还 TPM 预占）。所以这个 case 下**既没有 usage 下发，TPM 预占也全退了**
—— 限速对「只报缓存 token」的上游完全失效。

待确认：是否有上游真的报 `prompt_tokens=0, prompt_tokens_details.cached_tokens>0`。
**待确认**（无实测数据），但代码路径的裂缝是确定的。

---

## P3（nit）

### P3-1 `ResponsesSSE.fcSeq` 字段声明后从未使用

`internal/outwire/openai_responses.go:80` 声明 `fcSeq int`，全文件无任何读写
（grep 确认只有这一处）。残留字段，暗示曾是序号计数器，职责已被 `s.seq` 接管。

### P3-2 `internal/outwire/sink.go` 的 `BindDowngradedTools` 无调用方

`internal/outwire/sink.go:57-62` 定义了 `BindDowngradedTools`，全仓库无调用点
（grep 只命中定义处与 `ResponsesSSE.WithDowngradedTools` 的注释）。
Responses sink 的降级表是通过构造参数 `NewResponsesSSE(..., downgraded)` 传入的
（`cmd/gateway/main.go:1735`），走的是另一条路。

即：`BindDowngradedTools` 是一个**死代码**的能力探测入口。它与
`BindWriteDeadline` 长得几乎一样（同一段注释风格、同一套能力探测），
后来者很容易误以为降级表是经它绑定的 —— 而实际上 sink 构造早于任何
`BindDowngradedTools` 调用（`cmd/gateway/main.go:1704` 构造 sink，
`:1733` 已经是构造时传参），这条路径实际不可能被走到。

### P3-3 `anthropicErrorType` 的 `status` 参数有一半分支用不上

`internal/outwire/anthropic.go:34` 的签名带 `status int`，但只有
`case "invalid_api_key"`（`:37-41`）与 `case "upstream_auth_error"`（`:74-79`）
用到了它。其余分支只用 `code`。而这两个用到 `status` 的分支，
上游 401/403 已经被 `MapUpstreamError`（`internal/outwire/errors.go:69-71`）
**统一压成 502**，`anthropicErrorType` 收到的 status 永远是 502
（`anthropicErrorType(code, statusCode)` 传的是 HTTP 状态码 502，不是上游状态码）。

于是 `if status == http.StatusForbidden { return "permission_error" }` 这个分支
**永远不会命中** —— 401 与 403 都返回 `authentication_error`。
这与 `internal/outwire/errors.go:69-71` 的注释「绝不透传 401」一致（状态码确实
被压平了），但意味着 Anthropic 通道无法区分「key 被停用（403）」与
「key 无效（401）」，而 `anthropicErrorType` 的注释
（`internal/outwire/anthropic.go:38`）明说「Anthropic 用两种类型区分」——
该区分在当前调用链上**不可达**。

### P3-4 `internal/outwire/openai_chat.go` 的 `OpenAIChatChoice.Delta` 字段永不使用

`internal/outwire/openai_chat.go:24-28` 定义了 `Delta *OpenAIMessage`，但
`WriteNonStreamResponse`（`:342-346`）只填 `Message`。`Delta` 是 `omitempty`
指针，零值即省略，输出正确 —— 只是留着一个永不出现的字段，容易让人误以为
非流式响应也会带 delta。

---

## 已验证无问题的区域（及理由）

1. **`write_deadline.go` 的 `ResponseController` 用法本身没有回归**
   （`internal/outwire/write_deadline.go:40-51`）。历史坑是「包装 writer 导致
   丢 SSE 帧」—— 当前实现**没有包装 writer**，而是直接用
   `http.NewResponseController(w).SetWriteDeadline(...)` 给底层连接设截止时间，
   分片语义不受影响。`internal/server/server.go:110-113` 的 `Unwrap()` 让
   ResponseController 能穿透 `statusResponseWriter` 拿到真正的
   `*http.response`，能力探测成立。`internal/outwire/write_deadline_test.go:31-56`
   有回归护栏，且四个 sink（含 `openaiSink` 的转发层）都满足 binder 接口。
   注：P2-1/P2-2 指出的是**写错误被丢弃**与**截止时间语义**的问题，
   不是「包装 writer 丢帧」的旧坑复发。

2. **SSEWriter 的并发写保护是正确的**（`internal/outwire/openai_chat.go:78-86`
   的 `mu sync.Mutex`）。`WriteEvent` / `WriteComment` / `SetResponseID` /
   `ensureRole` 全部在同一把锁内。心跳 goroutine（`cmd/gateway/main.go:2759-2771`）
   与主事件循环并发写的场景有锁保护。`AnthropicSSE`（`:225`）与
   `ResponsesSSE`（`:39`）也各自有锁。

3. **Anthropic sink 的块状态机对「同一工具的多片参数」处理正确**
   （`internal/outwire/anthropic.go:310-315` 的注释与 `:352-360` 的
   `ensureBlock`）。同块判据用 `ToolIndex` 而非 `ToolID` 是对的 ——
   续片只带 `ToolIndex + ArgumentsDelta`（`rosetta@v1.2.0/stream.go:29`），
   用 ToolID 判会把每个参数分片切成一个新块。`openToolIdx` 初始为 -1
   （`:238`）避免了「未开块」与「index 0 的块」混淆。
   `internal/outwire/toolcall_stream_test.go:22` 有专门护栏。

4. **OpenAI Chat 的 tool_call 分片恒带 `function` 对象、且 `name` 只在非空时出现**
   （`internal/outwire/openai_chat.go:230-262`）。这是对严格 schema 客户端
   （AI SDK 的 `openaiChatChunkSchema`）的正确兼容，且避免了
   `name:""` 覆盖已有工具名的问题。`internal/outwire/toolcall_function_test.go:36`
   与 `toolcall_shape_audit_test.go:157-230` 有实测护栏。

5. **Responses 序列号 `sequence_number` 连续且无重号**
   （`internal/outwire/openai_responses.go:119-135`）。序号只在 `writeEvent`
   一处发放，赋值在 payload 建好之后、序列化之前；实测输出
   0→1→2→3→4→5 严格递增。注意：这里**只保证网关自发的序号连续**；
   上游 `output_index` 的连续性是另一回事，见 P1-4。

6. **OpenAI Chat 的 `[DONE]` / 断流语义符合 DESIGN §8.2**
   （`internal/outwire/sink.go:104-120`）。`status=="ok"` 才发
   `finish_reason` + `[DONE]`；truncated/overflow/error/canceled 都不发
   `[DONE]`。Anthropic 通道同理（`internal/outwire/anthropic.go:409-425`）：
   断流发 `event: error` 且不发 `message_stop`，canceled 什么也不写。
   这一点与 DESIGN §8.2 逐条对齐。

7. **Anthropic 缓存 token 的三桶拆分是正确的**
   （`internal/outwire/anthropic.go:110-127`）。`rosetta` 的 Anthropic 适配器
   把 cache read/creation **折进** `InputTokens`（`provider_anthropic.go:1016-1027`），
   而 Anthropic wire 语义要求三桶互斥。`anthropicUsageFields` 的减法拆分
   （含负值下限保护）正确避免了重复计费。
   `internal/outwire/anthropic_test.go:259-368` 有护栏。

8. **错误响应不泄漏上游 URL / api key / 内部路径**（响应体侧）
   （`internal/outwire/errors.go:83-96`、`:104-105`）。上游 400 的原文、
   transport error 的原文（含 endpoint 地址）都只进日志，响应体用固定文案。
   `ErrUpstreamMalformed` 同样只回固定串。**日志侧的泄漏见 P2-3**。

9. **`FailoverEligible` / `CredentialCooldown` 与 DESIGN §10 一致**
   （`internal/outwire/errors.go:287-353`）。5xx/401/403/404/410/402/408/429/
   传输错误可转移，400/422/流截断不可转移；冷却 30min/1h/60s 与
   DESIGN §10 的表逐条一致；404/410 返回 0 冷却（不罚凭据）也正确。

10. **`UpstreamErrorDetail` 的脱敏与截断正确**
    （`internal/outwire/errors.go:167-186`）。rune 边界截断避免了中文乱码尾巴，
    1KB 上限合理。`internal/outwire/error_detail_test.go` 有针对性护栏。

11. **`applyReplayGate`（`cmd/gateway/replay_gate.go`）的剥块逻辑闭环**
    —— 它虽在 `cmd/gateway` 而非 outwire，但直接决定出站内容。chat 剥全部思考块、
    anthropic 只剥无签名的、responses 不动（SDK 的 SealedBy 门闩自管），
    与 `rosetta@v1.2.0/provider_openai_chat.go:189,208-211` 与
    `provider_anthropic.go:409-417` 的实际约束一致；剥空后的 assistant turn
    整条删除，避免 SDK 报「只有 thinking 块无法回放」的 400。
    **没有把该保留的思考内容也剥掉**：anthropic 通道保留有签名的 thinking 与
    redacted_thinking（`:81-82`），与 SDK 的回放要求吻合。

12. **Responses sink 的 `SealedBy` 门闩正确**
    （`internal/outwire/openai_responses.go:294-297` 与 `:545-547`）。
    非 `openai-responses` 封的载荷既不出现在流式事件里、也不出现在非流式
    output item 里 —— 跨协议的不透明状态不外泄。

13. **下游 client 中断（context canceled）的错误路径**
    （`cmd/gateway/main.go:2819-2823`、`:2244`、`:2260-2262`）。客户端走了时
    `status="canceled"`，不写错误响应、不记 error 明细、不罚凭据、不计熔断。
    `internal/outwire` 侧对应的是「canceled 不写任何东西」
    （`sink.go:116-118`、`anthropic.go:414-417`、`openai_responses.go:464-467`）
    —— 语义一致。

---

## 附：本次审计的方法说明

所有 P1 结论均经过**独立复现**，复现环境在 `%TEMP%` 下用独立 Go module：

- `%TEMP%\a3tool` —— 用 `rosetta@v1.2.0` 直连 `httptest` 假上游，观察 SDK 交付给
  网关的统一事件（ToolIndex 语义、ReasoningID 边界、OutputIndex 填充、
  非法 arguments 的落地形态）。
- `%TEMP%\a3sink` —— 把 `internal/outwire/*.go`（去掉 `_test.go`）复制成独立包
  `a3sink/outwire`，直接喂 `rosetta.Event`，观察三种协议的实际线格式输出。

仓库内**未做任何写入**（`work/` 已在 `.gitignore` 中，报告文件本身也不受跟踪）。
