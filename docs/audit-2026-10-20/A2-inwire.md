# A2 入站协议层审计报告（internal/inwire/ + internal/effort/）

- 审计日期：2026-10-20
- 仓库：rosetta-gateway，main = ea19ce1
- 覆盖文件（全量，无遗漏）：
  - `internal/inwire/openai_chat.go`（604 行）
  - `internal/inwire/openai_responses.go`（1255 行）
  - `internal/inwire/anthropic.go`（588 行）
  - `internal/inwire/decode.go`（90 行）
  - `internal/effort/effort.go`（283 行）
  - 另读了同目录的测试文件（`openai_chat_test.go`、`openai_chat_audit_test.go`、
    `openai_responses_test.go`、`anthropic_test.go`、`repro_reasoning_effort_test.go`、
    `effort_test.go`），以及跨层契约的对照物：`cmd/gateway/main.go`、
    `cmd/gateway/replay_gate.go`、`internal/outwire/*`、`internal/effort/*`、
    `internal/upstream/upstream.go`、`rosetta v1.2.0` SDK 源码。
- 方法：全文阅读 + `rg` 精确串检索 + 在 `work/a2probe/`（gitignored）写只读探针程序，
  走真实 `rosetta.Client` 出站路径抓取实际 wire payload，验证每条猜想。
  未修改仓库内任何 git 跟踪文件，未执行任何 git 写操作。

## 结论概览

| 级别 | 数量 |
|---|---|
| P0 | 1 |
| P1 | 6 |
| P2 | 10 |
| P3 | 4 |

本次审计确认了一条**系统性根因**：思考挡位（effort）在本网关里**存在三套并行的
解析口径**——`internal/effort` 包、`inwire.parseEffort`、以及 Anthropic 入站的
预算→档位折算表——它们彼此不共享代码，于是同一个客户端挡位写法在三条入口上
得到三种不同的结果，夹取（Clamp）层的「唯一口径」承诺在数据面上并不成立。
详见 P1-1 与 P1-2。

第二类根因是**媒体块（image / file / audio）在三套入站协议之间的处理严重不对称**：
OpenAI Chat 只认 image_url，Responses 认 image/file/audio，Anthropic 认 image/document
但 tool_result 内的媒体被丢弃，Chat 的 audio/file 整块被丢。三处「丢法」各不相同，
但症状同形：请求 200、内容消失、无任何日志。这正是 DESIGN §7.2 想消灭的
「无声吞内容」，只是漏在了另外两个入口。

---

## P0

### P0-1 客户端用 `max` 档位时，请求被静默降级为「完全不思考」

- 位置：`internal/inwire/openai_chat.go:632-646`（`parseEffort`）、
  `internal/inwire/openai_chat.go:233`、`internal/inwire/openai_responses.go:652`
- 现象：客户端发 `reasoning_effort: "max"`（或 Responses 的
  `reasoning.effort: "max"`），网关**不设** `Thinking`，
  上游收到的 payload 里**根本没有 `reasoning_effort` 字段** ——
  即模型完全不思考，而客户端以为自己开了最高档思考。

代码证据（`internal/inwire/openai_chat.go:632`）：

    func parseEffort(s string) rosetta.Effort {
        switch strings.ToLower(strings.TrimSpace(s)) {
        case "minimal":
            return rosetta.EffortLow
        case "low":
            return rosetta.EffortLow
        case "medium":
            return rosetta.EffortMedium
        case "high", "xhigh":
            return rosetta.EffortHigh
        default:
            return rosetta.EffortUnset   // ← "max" 落到这里
        }
    }

`internal/effort/effort.go:93-118` 的 `normalizeAlias` 明确把 `max`/`maximum`
当作**已识别**的最高档（`Max` 是 `KnownLevels` 的最后一档，`effort.go:66`），
`effort.Parse("max")` 返回 `Max`。但 `inwire.parseEffort` 的 switch 里**没有
`max` 分支**。两套口径对同一输入给出相反答案。

实测（探针，走真实 `rosetta.Client` 出站）：

    effort=low        Thinking=&{low 0 }   upstream: {"reasoning_effort":"low"}
    effort=xhigh     Thinking=&{high 0 }   upstream: {"reasoning_effort":"high"}
    effort=max       Thinking=<nil>        upstream: {"messages":[...],"model":"m"}   ← 无 reasoning_effort
    effort=minimal   Thinking=&{low 0 }    upstream: {"reasoning_effort":"low"}
    effort=very_high Thinking=<nil>        upstream: {"messages":[...],"model":"m"}   ← 无 reasoning_effort
    effort=off       Thinking=<nil>        upstream: {"messages":[...],"model":"m"}

严重性升级为 P0 的理由不止是「思考强度不对」：

1. **不是静默降一档，是静默降为「零」**。`xhigh → high` 至少方向正确；
   `max → 无思考` 是把用户的显式意图整个抹掉。
2. **与 `effort` 包的公开承诺直接冲突**。`internal/effort/effort.go:48-66` 的
   注释明确写着「`max` 是独立档（比 xhigh 更强）」，且
   `internal/upstream/upstream.go:903-912` 的模型发现会从上游的
   `reasoning_supported_efforts=[low,medium,high,xhigh,max]` 里读出 `max`
   并落库为 `EffortLevels`；`cmd/gateway/main.go:3660-3667` 的
   `/v1/models` 还会把 `max` 作为 `supported_reasoning` 披露给客户端。
   **网关明确告诉客户端「支持 max」，然后把客户端发的 max 丢成零。**
3. **夹取层救不回来**。`cmd/gateway/main.go:1453-1455` 的
   `applyThinkingCapability` 第一行就是 `if m == nil || req.Thinking == nil { return }`。
   `req.Thinking` 为 nil ⇒ 整个函数直接返回 ⇒ 既不夹取、也不写
   `EffortRaw`、也不记那条「clamped ...」的 Warn 日志。
   **即使模型只支持 `[low,medium,high]`，客户端要 `max` 也应该被夹到 `high`，
   实际却是「不思考」且日志无痕。**
4. 同理受影响的还有 `very_high` / `extra_high` / `x-high` / `x_high` /
   `min` / `mid` / `default` / `maximum`（`effort.Parse` 能识别其中大部分，
   `parseEffort` 全部返回 Unset）。
   其中 `min`/`mid`/`default` 尤其恶劣：它们是**完全合法**的客户端写法
   （`effort.normalizeAlias` 自己就把 `mid`→medium、`default`→medium、
   `min`→minimal），却被判为「未知值」而丢掉整个思考请求。

修复方向（不改代码，仅建议）：`parseEffort` 必须**以 `effort` 包为唯一事实来源**，
而不是自己维护一张 switch。最小改动是让 `parseEffort` 改为
`effort.Parse(s)` → `effort.ToRosettaLevel(...)` 的映射；
根本改动是删掉 `inwire.parseEffort`，把「原始档位 → rosetta 三档」的映射
全部下沉到 `internal/effort`（`ToRosettaLevel` 已经存在且已被
`cmd/gateway/main.go:1497` 使用），由 inwire 只做透传。

---

## P1

### P1-1 effort 口径三分裂：`internal/effort` / `inwire.parseEffort` / Anthropic 预算折算表

- 位置：`internal/effort/effort.go`（全包）、`internal/inwire/openai_chat.go:632`、
  `internal/inwire/anthropic.go:165-175`
- 这是本次审计最主要的**架构性根因**。`internal/effort` 的包注释
  （`effort.go:1-27`）宣称自己是「思考挡位的唯一口径」，但实际链路上有三套
  各自独立的解析实现：

  | 实现 | 位置 | 认得的值 | 归一结果 |
  |---|---|---|---|
  | `effort.Parse` / `normalizeAlias` | `effort.go:93-118` | none/off/disabled、minimal/min、low、medium/mid/default、high、xhigh/x-high/x_high/extra_high/very_high、max/maximum | 6 档刻度 |
  | `inwire.parseEffort` | `openai_chat.go:632` | minimal、low、medium、high、xhigh | 3 档 rosetta |
  | Anthropic 预算折算 | `anthropic.go:165-175` | budget_tokens <=4096→low，<=16384→medium，else→high | 3 档 rosetta |

  实测三方对照（探针）：

      value        effort.Parse  inwire(parseEffort)  一致?
      max          max           NIL(无思考)          MISMATCH
      min          minimal       NIL(无思考)          MISMATCH
      mid          medium        NIL(无思考)          MISMATCH
      default      medium        NIL(无思考)          MISMATCH
      very_high    xhigh         NIL(无思考)          MISMATCH
      extra_high   xhigh         NIL(无思考)          MISMATCH
      x-high       xhigh         NIL(无思考)          MISMATCH
      maximum      max           NIL(无思考)          MISMATCH

- 为什么是 P1 而非 P2：这是**多个 agent 各写一套**的典型根因，且已经产生了
  P0-1 这个线上可见的后果。`internal/effort/effort.go:9` 的注释甚至点名
  「客户端发 xhigh 会在 inwire.parseEffort 里被压成 high」——说明作者**知道**
  `parseEffort` 存在、也知道它与 `effort` 包是两条路，却选择了「保留两套」
  而不是「让一套调用另一套」。
- 附带证据：`internal/effort/effort.go:9` 的注释描述的是旧行为
  （xhigh→high），而 `effort.go:48-66` 又新增了 `max` 档；
  `effort_test.go` 只测 `effort` 包自己，**没有任何测试跨包比对
  `effort.Parse` 与 `inwire.parseEffort` 的一致性**——这是测试覆盖的缺口，
  正是这类分歧能长期存活的原因。
- 建议：删掉 `inwire.parseEffort`，改为 `inwire` 调 `effort.Parse` +
  `effort.ToRosettaLevel`；并在 `internal/effort` 里加一条跨包一致性断言测试
  （对 `KnownLevels` 全集 + 全部别名，断言 `inwire` 产出的档位与
  `effort` 包的结论一致）。`effort.ToRosettaLevel` 已经现成可用，
  `cmd/gateway/main.go:1497` 已在用它。

### P1-2 `inwire` 的 effort 归一结果被数据面丢弃，导致「夹取」在多数情况下不生效

- 位置：`internal/inwire/openai_chat.go:233`、
  `internal/inwire/openai_responses.go:652`、`cmd/gateway/main.go:1453-1512`
- 现象：`inwire.ToRosetta` 把 `reasoning_effort` 归一到三档写进
  `req.Thinking.Effort`；`cmd/gateway/main.go:1511-1512` 随后
  `req.Thinking.EffortRaw = string(applied); req.Thinking.Effort = rosetta.EffortUnset`，
  **把 inwire 归一出的三档整个清空**，只留 `EffortRaw`。
  于是 inwire 里那张 `parseEffort` 的 switch 表在**数据面上完全不起作用** ——
  它只在「inwire 归一失败 ⇒ Thinking == nil ⇒ 整条思考被丢」这一条路径上起作用，
  而那恰恰是 P0-1 的病根。
- 换句话说：**inwire 的 effort 归一是死代码（除了「决定思考是否存在」这一位），
  真正决定发出去什么的是 `cmd/gateway/main.go` 的 `applyThinkingCapability`，
  而它读的是 `ing.rawEffort`（原始字符串），不是 inwire 归一后的结果。**
- 建议：让 inwire **只**负责产出 `RawEffort`（原样字符串），不再产出
  `Thinking.Effort`；把 `Thinking` 的存在性与 `Effort` 的赋值全部交给
  `applyThinkingCapability`。这样 `parseEffort` 可以直接删除，
  口径自然收敛到一处（与 P1-1 的建议同源）。

### P1-3 Anthropic 入站的 thinking 预算被静默改写，且改写幅度可达 12 倍

- 位置：`internal/inwire/anthropic.go:160-175`（预算→档位）、
  `internal/inwire/anthropic.go:265`（`ToRosetta` 填 `BudgetTokens`）、
  `cmd/gateway/main.go:1493-1510`（anthropic 分支清零 `BudgetTokens`）
- 现象链：客户端在 `/v1/messages` 发 `thinking.budget_tokens: 100000`
  （一个明确的大预算），`inwire` 把它折成档位 `"high"`；
  `applyThinkingCapability` 的 anthropic 分支把 `BudgetTokens` **清零**，
  改用 `effort.ToRosettaLevel("high")` = `"high"`；SDK 收到三档 high 后
  按自己的折算表换成 **32768**。客户端的 100000 变成 32768，无任何提示。
- 实测（真实 Anthropic 协议出站）：

      budget=2048   rawEffort=low    upstream thinking.budget_tokens=2048
      budget=5000   rawEffort=medium upstream thinking.budget_tokens=8192   （↑）
      budget=20000  rawEffort=high   upstream thinking.budget_tokens=32768  （↑）
      budget=100000 rawEffort=high   upstream thinking.budget_tokens=32768  （↓ 3 倍）

- 危害有两层：
  1. **语义漂移**：预算 5000 被放大到 8192（客户端要省，网关给它花更多，
     成本上升）；预算 100000 被压到 32768（客户端要深度思考，网关给它更浅，
     质量下降）。两个方向都破坏客户端意图，且**方向不一致**。
  2. **思考预算与 `max_tokens` 联动放大**：SDK 的 `plan()` 在
     `budget >= maxTokens` 时会把 `max_tokens` 涨到 `budget+4096`。
     实测客户端 `max_tokens=100` + `budget_tokens=5000` 时，
     上游收到 `max_tokens: 12288`（100 被放大 122 倍）。
     这直接影响**计费**：max_tokens 决定了上游的输出上限。
- 根因：`inwire` 把「预算」当成「档位」处理，而这两个概念不可逆互转
  （`anthropic.go:45-50` 的注释承认了这点却仍继续做双向折算）。
  正确做法是：**Anthropic→Anthropic 时预算应当原样透传**，
  只有在需要跨到 OpenAI 系（那里只有档位没有预算）时才折算成档位，
  且折算方向应是「取能满足客户端意图的最接近档位」而不是「先压成三档再让
  SDK 用另一张表折回来」。
- 附注：`cmd/gateway/main.go:1508` 的 `req.Thinking.BudgetTokens = 0`
  是在 **attempt 内**执行的（每个目标都跑一遍），所以 Anthropic→Anthropic
  这条本该零损耗的路径也被无差别地走了折算。

### P1-4 Anthropic `tool_result` 内的媒体块被静默丢弃

- 位置：`internal/inwire/anthropic.go:623-641`（`toolResultText`）、
  `internal/inwire/anthropic.go:465-472`
- 现象：Anthropic 协议允许 `tool_result.content` 是块数组，块里可以有
  `image`。`toolResultText` 只抽 `text` 块，`image` 块被静默丢掉。
- 实测：

      入站: {"type":"tool_result","tool_use_id":"t1","content":[
               {"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}},
               {"type":"text","text":"caption"}]}
      解出: role=tool type=tool_result content="caption"    ← 图片消失
      出站: {"messages":[{"content":"caption","role":"tool","tool_call_id":"t1"}],...}

- 危害：agent 场景下「工具返回了一张截图，模型应该看这张截图」是极常见的
  用法。图片被丢后模型只看到 caption 文字，**请求 200、回答正常**，
  客户端与模型都察觉不到。对比 DESIGN §7.2 明确把
  「工具输出与用户附件的媒体块」列为 R12 落点并已为 Responses 入口实现
  （`openai_responses.go:930-968` 的 `responsesItemToolOutput` 会把媒体
  搬进紧随其后的 user 消息）—— **Anthropic 入口是同一个需求的漏网之鱼**。
- 建议：复用 Responses 入口那套「拆文本 + 媒体另起一条 user 消息 +
  留一行说明」的做法（`responsesToolMediaNotice` 已在
  `openai_responses.go:1013` 附近），在 `convertUserMessage` 的
  `tool_result` 分支里对 `b.Content` 走同一套分块解析。

### P1-5 Responses 入站的 `input_file` 只带 `file_id` 时，整个请求被网关判死

- 位置：`internal/inwire/openai_responses.go:971-991`（`responsesFileBlock`）
- 现象：`responsesFileBlock` 用 `firstNonEmpty(p.FileData, p.FileURL, p.FileID)`
  取值，然后**同时**写 `FileData: data` 和 `FileID: p.FileID`。
  当载荷只有 `file_id`（OpenAI 官方 Responses 的合法形态：文件已在 provider
  侧上传，引用其 id）时，`data == p.FileID == "file-abc"`，
  于是产生一个「FileData 和 FileID 同时有」的块。
- 实测：

      入站: {"model":"m","input":[{"type":"message","role":"user",
             "content":[{"type":"input_file","file_id":"file-abc"}]}]}
      decode err=<nil>              ← 入站解码静默通过
      统一模型: type=file fileData="file-abc" fileID="file-abc"   ← 两个源都有
      出站: rosetta: invalid request: Messages[0]: blocks[0] (file):
            file block has both FileData and FileID; set exactly one source

- 危害：这是 **inwire 造出一个必然被 rosetta 拒绝的统一模型**。
  错误消息是 SDK 内部的措辞（`file block has both FileData and FileID`），
  客户端无从知道是自己少发了 `file_data` 还是别的什么。
  症状是「请求 400，但网关日志里入站解码一切正常」。
- 根因：`firstNonEmpty` 的三选一语义与「同时填两个字段」的赋值方式互相矛盾。
  修法：`file_id` 形态应**只**填 `FileID`（`FileData` 留空），
  `file_url` 形态只填 `FileData`，`file_data` 形态只填 `FileData` ——
  三者互斥，与 rosetta `Block.validate()` 的「exactly one source」要求对齐。
  同函数下方的 `responsesAudioBlock`（`openai_responses.go:993`）有同样问题
  （见 P2-3）。

### P1-6 Responses 入站 `tool_choice` 指向 namespace 时，该约束被静默丢弃

- 位置：`internal/inwire/openai_responses.go:579-628`（`rewriteToolChoice`）、
  `internal/inwire/openai_responses.go:1141-1146`（`effectiveToolChoice`）
- 现象：decode 阶段明确接受 `{"type":"namespace","name":"shell"}` 形态的
  `tool_choice`（`openai_responses.go:338-343` 的注释说
  「namespace 也接受：客户端可以用命名空间强制『在这个组里选』，
  展平后这个约束由 rewriteToolChoice 兑现」）。
  但 `rewriteToolChoice` 的实现只处理 `obj.Type != "function"` 的分支：

      if json.Unmarshal(a.ToolChoice, &obj) != nil || obj.Type != "function" || obj.Name == "" {
          return nil // 字符串形态或非 function 选择，与展平无关
      }

  `type == "namespace"` 落进这个 `return nil`，于是**约束从未被兑现**，
  原始 `{"type":"namespace","name":"shell"}` 被原样发给上游
  —— 而上游的工具表里只有展平后的 `shell__ls`，没有名为 `shell` 的工具。
- 实测：

      入站: tools=[{namespace "shell" tools=[{function "ls"}]}], tool_choice={"type":"namespace","name":"shell"}
      展平后工具表: shell__ls
      出站 payload: "tool_choice":{"name":"shell","type":"namespace"}   ← 约束原样透传，指向不存在的工具

  上游收到「强制调用 shell」而表里只有 `shell__ls`，结果要么 400，
  要么**静默退回 auto** —— 后者正是 DESIGN §7.3 明令要避免的
  「客户端要强制，实际拿到模型自由选择，且无任何信号」。
- 根因：decode 的白名单（接受 namespace）与 rewrite 的实现（只处理 function）
  不一致，属同一改动（784b517）内部的半成品。
- 建议：`rewriteToolChoice` 增加 `obj.Type == "namespace"` 分支 ——
  在展平后的表里找出所有 `ns.Name + "__"` 前缀的候选：
  恰好一个 ⇒ 改写成该展平名；多于一个 ⇒ 保持原值（歧义不猜）；
  一个都没有 ⇒ 保持原值。

---

## P2

### P2-1 三个入站协议的 `tool_choice` 翻译矩阵不一致，语义在跨协议时漂移

- 位置：`internal/inwire/openai_chat.go:397-451`（chat 的两个翻译函数）、
  `internal/inwire/anthropic.go:343-380`（anthropic 的两个翻译函数）、
  `internal/inwire/openai_responses.go:1170-1240`（responses 的两个翻译函数）
- 实测三入站 × 三出站的完整矩阵（探针）：

                          → openai-chat        → openai-responses   → anthropic
    anthropic(none)         none                (丢弃)               {"type":"none"}
    anthropic(any)          required            required             {"type":"any"}
    chat(none)               none                none                 (丢弃)
    responses(none)         none                "none"               (丢弃)

- 三个方向各有一次「丢弃」，但**被丢弃的方向不一致**：
  - Anthropic 的 `none`（禁止调用已声明工具）在 chat 上**保留**为 `"none"`，
    在 responses 上**丢弃**。同一个 Anthropic 客户端，故障转移前后
    「是否禁止调工具」的行为不一致 —— 这与 `openai_chat.go:268-280`
    注释里说要消灭的「故障转移前后同一请求行为不一致」是同一类问题。
  - Chat 的 `none` 在 anthropic 上丢弃（`chatToolChoiceAnthropic`
    的 `default` 分支，`openai_chat.go:434-440`），注释说「Anthropic 无法表达」。
    但 Anthropic **其实有** `tool_choice: {"type":"none"}`（Anthropic 官方
    支持 auto/any/tool/none 四种），而同一文件
    `anthropic.go:351-357` 的 `openAIValue` 又把 Anthropic 的 none
    翻译成 chat 的 `"none"`。**同一个协议对里，none 在两个方向上一个被保留、
    一个被丢弃。**
- 建议：建立一张**单一**的三×三 tool_choice 语义映射表（放在一个文件里），
  以「Anthropic 的 none == chat 的 none == responses 的 none」
  为前提统一处理；若确认 Responses 的 none 语义确实不同，则应在
  网关层**报错**（这是控制位，不是软偏好），而不是静默丢弃。

### P2-2 OpenAI Chat 入站只认 `image_url`，`input_audio` / `file` 块被静默丢弃

- 位置：`internal/inwire/openai_chat.go:532-540`（`openAIContentPart`）、
  `internal/inwire/openai_chat.go:553-594`（`extractUserContent`）
- 现象：`openAIContentPart` 只声明了 `type`/`text`/`image_url`，
  `extractUserContent` 的 switch 也只有 `case "text"` 与 `case "image_url"`。
  OpenAI Chat 协议的 `input_audio` 与 `file` 分块被 `encoding/json` 静默忽略。
- 实测：

      入站: content=[{"type":"text","text":"listen"},
                      {"type":"input_audio","input_audio":{"data":"aGk=","format":"wav"}}]
      出站: {"messages":[{"content":"listen","role":"user"}],...}   ← 音频消失，无错误

  纯音频（无文本）时更糟：解出的 user 消息没有任何有效块，
  rosetta 报 `invalid request: Messages[0]: message has no content blocks` → 400，
  错误消息指向内部结构，客户端不知道自己漏发了什么。
- 对比：`openai_chat.go:540-551` 的注释花了很大篇幅讲「旧实现把 image_url
  静默丢掉」是错的、要改成 `rosetta.BlockImage` —— **同一段代码对 audio/file
  犯了完全一样的错**。而且 rosetta 侧**已经支持** audio（`provider_openai_chat.go:383`
  的 `BlockAudio` 分支会编成 `input_audio`）与 file（`provider_openai_chat.go:392`
  编成 `file`），inwire 却没把这两类块喂进去。
- 建议：在 `openAIContentPart` 里补 `input_audio` 与 `file` 字段，
  在 `extractUserContent`/`userMessage` 里产出 `rosetta.BlockAudio` /
  `rosetta.BlockFile`，与 `openai_responses.go:844-860` 的
  `input_file`/`input_audio` 处理对齐。

### P2-3 Responses 入站的 `input_audio` 远端 URL 被当成本地 base64，导致 400

- 位置：`internal/inwire/openai_responses.go:993-1010`（`responsesAudioBlock`）
- 现象：`responsesAudioBlock` 用 `firstNonEmpty(p.AudioData, p.AudioURL)`，
  远端 URL 被塞进 `AudioData`。但 rosetta 的 `BlockAudio` 语义是
  「**内联** base64」，`validate()` 会用 `isBase64` 校验
  （`rosetta/message.go` 的 `BlockAudio` 分支）。
- 实测：

      入站: content=[{"type":"input_audio","audio_url":"https://x/a.wav","format":"wav"}]
      decode err=<nil>
      出站: rosetta: invalid request: Messages[0]: blocks[0] (audio):
            audio block AudioData is not valid base64

- 与 P1-5 同源：`firstNonEmpty` + 单一目标字段 的模型没有区分
  「内联」与「引用」两种载荷形态。修法：远端 URL 形态应当**不支持**
  （responses 协议也不支持音频输入 —— `provider_openai_responses.go:417`
  的 `encodeResponsesUser` 对 `BlockAudio` 明确报
  「responses protocol does not support audio input」），
  所以正确行为是**在入站就明确 400**（说清「Responses 协议不支持音频输入，
  请改用 Chat Completions」），而不是造出一个必然被 SDK 拒绝的块。

### P2-4 Anthropic 入站的 `document` 块（source.type=text）被静默丢弃

- 位置：`internal/inwire/anthropic.go:599-621`（`fileBlock`）
- 现象：`fileBlock` 只处理 `source.type` 为 `base64` 与 `url` 两种，
  Anthropic 官方还支持 `source.type: "text"`（`{"type":"text","media_type":"text/plain","data":"<base64>"}`）。
  该形态落到 `default` → 返回 `false` → 块被丢弃。
- 实测：

      入站: content=[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"aGVsbG8="}}]
      解出: role=user type=text text=""        ← 整块消失
      出站: rosetta: invalid request: Messages[0]: message has no content blocks   ← 400

  注意 rosetta 侧**是支持**这个形态的：`provider_anthropic.go:556-560`
  的 `encodeAnthropicDocument` 明确处理 `media == "text/plain"`，
  把它编成 Anthropic 的 `{"type":"text"}` source。**只有 inwire 这一侧不认。**
- 危害：文本文件（.txt 等）走 `/v1/messages` 时整个请求 400，
  且入站 decode 报「一切正常」——错误来源完全不指向 inwire。
- 建议：`fileBlock` 补 `case "text"` 分支，产出
  `rosetta.Block{Type: BlockFile, MimeType: "text/plain", FileData: <base64>}`。

### P2-5 Anthropic `tool_result` 的 `cache_control` 断点被丢弃

- 位置：`internal/inwire/anthropic.go:465-472`（`convertUserMessage` 的
  `tool_result` 分支）、`internal/inwire/anthropic.go:623-641`（`toolResultText`）
- 现象：`tool_result` 分支构造 `rosetta.Block{Type: BlockToolResult, ...}`
  时**没有带** `CacheControl`，而同一函数里 `text` / `image` / `document`
  分支都带（`anthropic.go:473`、`anthropic.go:479`、`anthropic.go:485`）。
  rosetta 侧 `provider_anthropic.go:393-399` 的 `BlockToolResult` 分支
  是支持 `cache_control` 的。
- 实测：`{"type":"tool_result","tool_use_id":"t1","content":"ok","cache_control":{"type":"ephemeral"}}`
  出站为 `{"content":"ok","role":"tool","tool_call_id":"t1"}` —— 断点消失。
- 危害：Claude Code 一类重度依赖提示缓存的客户端会少命中一段缓存，
  **成本上升但没有任何错误信号**。这属于「客户端发了、inwire 吞了」——
  与 P1-4 属于同一类漏点。

### P2-6 Chat 入口只有 `n` 被显式处理，其余协议私有字段一律静默丢弃

- 位置：`internal/inwire/openai_chat.go:44-52`、`internal/inwire/openai_chat.go:191-208`
  vs `internal/inwire/openai_responses.go:281-320`
- 现象：Chat 入口对 `n > 1` 明确 400，注释写得很好
  （「rosetta 与三个上游协议都不支持多候选，ToRosetta 也从不读这个字段」）。
  但**其他 Chat 私有字段**（`logprobs`、`service_tier`、`metadata`、
  `store`、`logit_bias`、`modalities`）既不声明也不拒绝 ——
  它们被 `encoding/json` 静默忽略。
- 实测：

      入站: {"model":"m","messages":[...],"stop":["END"],"logprobs":true,
             "service_tier":"priority","metadata":{"k":"v"},"store":true,
             "stream_options":{"include_usage":true}}
      Extra=map[]            ← 全部私有字段都没进 Extra
      出站: {"messages":[...],"model":"m","stop":["END"]}   ← 只剩 SDK 自己建模的字段

  客户端发了 `logprobs: true` 却拿不到 logprobs，`service_tier: "priority"`
  的优先级也没了，且没有任何提示。
- Responses 入口同样：`include` / `max_tool_calls` / `truncation` /
  `service_tier` / `metadata` 全被忽略（`openai_responses.go:87` 的注释
  自己承认了「metadata / service_tier / safety_identifier 等未建模字段由
  encoding/json 忽略」）。
- 建议：与 `n` 的处理保持一致口径：要么透传（作为 Extra），
  要么显式 400。**当前是第三种状态：静默丢弃**，而这正是本项目反复修过的
  失败模式。

### P2-7 Responses 入站的 `instructions` 与 system item 会**重复**下发

- 位置：`internal/inwire/openai_responses.go:641-646`（`System: a.Instructions`）、
  `internal/inwire/openai_responses.go:792-810`（`convertResponsesMessage` 的
  `system`/`developer` 角色分支）
- 现象：Responses 的 `instructions` 字段被填进 `req.System`（统一模型的
  顶层 system 字段），而 `input` 数组里 `role:"system"`/`"developer"` 的
  item 被转成 `rosetta.System(...)` 消息。
  两者在 SDK 出站时**都**会被编码成 system：
  - `provider_openai_responses.go:311-316`：`req.System` → 一条
    `{"type":"message","role":"system","content":...}` item
  - `provider_openai_responses.go:317-321`：`RoleSystem` 消息 → 又一条
    `{"type":"message","role":"system","content":...}` item
  两条 system item 同时出现在 input 里。
- 对比 Anthropic 入口（`anthropic.go:217-243`）：`system` 字段为块数组时
  走 `RoleSystem` 消息、否则走 `req.System` —— **二选一，不重复**。
  Chat 入口（`openai_chat.go:478`）只有 `RoleSystem` 一条路。
  只有 Responses 会同时用两条路。
- 危害：客户端同时发 `instructions` 与 `{"role":"system"}` item 时，
  system 提示被**写两遍**（token 浪费 + 指令权重被放大）。
  这是「三套协议字段翻译不对称」的典型实例。
- 建议：Responses 入口统一走一条路（建议 `instructions` → `req.System`，
  system/developer item → 也并入 `req.System` 或统一为 `RoleSystem`，
  二选一）。

### P2-8 跨协议硬约束的判定只覆盖「结构化输出」一项，其余硬约束仍走静默丢弃

- 位置：`cmd/gateway/main.go:2050-2071`（硬约束过滤）、
  `internal/inwire/openai_chat.go:462-476`（`RequiresStructuredOutput`）、
  `internal/inwire/openai_responses.go:1314-1322`（同名方法）
- 现状：网关层只有一个 `requiresStructuredOutput` 硬约束闸门。
  Anthropic 入站的 `requiresStructuredOutput` **恒为 false**
  （`cmd/gateway/main.go:1682`，注释说「Anthropic 入站协议没有结构化输出概念」），
  这本身合理。但下面这些同样是**硬约束**的字段，没有对应的闸门：
  - Chat 的 `tool_choice: "none"`（禁止调工具）→ anthropic 上被丢弃（P2-1）
  - Responses 的 `tool_choice: {"type":"namespace"}` → 落到不存在的工具名（P1-6）
  - Chat 的 `stop` → responses 上**被 SDK 丢弃**
    （`provider_openai_responses.go:278-280`：
    「responses: stop sequences are not supported by this protocol; dropping them」）。
    `stop` 是**生成约束**（不是软偏好）：客户端要求模型在 "END" 处停止，
    到了 responses 上游就变成不停止，输出可能无限长。
    实测 chat→responses 出站 payload 里确实没有 `stop`：
    `{"input":[...],"model":"m"}`（对比 chat→chat 有 `"stop":["END"]`）。
- 建议：把「硬约束」抽成一张**声明式清单**（约束 → 各协议的表达能力 →
  降级/拒绝策略），由网关层统一判定，而不是每个协议各写一个
  `RequiresStructuredOutput`。

### P2-9 namespace 展平与降级表两条改动未闭环（784b517 × 1360e11）

- 位置：`internal/inwire/openai_responses.go:378-402`（`flattenNamespaceTools`）、
  `internal/inwire/openai_responses.go:534-556`（`responsesDowngradedTools`）
- 现象：`flattenNamespaceTools` 对 namespace 内的项**一律**用
  `responsesToolDefinition(name, inner.Description, inner.Parameters)` 构造，
  **不走** `responsesPlainTool` 的降级逻辑。于是 namespace 里若含
  `custom` 或 `local_shell` 类型（`validateResponsesTool` 明确放行这两种，
  `openai_responses.go:238-270`），展平后会：
  - `custom` → 参数 schema 变成 `{}`（因为 custom 没有 parameters），
    模型无从知道要传 `{"input":"..."}`；且**不进 `downgradedTools` 表**，
    出站不会还原成 `custom_tool_call`，Codex 拿到的仍是普通 function_call。
  - `local_shell` → 工具名变成 `ns__`（`inner.Name` 为空，因为
    `{"type":"local_shell"}` 没有 name 字段），更糟。
- 实测：

      tools=[{namespace "ns" tools=[{custom "apply_patch"}]}]
      展平: ns__apply_patch params={}          ← schema 丢了（应为 {"input":"..."}）
      downgraded=map[]                          ← 未登记，出站不会还原成 custom_tool_call

      tools=[{namespace "ns" tools=[{local_shell}]}]
      展平: ns__ params={}                      ← 名字成了 "ns__"

- 根因：784b517 引入展平时只考虑了 namespace 内是 function 的情形；
  1360e11 引入的降级表 `responsesDowngradedTools`（`openai_responses.go:534`）
  **只遍历顶层 tools，不递归进 namespace**：

      for _, t := range in {
          if t.Type == "custom" || t.Type == "local_shell" {
              ...
          }
      }

  两条改动**没有闭环**：展平不认降级、降级不认展平。
  这正是任务书第 (5) 项要求核对的「实现是否闭环」，答案是**未闭环**。
- 建议：`flattenNamespaceTools` 内的项也走 `responsesPlainTool`；
  `responsesDowngradedTools` 递归进 namespace 并用**展平后的名字**做键
  （与 `flattenNamespaceTools` 共用同一份命名规则，避免两处再写一遍）。

### P2-10 Responses 入站的 `include` 开关被丢弃，多轮 sealed reasoning 会话可能中断

- 位置：`internal/inwire/openai_responses.go:87`（注释明说
  「metadata / service_tier / safety_identifier 等未建模字段由 encoding/json 忽略」）
- 实测：

      入站: {"model":"m","input":"hi","include":["reasoning.encrypted_content"]}
      出站 payload: {"input":[...],"model":"m","store":true,"tool_choice":"auto"}   ← include 消失

- 危害：Responses 协议里 `include:["reasoning.encrypted_content"]` 是
  客户端**主动索取**可回放的密封推理载荷的开关。不透传 ⇒ 上游可能不返回
  `encrypted_content` ⇒ 客户端下一轮回放 reasoning item 时
  拿不到载荷 ⇒ 多轮推理会话断裂。
  这与 DESIGN §7.1 强调的「sealed reasoning 跨协议边界」是同一主题，
  但方向相反：§7.1 解决的是「载荷别弄丢」，本条是「请求里索取载荷的开关
  别弄丢」。
- 建议：在 `ResponsesRequest` 声明 `Include []string`，
  并在 `ApplyUpstreamExtras` 的 `openai-responses` 分支透传
  （`include` 不在 rosetta 的 `openaiResponsesReservedPayloadKeys` 里，
  走 Extra 合法）。

---

## P3

### P3-1 `decode.go` 的重复键检查是大小写敏感的，与 JSON 键语义不完全对齐

- 位置：`internal/inwire/decode.go:44-90`（`rejectDuplicateTopLevelKeys`）
- 现象：检查按**字节精确**的键名去重，因此 `{"model":"a","MODEL":"b"}`
  不被判为重复。而 `encoding/json` 的匹配是**大小写不敏感**的 ——
  后者胜出，于是 `MODEL` 的值会覆盖 `model`。
- 实测：

      {"model":"cheap","MODEL":"expensive",...}  -> 解析出的 model = "expensive"
      {"model":"m",...,"max_tokens":100,"MAX_TOKENS":100000} -> MaxOutputTokens = 100000
      {"model":"m",...,"max_tokens":100,"max_tokens":100000} -> 正确报重复键

- 危害：同一条 JSON 请求走私攻击用精确重复被挡住、用大小写变体就绕过。
  `decode.go:18-25` 的注释说「网关按一个模型记账/鉴权，上游按另一个模型推理」
  —— 这个隐患仍在，只是换了个大小写。
- 建议：把键统一转小写（`strings.ToLower`）后再判重。
  注意：这样也会把 `MAX_TOKENS` 与 `max_tokens` 判为重复，而它们在
  OpenAI 协议里**是不同字段**（前者是 chat 的、后者是 responses 的别名，
  见 `openai_chat.go:16-17`）—— 所以更精确的做法是
  「先按小写归一，再检查归一后是否与某个已知字段名冲突」，
  而非简单地全小写去重。**待确认**：这一点需结合 DESIGN 的字段表定夺。

### P3-2 `decode.go` 对「非对象顶层」让 `json.Unmarshal` 的 Go 内部措辞回给客户端

- 位置：`internal/inwire/decode.go:54-58`、`internal/inwire/decode.go:86-89`
- 现象：`rejectDuplicateTopLevelKeys` 在 `dec.Token()` 出错、键不是 string、
  或值解码失败时都 `return nil`（交给随后的 `json.Unmarshal` 精确报错）——
  这个设计是对的（不抢答、错误信息更准）。但注释里说「顶层不是对象：也交给
  Unmarshal 报『cannot unmarshal ...』」，实测顶层是数组时 `json.Unmarshal`
  报的是 `json: cannot unmarshal array into Go value of type inwire.OpenAIChatRequest`，
  **类型名是 Go 内部结构体名**，会原样回给客户端
  （`cmd/gateway/main.go:1776` 的
  `codec.WriteError(w, 400, "invalid_request_error", err.Error())`）。
- 这与 `openai_chat.go:27-28` 注释里批评过的「错误消息里是 Go 的结构体字段名，
  对客户端毫无意义」是同一类问题，只是发生在 body 顶层而非字段。
- 建议：`decodeTopLevelObject` 在顶层非对象时给一条人类可读的
  「request body must be a JSON object」。

### P3-3 `openai_chat.go` 的 `parseEffort` 注释与实现存在偏差

- 位置：`internal/inwire/openai_chat.go:621-646`
- 现象：这是 P1-1 的具体表现（两处各写一张别名表）。此处单列是为了指出
  `parseEffort` 的注释（`openai_chat.go:613-620`）声称
  「入口不止 OpenAI Chat 的 reasoning_effort：Responses 的 reasoning.effort
  还会有 minimal 与 xhigh」—— 但它**只**处理 minimal/xhigh，
  且**遗漏 max**（P0-1）。注释与实现的偏差本身就是缺陷信号。

### P3-4 `internal/effort` 的 `Clamp` 与 `ToRosettaLevel` 的空集语义不同（已文档化的取舍）

- 位置：`internal/effort/effort.go:201-235`（`Clamp`）、
  `internal/effort/effort.go:263-283`（`ToRosettaLevel`）
- 现象：`Clamp(wanted, supported)` 在 `supported` 为空时**原样返回 wanted**
  （「未配置 = 网关不干预」）；`ToRosettaLevel(l)` 内部调用
  `Clamp(l, KnownLevels)`，`KnownLevels` 非空所以不受影响 ——
  这条路径没问题。真正的不一致在**语义层**：`Clamp` 的「空集 = 不干预」
  意味着 `effort.Clamp(Max, nil) == Max`，而 `ToRosettaLevel(Max) == "high"`。
  两级折叠的语义不同（前者保真、后者有损），注释里都有交代，属于**已文档化的
  设计取舍**，此处仅记录以免后来者误以为是 bug。
- 无需修改。

---

## 与出站层的契约核对（任务书第 6 项）

### inwire 产出但出站不消费（= 线上丢字段）

| inwire 字段 | 位置 | 出站处置 | 结论 |
|---|---|---|---|
| `ResponsesRequest.ToolChoice`（namespace 形态） | `openai_responses.go:74` | `rewriteToolChoice` 不处理，原样透传 | **P1-6 缺陷** |
| `ResponsesRequest.downgradedTools`（namespace 内层） | `openai_responses.go:534` | 表不覆盖 namespace 内的降级工具 | **P2-9 缺陷** |
| `AnthropicMessagesRequest.TopK` → responses | `anthropic.go:283-297` | 丢弃（Responses 无此旋钮） | 合理（软偏好） |
| `OpenAIChatRequest.PresencePenalty`/`FrequencyPenalty`/`Seed`/`User` → responses | `openai_chat.go:288-296` | 丢弃 | 合理（软偏好） |
| `OpenAIChatRequest.StreamOptions` → 上游 | `openai_chat.go:26`、`cmd/gateway/main.go:2583` | 不进 Extra；上游 usage 由 SDK 自行申请 | 合理 |
| `ResponsesRequest.Include`（`include:["reasoning.encrypted_content"]`） | 未声明 | 整个字段被 `encoding/json` 忽略 | **P2-10 缺陷** |
| `ResponsesRequest.MaxToolCalls` / `Truncation` / `ServiceTier` / `Metadata` | 未声明 | 静默忽略 | P2-6 同类 |
| `OpenAIChatRequest.Logprobs` / `ServiceTier` / `Metadata` / `Store` | 未声明 | 静默忽略 | P2-6 同类 |

### 出站消费但 inwire 从不产出

| 出站消费的字段 | 出站位置 | 结论 |
|---|---|---|
| `rosetta.Block.Sealed` / `SealedBy` / `ReasoningID` | `provider_openai_responses.go:368-385` | **有产出**（`openai_responses.go:790-806` 的 reasoning item 分支），闭环 |
| `rosetta.Block.Signature`（Anthropic 签名） | `provider_anthropic.go:408-420` | **有产出**（`anthropic.go:438-441`），闭环 |
| `rosetta.Block.CacheControl`（tool 级） | `provider_anthropic.go` | **有产出**（`anthropic.go:225-228`），闭环 |
| `rosetta.Block.AudioData` / `AudioFormat` | `provider_openai_chat.go:383` | **无产出**（见 P2-2），出站有能力但入站不喂 |
| `rosetta.Block.FileID` | `provider_openai_responses.go:419` | **产出但与 FileData 冲突**（P1-5） |
| `rosetta.ThinkingConfig.EffortRaw` | `cmd/gateway/main.go:1511` | 由 main 写，非 inwire；inwire 只给 `RawEffort` 字符串（见 P1-2） |

---

## 「三套协议字段翻译对称性」总表（任务书第 1 项的正面结论）

| 统一模型字段 | OpenAI Chat 入站 | Responses 入站 | Anthropic 入站 | 对称性 |
|---|---|---|---|---|
| `Model` | 有 | 有 | 有 | 对称 |
| `Messages` | 有 | 有 | 有 | 对称（但 system 位置有重复，见 P2-7） |
| `System` | 无（走 RoleSystem 消息） | 有（`instructions`） | 有/RoleSystem | **不对称，P2-7** |
| `MaxOutputTokens` | 有（max_completion_tokens 优先） | 有 | 有（必填） | 对称 |
| `Temperature` / `TopP` | 有 | 有 | 有 | 对称 |
| `StopSequences` | 有 | 无（协议无此概念，SDK 丢） | 有 | **不对称，P2-8** |
| `Tools` | 有（只认 function） | 有（+namespace/custom/local_shell） | 有 | **不对称，P2-9** |
| `ToolChoice` | 有（any 形态） | 有（+namespace） | 有 | **语义漂移，P2-1** |
| `ParallelToolCalls` | 有 | 有 | 无（协议无） | 合理 |
| `Thinking`（effort） | 有 | 有 | 有（预算→档位） | **口径分裂，P1-1/P1-3** |
| `Extra`（协议私有） | 部分 | 部分 | 部分 | **覆盖不全，P2-6/P2-10** |
| 媒体块 image | 有 | 有 | 有 | 对称 |
| 媒体块 file | 无 **P2-2** | 有 | 部分 **P2-4** | **不对称** |
| 媒体块 audio | 无 **P2-2** | 无(远端URL崩) **P2-3** | 无（协议不支持） | **不对称** |
| `Sealed` reasoning | 无（协议无） | 有 | 有（签名） | 对称（按 §7.1 门闩） |

---

## 已验证无问题的区域（含理由）

1. **`internal/inwire/decode.go` 的重复键拒绝（顶层、精确匹配）** —
   逻辑正确：`dec.More()` 循环 + `json.RawMessage` 跳过值，不会误判嵌套对象
   内部的同名键（设计如此，注释已说明取舍）。对**精确重复**的键确实报
   `ErrDuplicateJSONKey` 并被 `cmd/gateway/main.go:1768-1775` 映射成 400。
   唯一缺陷是大小写敏感（P3-1），不影响该机制本身的正确性。

2. **请求体大小限制** — 三个入口都是同一套纪律：
   `io.ReadAll(io.LimitReader(r.Body, maxBytes+1))` + 长度判定 +
   `&http.MaxBytesError{}`（`openai_chat.go:158-166`、
   `openai_responses.go:286-294`、`anthropic.go:119-127`）。
   读 `maxBytes+1` 再显式判长度，避免了 LimitReader 静默截断导致
   「报 400 而不是 413」的经典错误。`cmd/gateway/main.go:1768-1775`
   用 `errors.As` 正确映射到 413 + `request_too_large`。
   `internal/config/config.go:407` 还给 `max_request_body_bytes`
   加了 512 MiB 上界，防误配 OOM。**无问题。**

3. **重复 JSON 键的拒绝不会误伤正常请求** — 顶层键去重不递归，
   而 `messages` / `input` 数组内部的同名键（如两条 message 都有 `role`）
   不受影响。实测正常请求全部通过。**无问题。**

4. **流式标识判定** — `Stream bool` 三个入口都用 `json:"stream,omitempty"`
   声明（`openai_chat.go:27`、`openai_responses.go:41`、`anthropic.go:40`），
   正确处理了 `"stream": true` 与缺省。`cmd/gateway/main.go:1641/1676/1714`
   三处一致地把它送进 `ingressRequest.stream`。**无问题。**

5. **Content-Type / 编码处理** — 入站一律 `io.ReadAll` + `json.Unmarshal`，
   不依赖 Content-Type；`cmd/gateway/main.go:1776` 统一按
   `invalid_request_error` 回 400，形状由 `internal/outwire/errors.go:193`
   的 `WriteOpenAIError` / Anthropic 对应函数按入口协议输出，
   与 DESIGN §9 的错误形状一致。**无问题**（除 P3-2 的错误文案问题）。

6. **`n > 1` 的显式拒绝** — `openai_chat.go:191-208` 逻辑正确：
   `n>1` 拒绝、`n==1` 放行、`n==0` 放行、`n<0` 拒绝，且错误消息说明了原因
   与改法。**无问题**（但见 P2-6：其它 Chat 私有字段没有同等处置）。

7. **`RequiresStructuredOutput` 的两个实现** —
   `openai_chat.go:462-476` 与 `openai_responses.go:1314-1322` 逻辑等价、
   都只认 `json_object`/`json_schema`、畸形 JSON 返回 false。
   `responseFormatToTextFormat`（`openai_chat.go:494-520`）与
   `textFormatToResponseFormat`（`openai_responses.go:1260-1300`）
   是**互为镜像**的一对，json_object 直映、json_schema 摊平/嵌套互转、
   未知类型原样透传给上游拒绝。实测 chat→responses 与 responses→chat
   两条路的结构化输出都能正确落地。**无问题**（这是本仓做得最好的翻译对）。

8. **Responses `text.format` 的双层 format 修复** —
   `openai_responses.go:1148-1156` 用 `textFormatMap()` 正确拆包，
   不会产出 `{"format":{"format":{...}}}` 的双层。**无问题。**

9. **namespace 展平的前缀规则** — `openai_responses.go:378-402`：
   用 `父名__原名`、用 `__` 而非 `.`、不改写 description，
   符合 DESIGN §7.3 的全部三条要求。**前缀规则本身无问题**
   （问题在 P2-9：namespace 内的降级类型没走展平规则）。

10. **namespace 工具的重名处理** — `openai_responses.go:394-398` 的
    `seen[name]` 检查后**不覆盖**而是仍然 append，让 rosetta 的
    `validate()` 去报 `duplicate tool name`（实测：
    `rosetta: invalid request: duplicate tool name "shell__ls"`）。
    这比静默去重好（后者会让一个工具凭空消失）。**无问题**
    （错误消息是 SDK 措辞，客户端可读性一般，但语义正确）。

11. **嵌套 namespace 的拒绝** — `openai_responses.go:272-282` 明确拒绝
    嵌套 namespace 并带位置信息 `tools.tools[0]`。**无问题。**

12. **Responses 内建工具 item 类型的白名单** —
    `openai_responses.go:168-183` 的 `responsesSupportedItemTypes` 与
    `validateResponsesInputItems`（`openai_responses.go:196-218`）配合，
    对 `web_search_call` 等服务端执行型 item 报带位置的 400，
    对 Codex 族（`local_shell_call`/`custom_tool_call`）放行。
    符合 DESIGN §7.3。**无问题。**

13. **Responses reasoning item 的 sealed 搬运** —
    `openai_responses.go:779-806`：空 item 跳过（避免 validate 报
    「thinking block is empty」）、`ReasoningID` 单独设置、
    `SealedBy` 设为 `"openai-responses"`。与 SDK 的
    `provider_openai_responses.go:368-385` 的 `SealedBy` 门闩闭环。
    **无问题**（唯一缺的是请求侧的 `include` 开关，见 P2-10）。

14. **`rewriteToolChoice` 的 function 形态与歧义不猜** —
    `openai_responses.go:579-628`：唯一匹配才改名，多个 namespace 同名工具时
    保持原值不猜。**无问题**（namespace 形态的缺口见 P1-6）。

15. **Anthropic `content` 单对象形态的 400** —
    `anthropic.go:196-206` + `decodeContentBlocks`（`anthropic.go:539-565`）
    明确区分「数组 / 纯字符串 / 单对象」，单对象报
    `ErrContentSingleObject` 而不是静默丢内容。
    注释里也写明了旧实现「用户输入被静默丢弃且请求照常 200」的历史。
    **无问题。**

16. **Anthropic `tool_choice` 的 decode 收敛** —
    `anthropic.go:145-156` 校验 type ∈ {auto,any,none,tool}，
    `type=tool` 必须带 name，非法值报带原值的 400。
    **无问题**（翻译侧的问题见 P2-1）。

17. **Anthropic `thinking` 的 decode 校验** —
    `anthropic.go:157-175`：`type=enabled` 要求 `budget_tokens>0`、
    `type=disabled` 放行、其它 type 报 400。**无问题**
    （预算折算的问题见 P1-3）。

18. **`internal/effort` 包自身的正确性** — `Clamp`（`effort.go:201-235`）
    的「同距取更强」「None 永不作候选」「空集=不干预」三条语义都与注释一致，
    且 `effort_test.go` 逐条钉住。`ToRosettaLevel`（`effort.go:263-283`）
    用 `default` 兜底并有逐档断言测试（`effort_test.go:199-215`），
    不会静默降一档。`ParseLevels`/`FormatLevels` 的去重/排序/未知值回传
    逻辑正确。**effort 包自身无问题**；问题在于**没人调用它**
    （P1-1、P1-2）。

19. **`internal/effort` 的 `Max` 档在存储与披露链路的保真** —
    `effort.go:66`（`Max` 独立档）、`effort.go:126-133`（别名不折叠成 XHigh）、
    `internal/upstream/upstream.go:903-912`（从上游读 `max`）、
    `internal/store/model_dao.go:72/118`（FormatLevels/ParseLevels 往返保真）、
    `cmd/gateway/main.go:3660-3667`（`supported_reasoning` 披露 `max`）——
    这条链**完整且正确**。它是 P0-1 的对照面：**披露层认 max、存储层认 max、
    只有入站解析层不认 max。**

20. **Anthropic 的 `cache_control` 逐块透传（user 侧 text/image/document）** —
    `anthropic.go:473/479/486` 三处都带了 `b.cacheControl()`，
    与 rosetta 的 `provider_anthropic.go:355-360` 的 `putCC` 闭环。
    **无问题**（仅 tool_result 分支漏了，见 P2-5）。

21. **Anthropic 工具级 `cache_control`** — `anthropic.go:225-228`
    把工具的断点挂到 `rosetta.ToolDefinition.CacheControl`，
    rosetta 侧 `provider_anthropic.go` 有对应处理。**无问题。**

22. **`decode.go` 的错误不抢答原则** — 顶层非对象、键非字符串、值解码失败时
    都 `return nil` 交给 `json.Unmarshal` 精确报错，避免了两套错误口径。
    **无问题**（文案问题见 P3-2）。

23. **三个入口的 `ToRosetta` 都是逐 attempt 工厂、无跨 attempt 状态污染** —
    `openai_responses.go:62-73` 的 `flattenedToolChoice` 注释详细说明了
    为什么可以把重写结果挂在请求结构体上（ToRosetta 与 ApplyUpstreamExtras
    在同一个 attempt 内先后执行），`cmd/gateway/main.go` 的
    `ing.buildRosetta()` 每次 attempt 重新调用。实测
    `effectiveToolChoice` 的返回值与本次 attempt 的展平结果一致。
    **无问题。**

24. **Responses 入站 `function_call_output` 的媒体块处理** —
    `openai_responses.go:930-968`（`responsesItemToolOutput`）正确区分
    字符串 / 结构对象 / 分块数组三种 output 形态，
    并把媒体搬进紧随其后的 user 消息 + 留一行说明
    （`responsesToolMediaNotice`）。实测 `local_shell_call_output`
    的 `{"stdout":...}` 结构对象也能正确展平。
    **无问题**（这是 §7.2 的正确实现，Anthropic 入口缺同样的处理，见 P1-4）。

25. **Chat 的 `refusal` 兜底与 Responses 的 `output_text` 方向隔离** —
    `openai_chat.go:509-527`（`assistantText`）在 content 为空时回落
    `refusal`，避免「模型正当拒答后续聊必然 400」的死局；
    `openai_responses.go:792-810`（`convertResponsesMessage`）
    区分 assistant 读 `output_text`、user 只读 `input_text`、
    未知角色只采信 `input_text`，避免把模型上一句回答当用户新指令。
    两者都是经过实测确认的正确行为。**无问题。**

---

## 附：验证手段说明

所有「实测」结论均来自 `work/a2probe/` 下的只读探针程序（该目录被 gitignore，
不在仓库跟踪范围内，审计结束后可删；本次未删除，仅在此说明）。探针走的是真实的
`inwire.Decode*Request` → `ToRosetta` → `ApplyProtocolPrivateExtra` /
`ApplyUpstreamExtras` → `rosetta.Client.Chat` 链路，并抓取假上游收到的
实际 wire payload，因此结论反映的是**线上真实行为**，而非对代码的静态推断。

未做验证、标注「待确认」的项：P3-1 中「大小写归一后与
`max_tokens`/`max_completion_tokens` 这类别名字段如何共存」需要结合
DESIGN 的字段表与出站行为定夺，本次未展开。



