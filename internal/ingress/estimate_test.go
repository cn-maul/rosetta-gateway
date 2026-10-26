package ingress

import (
	"testing"

	"github.com/cn-maul/rosetta"
)

func TestR4_InputEstimateCountsMedia(t *testing.T) {
	textOnly := &rosetta.ChatRequest{Messages: []rosetta.Message{rosetta.User("hello")}}
	withMedia := &rosetta.ChatRequest{Messages: []rosetta.Message{{
		Role: rosetta.RoleUser,
		Blocks: []rosetta.Block{
			{Type: rosetta.BlockText, Text: "hello"},
			{Type: rosetta.BlockImage, ImageURL: "https://example.test/image.png"},
			{Type: rosetta.BlockAudio, AudioData: "UklGRg==", AudioFormat: "wav"},
			{Type: rosetta.BlockFile, FileName: "report.pdf", MimeType: "application/pdf", FileData: "JVBERg=="},
		},
	}}}

	base := EstimateInputTokensOnly(textOnly)
	got := EstimateInputTokensOnly(withMedia)
	if got < base+1500+500+3000 {
		t.Fatalf("media estimate = %d, text-only = %d; media was ignored", got, base)
	}
}

func TestR4_InlineFileEstimateScalesWithPayload(t *testing.T) {
	small := &rosetta.ChatRequest{Messages: []rosetta.Message{{
		Role: rosetta.RoleUser,
		Blocks: []rosetta.Block{{
			Type: rosetta.BlockFile, FileName: "small.pdf", FileData: "JVBERg==",
		}},
	}}}
	large := &rosetta.ChatRequest{Messages: []rosetta.Message{{
		Role: rosetta.RoleUser,
		Blocks: []rosetta.Block{{
			Type: rosetta.BlockFile, FileName: "large.pdf", FileData: string(make([]byte, 64*1024)),
		}},
	}}}
	if EstimateInputTokensOnly(large) <= EstimateInputTokensOnly(small) {
		t.Fatalf("large inline file must increase the estimate")
	}
}

func TestR4_OutputCeilingUsesResolvedChainWhenClientOmitsCap(t *testing.T) {
	req := &rosetta.ChatRequest{
		Messages: []rosetta.Message{rosetta.User("hello")},
	}
	got := RequestOutputCeiling(req, []int{1024, 8192, 4096})
	if got != 8192 {
		t.Fatalf("output ceiling = %d, want chain maximum 8192", got)
	}
	if EstimateRequestTokens(req, got) < EstimateInputTokensOnly(req)+8192 {
		t.Fatalf("admission estimate did not include the chain output ceiling")
	}
}

func TestR4_ClientOutputCapWinsOverChainCeiling(t *testing.T) {
	req := &rosetta.ChatRequest{MaxOutputTokens: 128}
	if got := RequestOutputCeiling(req, []int{8192}); got != 128 {
		t.Fatalf("output ceiling = %d, want client cap 128", got)
	}
}
