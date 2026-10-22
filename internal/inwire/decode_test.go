package inwire

import (
	"encoding/json"
	"errors"
	"testing"
)

// 顶层重复键是「JSON 走私」的入口：扫描器与 Unmarshal 可能各取一个值
// （Go 的 Unmarshal 取最后一个），于是「网关据以路由/计费的 model」
// 与「上游看到的 model」可以是同一个请求体里的两个不同名字。
// 这条检查在 openai_chat / anthropic / openai_responses 三个入口共用。
func TestDecodeTopLevelObject_RejectsDuplicateTopLevelKey(t *testing.T) {
	var dst struct {
		Model string `json:"model"`
	}
	err := decodeTopLevelObject([]byte(`{"model":"a","model":"b"}`), &dst)
	if !errors.Is(err, ErrDuplicateJSONKey) {
		t.Fatalf("want ErrDuplicateJSONKey, got %v", err)
	}
}

// 转义过的键名必须按**解码后**的字面量比较：`"mo\u0064el"` 与 `"model"`
// 是同一个键。只做字节级比较会把这条走私路径漏掉。
func TestDecodeTopLevelObject_RejectsEscapedDuplicateKey(t *testing.T) {
	var dst struct {
		Model string `json:"model"`
	}
	err := decodeTopLevelObject([]byte(`{"mo\u0064el":"a","model":"b"}`), &dst)
	if !errors.Is(err, ErrDuplicateJSONKey) {
		t.Fatalf("want ErrDuplicateJSONKey for escaped duplicate, got %v", err)
	}
}

// 检查只管顶层：嵌套对象里的重复名交给 Unmarshal 的语义，不在这里抢答
// （抢答会让「上游 schema 里恰好有重复名」的正常请求变成 400）。
func TestDecodeTopLevelObject_AllowsNestedDuplicateKey(t *testing.T) {
	var dst map[string]json.RawMessage
	if err := decodeTopLevelObject([]byte(`{"a":{"x":1,"x":2}}`), &dst); err != nil {
		t.Fatalf("nested duplicate must not be rejected here, got %v", err)
	}
}

// 语法错误、截断、非对象顶层都**不**在这里报错：它们有各自的 400 文案，
// 由随后的 Unmarshal 精确指出（本函数只负责重复键这一件事）。
func TestDecodeTopLevelObject_DefersOtherSyntaxErrors(t *testing.T) {
	for _, body := range []string{
		``,
		`[1,2]`,
		`{"model":`,
		`{"model":"a"`,
		`{"model":"a",`,
		`{"model":1,"other":{"nested":`,
	} {
		var dst struct {
			Model string `json:"model"`
		}
		err := decodeTopLevelObject([]byte(body), &dst)
		if errors.Is(err, ErrDuplicateJSONKey) {
			t.Fatalf("body %q must not be reported as a duplicate key", body)
		}
	}
}

// 正常请求照常解码，重复键检查不能把合法 body 判死。
func TestDecodeTopLevelObject_DecodesValidBody(t *testing.T) {
	var dst struct {
		Model string          `json:"model"`
		Tools json.RawMessage `json:"tools"`
	}
	body := []byte(`{"model":"flash","tools":[{"a":[1,2,{"b":null}]}]}`)
	if err := decodeTopLevelObject(body, &dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dst.Model != "flash" {
		t.Fatalf("model = %q, want flash", dst.Model)
	}
}
