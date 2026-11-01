package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// log_format 的三条约束：缺省 auto、三种合法值都收、其它一律拒。
//
// 为什么必须校验而不是静默回落：格式选错时的表现是「日志长得不对」，
// 而不是「启动失败」——运维会以为开关没生效，反复改配置、反复重启，
// 最后怀疑到别的地方去。与 log_level 同一条理由。
func TestLogFormat_缺省为auto(t *testing.T) {
	cfg := Default()
	if cfg.LogFormat != "auto" {
		t.Fatalf("Default().LogFormat = %q, want auto", cfg.LogFormat)
	}
}

func TestLogFormat_空串经setDefaults补成auto(t *testing.T) {
	cfg := Default()
	cfg.LogFormat = ""
	cfg.setDefaults()
	if cfg.LogFormat != "auto" {
		t.Fatalf("setDefaults 后 LogFormat = %q, want auto", cfg.LogFormat)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("补成 auto 后不该报错：%v", err)
	}
}

func TestLogFormat_合法值被接受(t *testing.T) {
	for _, v := range []string{"auto", "json", "text", "JSON", " Text "} {
		cfg := Default()
		cfg.LogFormat = v
		if err := cfg.validate(); err != nil {
			t.Fatalf("log_format=%q 应被接受，却报错：%v", v, err)
		}
	}
}

func TestLogFormat_非法值被拒(t *testing.T) {
	for _, v := range []string{"test", "console", "yaml", "jsonl", "1"} {
		cfg := Default()
		cfg.LogFormat = v
		err := cfg.validate()
		if err == nil {
			t.Fatalf("log_format=%q 应被拒绝，却通过了", v)
		}
		if !strings.Contains(err.Error(), "log_format") {
			t.Fatalf("错误文案应提到 log_format，实际：%v", err)
		}
	}
}

// 生成的默认配置必须带这一项，否则新装用户永远不知道有开关。
// LoadOrGenerate 写文件用的就是 json.MarshalIndent(Default())，这里等价复现。
func TestLogFormat_默认配置序列化后含该键(t *testing.T) {
	raw, err := json.MarshalIndent(Default(), "", "  ")
	if err != nil {
		t.Fatalf("marshal default: %v", err)
	}
	if !strings.Contains(string(raw), `"log_format": "auto"`) {
		t.Fatalf("默认配置里缺 log_format:\n%s", raw)
	}
}

// 显式写 json 时，生成的配置里不该被改写成 auto。
func TestLogFormat_显式值不被覆盖(t *testing.T) {
	cfg, err := Parse([]byte(`{"log_format":"text"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.LogFormat != "text" {
		t.Fatalf("Parse 后 LogFormat = %q, want text", cfg.LogFormat)
	}
}
