// Package protocol 是网关内部协议名的唯一口径。
//
// 三种上游协议在四处被"翻译"：管理接口的入参校验、配置存库、
// SDK 客户端的构造、以及诊断日志里的记录。这四处曾经各写一份
// switch，两份逐字相同（一份在上游连接池、一份在诊断），一份是它的
// 反向映射。加第四种协议时漏改任何一处都不会编译失败 —— 症状是
// 「配置是对的，但日志/探测把它写成另一个名字」。
//
// 所以映射只留在这里：名字 ↔ SDK 常量各一个函数，别处一律调用。
package protocol

import (
	"strings"

	"github.com/cn-maul/rosetta"
)

const (
	Auto      = "auto"
	Chat      = "chat"
	Response  = "response"
	Anthropic = "anthropic"
)

const (
	LegacyChat     = "openai-chat"
	LegacyResponse = "openai-responses"
)

const AllowedValues = "auto / chat / response / anthropic"

// Normalize 把历史别名折回当前取值，并去掉首尾空白。
func Normalize(value string) string {
	switch strings.TrimSpace(value) {
	case LegacyChat:
		return Chat
	case LegacyResponse:
		return Response
	default:
		return strings.TrimSpace(value)
	}
}

// Valid 判断是否是白名单内的取值（含 auto）。
func Valid(value string) bool {
	switch value {
	case Auto, Chat, Response, Anthropic:
		return true
	default:
		return false
	}
}

// ToSDK 把网关内部协议名翻成 SDK 常量。
//
// 第二个返回值表示是否认得这个名字：不认识时调用方必须自己决定兜底
// 还是报错 —— 这里不给默认值，否则「拼错的配置」会被静默当成 chat。
func ToSDK(name string) (rosetta.Protocol, bool) {
	switch Normalize(name) {
	case Chat:
		return rosetta.ProtoOpenAIChat, true
	case Response:
		return rosetta.ProtoOpenAIResponses, true
	case Anthropic:
		return rosetta.ProtoAnthropic, true
	default:
		return "", false
	}
}

// FromSDK 是 ToSDK 的反向映射，用于把 SDK 探测出的协议写回库、写进日志。
//
// 未知协议原样返回 SDK 的字符串值（而不是猜一个）：那种情况下
// 「日志里出现一个陌生名字」正是需要看到的信息。
func FromSDK(p rosetta.Protocol) string {
	switch p {
	case rosetta.ProtoOpenAIChat:
		return Chat
	case rosetta.ProtoOpenAIResponses:
		return Response
	case rosetta.ProtoAnthropic:
		return Anthropic
	default:
		return string(p)
	}
}
