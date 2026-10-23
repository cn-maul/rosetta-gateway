package dataplane

import (
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/routing"
)

// 上游展示名里带 "/"、";" 或反斜杠时，轨迹仍必须是可还原的一条记录。
// 前端解析器使用同一套转义规则；这里钉住写端，避免改名后历史轨迹错位。
func TestAppendRouteTrace_EscapesProviderAndModelSeparators(t *testing.T) {
	cand := routing.Candidate{
		Provider:      &routing.ProviderRef{Name: `肥鱼/华东;主\线`, Slug: "ignored"},
		UpstreamModel: &routing.UpstreamModel{ModelID: `org/model;v2\preview`},
	}
	got := AppendRouteTrace(nil, 2, cand, "fail_switch", "upstream_error")
	if len(got) != 1 {
		t.Fatalf("trace len=%d", len(got))
	}
	want := `2:肥鱼\/华东\;主\\线/org/model\;v2\\preview -> fail_switch:upstream_error`
	if got[0] != want {
		t.Fatalf("trace=%q\nwant  %q", got[0], want)
	}
}

// 旧快照没有 Name 时必须回退到 Slug，不能写出空的上游名。
func TestAppendRouteTrace_FallsBackToSlug(t *testing.T) {
	cand := routing.Candidate{
		Provider:      &routing.ProviderRef{Slug: "backup"},
		UpstreamModel: &routing.UpstreamModel{ModelID: "gpt-4o"},
	}
	got := AppendRouteTrace(nil, 1, cand, "ok", "")
	if len(got) != 1 || got[0] != "1:backup/gpt-4o -> ok" {
		t.Fatalf("trace=%v", got)
	}
}
