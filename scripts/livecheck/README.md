# livecheck —— 用真上游验证思考开关

单元测试只能证明网关**写出了**某个字段，证明不了上游**认这个字段**。
这个工具拿一条真实的网关 key 打真实上游，看「关」是不是真的关掉了。

## 跑之前需要

1. 网关在跑（默认 `127.0.0.1:8666`）。
2. 一把网关 API key（`sk-gw-...`），不是上游厂商的 key。
3. 这把 key 能访问的目标模型名（网关对外名，不是上游内部名）。

## 用法

```powershell
go run ./scripts/livecheck -base http://127.0.0.1:8666 -key sk-gw-... -model deepseek-chat
```

常用参数：

| 参数 | 说明 |
|---|---|
| `-protocol chat\|anthropic\|response` | 走哪个入口；要和该模型实际支持的协议一致 |
| `-stream` | 额外跑一次流式（并要求真的收到 SSE） |
| `-repeat 3` | 每个用例重复 3 次，看稳定性 |
| `-timeout 180s` | 单次请求超时 |

## 它会发什么

固定 5～6 个用例，共用同一个提示词（一道需要计算的应用题，能稳定触发思考）：

1. 什么都不改（对照）
2. `@thinking:off`
3. `@effort:low`
4. `@effort:high`
5. `@dialect:qwen@thinking:off`
6. （`-stream` 时）`@thinking:off` + `stream:true`

## 怎么判「通过」

不看 HTTP 200。上游完全可能收下一个它不认识的字段照样 200。判据是响应里有没有思考痕迹：

- Chat：`choices[].message.reasoning_content`
- Anthropic：`content[].type == "thinking"`
- Responses：`output[].type == "reasoning"`

要关的用例出现思考痕迹 → 不通过；要开的用例完全没有 → 也不通过。

流式用例还会额外检查响应体是不是真的 SSE —— 网关或上游退回整包 JSON 时，
「没看到思考」只是因为它走了一条根本没被测试的路径。

## 读结果

```
用例                           状态     思考?    思考tok      耗时        备注
baseline（不改任何参…              200    有      18         4ms       210
后缀 @thinking:off             200    无      0          1ms       210
```

`状态 200 + 思考? 无` 才算「关掉了」。

## 已知限制

- 只看「有没有思考痕迹」，不核对计费金额。
- 有些上游既不返回思考内容也不报 reasoning_tokens，此时工具只能给出
  「没有痕迹」，无法区分「真关了」和「有思考但不回传」。这类模型建议用
  `-repeat` 多跑几次并配合网关用量页对比。
