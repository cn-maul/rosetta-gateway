# 思考方言对照表（官方文档口径）

本文件记录 `internal/dialect` 里每条规则**出自哪份厂商文档**，以及三协议
（chat / responses / anthropic）在目标方言下的实际出站字段。

它的作用不是替代代码注释，而是解决一个具体的失效模式：方言表曾经混杂
「厂商文档」和「某个中转站实测」两种依据，两者冲突时看不出谁对谁错。
以后改表必须先在这里补出处；没有出处的改动视为猜测。

> 核对日期：**2026-11-11**。厂商文档会变，改表前请重新打开链接确认，
> 不要把本文件当成永久事实。

## 出处

| 厂商 | 官方页面 |
|---|---|
| OpenAI | Chat / Responses 的 `reasoning_effort`、`reasoning.effort`（需代理访问） |
| Anthropic | Messages API 的 `thinking` / `output_config`（需代理访问） |
| DeepSeek | `https://api-docs.deepseek.com/guides/thinking_mode`、`/guides/anthropic_api` |
| GLM（智谱） | `https://docs.bigmodel.cn/cn/guide/capabilities/thinking.md` |
| Qwen（百炼） | `https://help.aliyun.com/zh/model-studio/deep-thinking`、`/qwen-api-via-openai-responses` |
| MiniMax | `https://platform.minimaxi.com/docs/api-reference/text-chat-openai.md`、`/responses-create.md` |
| Kimi | `https://platform.kimi.com/docs/guide/use-thinking-models.md`、`/docs/api/chat.md` |

## 各家写法

### DeepSeek

Chat 入口（官方 thinking mode 页）：

```json
{"thinking": {"type": "enabled" | "disabled"}, "reasoning_effort": "low|high|max"}
```

Responses 入口（同一页的对照表）：

```json
{"reasoning": {"effort": "none|low|high|max"}}
```

- `none` 关闭思考；`thinking.type` 是另一条独立的开关路径。
- 档位映射：`minimal → low`、`medium/xhigh → high`、`ultra → max`。
  网关把 `ultra` 归一到 `max`，因为发出去的就是上游实际执行的档位。
- Anthropic 兼容端点：`thinking.type` 支持 `enabled|disabled`，
  **`budget_tokens` 被忽略**，强度只认 `output_config.effort`。

### GLM（智谱）

```json
{"thinking": {"type": "enabled" | "disabled"}, "reasoning_effort": "max|high|low"}
```

- **GLM-5.3 / 5.3-FLASH 不能关闭**：`thinking.type:"disabled"` 直接报错。
  网关因此把「关」降级成最低档 `low`，并记 WARN。
- 5.3 的 `reasoning_effort` 只接受 `max|high|low`，收到 `medium` 会报错；
  5.2 及以下接受全部档位并自行映射。
- `thinking.clear_thinking` 是厂商扩展，客户端带来时原样转发。

### Qwen（百炼）

```json
{"enable_thinking": true | false, "reasoning_effort": "none|minimal|low|medium|high|xhigh|max"}
```

- Chat 入口历史上以 `enable_thinking` 为主；Responses 入口用
  `reasoning.effort`，官方写明后者优先级更高、前者后续不再支持。
  网关两者都发，保证新老模型都能生效。
- 「仅思考模式」的模型（如 `qwen3.8-2.4t-a95b`）无法关闭，也不需要发
  `enable_thinking`。
- `qwen3.8-omni-flash` 的默认档是 `xhigh`；`minimal` 映射为 `low`，
  `high`/`max` 映射为 `xhigh`。

### MiniMax

```json
{"thinking": {"type": "adaptive" | "disabled"}, "reasoning_effort": "low|medium|high|xhigh|max"}
```

- `thinking.type` 的枚举里**没有 `enabled`**，`adaptive` 才是默认值。
  网关在开启态写 `adaptive`，客户端带来的 `enabled` 也会收敛成它。
- `MiniMax-M3.1-Flash` 强制思考，`disabled` 返回 400，`none` 也返回 400；
  最低可发档位是 `low`。
- `MiniMax-M3` 接受 `adaptive` / `disabled`。

### Kimi（月之暗面）

K3 系列（`kimi-k3`、`kimi-k3.1-pro`、`-highspeed`）：

```json
{"reasoning_effort": "low|high|max"}
```

- **只有 `reasoning_effort`，没有 `thinking`**。官方 API 参考用 `oneOf`
  把 K3 与 K2.x 分成两个 schema：`thinking` 属于 K2.x 的
  `ChatRequestBase`。给 K3 发 `thinking` 是往不接受的 schema 里塞字段，
  网关会替客户端删掉。
- K3 始终思考、始终开启 Preserved Thinking，无法关闭。

K2.x（`kimi-k2.6`、`kimi-k2.7-code`）：

```json
{"thinking": {"type": "enabled" | "disabled", "keep": null | "all"}}
```

- 官方对照表把 `reasoning_effort` 标为**不支持**（K2.x 两列都是「—」）。
  网关因此不回发该字段。
- `kimi-k2.7-code` 只能 `type:"enabled"`，传 `disabled` 报错；
  `keep` 固定为 `"all"`。
- `kimi-k2.6` 默认 `enabled`，可关；`keep` 默认 `null`。

### OpenAI / Anthropic

- Chat：`reasoning_effort`，没有 `thinking` 字段；关闭用 `"none"`。
- Responses：`reasoning.effort`，关闭同样是 `"none"`；`include`
  （如 `reasoning.encrypted_content`）挂在 reasoning 配置上，关思考时不能
  一并抹掉。
- Anthropic Messages：`thinking:{type:"enabled",budget_tokens:N}` /
  `{"type":"disabled"}`；新版 API（`2026-10-21`）移除了
  `temperature`/`top_p`/`top_k`/`stop_sequences`/`service_tier`。

## 三协议 × 关闭思考

| 目标 | 关闭写法 |
|---|---|
| OpenAI chat | `reasoning_effort:"none"` |
| DeepSeek chat | `thinking:{type:"disabled"}` |
| GLM chat | `thinking:{type:"disabled"}`（5.3 不行 → 降到 `low`） |
| Qwen chat | `enable_thinking:false`（且不发 `reasoning_effort`） |
| MiniMax chat | `thinking:{type:"disabled"}`（M3.1-Flash 不行 → 降到 `low`） |
| Kimi K3 chat | 无关闭写法（`reasoning_effort` 最低 `low`） |
| Kimi K2.6 chat | `thinking:{type:"disabled"}` |
| OpenAI responses 目标 | `reasoning.effort:"none"` |
| Anthropic 目标 | `thinking:{type:"disabled"}` |
| Anthropic 目标 + 厂商兼容端点 | `thinking:{type:"disabled"}`，强度走 `output_config.effort` |
| 方言未知 | 只发协议标准字段；厂商文档说「不能关」的模型干脆不发 |

## 改这张表的规矩

1. 先按上表打开官方文档，确认字段名与取值枚举；不要从别处的代码注释反推。
2. 官方文档与实际行为冲突时，用 `can_disable_thinking` 这类**按模型的三态
   纠正**表达实测结果，不要直接改写文档表 —— 否则换一家中转站就全错。
3. 新规则必须同时补一条 `internal/ingress` 的用例，断言**上游真正收到的
   字节**（`probeWireModel`），而不是只测中间变量。
