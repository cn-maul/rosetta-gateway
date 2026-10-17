package inwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrDuplicateJSONKey 表示请求体顶层对象里出现了重复键。
//
// 为什么要拒绝而不是「后者覆盖前者」：
//
//   - 这是 JSON 请求走私的经典载体。{"model":"cheap","model":"expensive"} 在
//     不同解析器里可能得到不同答案 —— 网关按一个模型记账/鉴权，上游按另一个
//     模型推理。两边都「成功」，差额没人发现。
//   - 三个下游协议（OpenAI Chat / Responses / Anthropic Messages）都以顶层对象
//     承载 model 字段，所以这是三个入口的共同风险，不是某一个协议的特例。
//
// 只检查顶层键，不递归检查嵌套对象：顶层才承载 model / stream / max_tokens 这类
// 决定路由与计费的字段；递归扫描会给每个请求多一次完整解码，热路径上不划算。
var ErrDuplicateJSONKey = errors.New("request body contains duplicate top-level keys")

// decodeTopLevelObject 把请求体解码成结构体，并在此之前拒绝重复顶层键。
//
// 单遍扫描：json.Decoder 顺手把每个键解成 string，再用「见过就报错」的
// set 判定。随后用原始字节做一次 Unmarshal，避免二次读取 r.Body。
func decodeTopLevelObject(body []byte, dst any) error {
	if err := rejectDuplicateTopLevelKeys(body); err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

func rejectDuplicateTopLevelKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))

	tok, err := dec.Token()
	if err != nil {
		// 交给随后的 Unmarshal 精确报错（空 body、非对象等）。
		return nil
	}
	if tok != json.Delim('{') {
		// 顶层不是对象：也交给 Unmarshal 报「cannot unmarshal ...」，
		// 由调用方的 400 分支处理，不在这里抢答。
		return nil
	}

	seen := make(map[string]struct{})
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateJSONKey, key)
		}
		seen[key] = struct{}{}

		// 跳过值。RawMessage 让 decoder 自行处理嵌套对象/数组/标量，
		// 不在这里重新实现 JSON 语法。
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil
		}
	}

	// 读掉收尾的 '}'。读不到说明 body 被截断，同样交给 Unmarshal 报错。
	if _, err := dec.Token(); err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	return nil
}
