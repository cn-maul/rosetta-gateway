package inwire

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
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
// 顶层扫描使用 jsontext.SkipValue：它遍历并校验嵌套值语法，但不会像
// json.RawMessage 那样把整段值复制到新的缓冲区。随后仍用 Unmarshal 填充
// 业务结构体，但重复键检查本身不再额外分配约 body 大小的临时数据。
func decodeTopLevelObject(body []byte, dst any) error {
	if err := rejectDuplicateTopLevelKeys(body); err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

func rejectDuplicateTopLevelKeys(body []byte) error {
	// AllowDuplicateNames(true) 关闭 jsontext 自己的“全对象递归去重”，
	// 因为本函数只对决定路由/计费的顶层字段负责；嵌套 schema 中出现重复名
	// 由最终 Unmarshal 的语义处理。顶层集合这里仍显式检查。
	dec := jsontext.NewDecoder(bytes.NewReader(body), jsontext.AllowDuplicateNames(true))

	tok, err := dec.ReadToken()
	if err != nil {
		// 交给随后的 Unmarshal 精确报错（空 body、非对象等）。
		return nil
	}
	if tok.Kind() != '{' {
		// 顶层不是对象：也交给 Unmarshal 报「cannot unmarshal ...」，
		// 由调用方的 400 分支处理，不在这里抢答。
		return nil
	}

	seen := make(map[string]struct{})
	for dec.PeekKind() != '}' {
		keyTok, err := dec.ReadToken()
		if err != nil {
			return nil
		}
		if keyTok.Kind() != '"' {
			return nil
		}
		key := keyTok.String()
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateJSONKey, key)
		}
		seen[key] = struct{}{}

		if err := dec.SkipValue(); err != nil {
			return nil
		}
	}

	// 读掉收尾的 '}'。读不到说明 body 被截断，同样交给 Unmarshal 报错。
	if _, err := dec.ReadToken(); err != nil {
		return nil
	}
	return nil
}
