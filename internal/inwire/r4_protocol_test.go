package inwire

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
)

func TestR4_ChatInputAudioAndFileArePreserved(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"m",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"inspect these"},
			{"type":"input_audio","input_audio":{"data":"data:audio/wav;base64,UklGRg==","format":"wav"}},
			{"type":"file","file":{"filename":"report.pdf","file_data":"data:application/pdf;base64,JVBERg=="}}
		]}]
	}`))
	got, err := DecodeOpenAIChatRequest(req, 1<<20)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	blocks := got.ToRosetta().Messages[0].Blocks
	if len(blocks) != 3 {
		t.Fatalf("blocks = %+v, want text + audio + file", blocks)
	}
	if blocks[0].Type != rosetta.BlockText || blocks[0].Text != "inspect these" {
		t.Fatalf("text block lost: %+v", blocks[0])
	}
	if blocks[1].Type != rosetta.BlockAudio || blocks[1].AudioData != "UklGRg==" || blocks[1].AudioFormat != "wav" {
		t.Fatalf("audio block lost: %+v", blocks[1])
	}
	if blocks[2].Type != rosetta.BlockFile || blocks[2].FileName != "report.pdf" || blocks[2].FileData != "JVBERg==" {
		t.Fatalf("file block lost: %+v", blocks[2])
	}
}

func TestR4_ChatFileIDUsesOnlyFileID(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"m",
		"messages":[{"role":"user","content":[
			{"type":"file","file":{"filename":"remote.pdf","file_id":"file-123"}}
		]}]
	}`))
	got, err := DecodeOpenAIChatRequest(req, 1<<20)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	block := got.ToRosetta().Messages[0].Blocks[0]
	if block.FileID != "file-123" || block.FileData != "" {
		t.Fatalf("file source must be exclusive: %+v", block)
	}
}

func TestR4_AnthropicTextDocumentIsPreserved(t *testing.T) {
	got := decodeAnthropic(t, `{
		"model":"claude",
		"max_tokens":16,
		"messages":[{"role":"user","content":[
			{"type":"text","text":"summarize this"},
			{"type":"document","source":{"type":"text","media_type":"text/plain","data":"document body"}}
		]}]
	}`)

	blocks := got.ToRosetta().Messages[0].Blocks
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v, want text + document text", blocks)
	}
	if blocks[1].Type != rosetta.BlockText || blocks[1].Text != "document body" {
		t.Fatalf("text document lost: %+v", blocks[1])
	}
}

func TestR4_UnsupportedResponsesToolErrorListsEveryAcceptedType(t *testing.T) {
	err := validateResponsesTool(ResponsesTool{Type: "web_search"}, "tools[0]")
	if err == nil {
		t.Fatal("web_search must be rejected")
	}
	for _, accepted := range []string{"function", "custom", "local_shell", "namespace"} {
		if !strings.Contains(err.Error(), accepted) {
			t.Fatalf("error %q does not name accepted type %q", err, accepted)
		}
	}
}
