// Command livecheck 拿一条真实的网关 key 去打真实上游，验证
// 「思考开关到底有没有生效」。
//
// # 为什么需要它
//
// 单元测试只能证明网关**写出了**某个字段；证明不了上游**认这个字段**。
// 这两件事之间隔着整个厂商实现：GLM-5.3 收到 disabled 是 400，
// DeepSeek 收到 budget_tokens 是静默忽略，MiniMax 收到 none 也是 400。
// 这类差异只有在真上游上跑一遍才看得见。
//
// # 判据
//
// 不看 HTTP 200 就下结论 —— 上游完全可能收下一个它不认识的字段照样 200。
// 真正说明「关掉了」的证据有三条，任一条成立即算生效：
//
//  1. 响应里没有思考内容（chat 的 reasoning_content、Anthropic 的
//     thinking 块、Responses 的 reasoning item）；
//  2. usage 里的 reasoning_tokens 为 0 或缺失；
//  3. 同一个模型「开」与「关」两次调用的 reasoning_tokens 有显著差异。
//
// 第 3 条是兜底：有些上游不返回思考内容，只计费。
//
// 用法：
//
//	livecheck -base http://127.0.0.1:8666 -key sk-gw-... -model deepseek-chat
//	livecheck -base ... -key ... -model glm-5.3 -dialect glm -protocol anthropic
//
// 只读不写：除了一次正常的补全调用外不触碰任何管理接口。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

type config struct {
	base     string
	key      string
	model    string
	protocol string
	dialect  string
	stream   bool
	timeout  time.Duration
	repeat   int
}

func main() {
	var c config
	flag.StringVar(&c.base, "base", "http://127.0.0.1:8666", "网关地址")
	flag.StringVar(&c.key, "key", os.Getenv("GATEWAY_KEY"), "网关 API key（也可用 GATEWAY_KEY）")
	flag.StringVar(&c.model, "model", "", "要测的模型名（网关对外名）")
	flag.StringVar(&c.protocol, "protocol", "chat", "chat | anthropic | response")
	flag.StringVar(&c.dialect, "dialect", "", "只验证某一方言时传它，留空则跑全套")
	flag.BoolVar(&c.stream, "stream", false, "额外跑一次流式")
	flag.DurationVar(&c.timeout, "timeout", 180*time.Second, "单次请求超时")
	flag.IntVar(&c.repeat, "repeat", 1, "每个用例重复次数（看稳定性）")
	flag.Parse()

	if c.key == "" || c.model == "" {
		fatalf("-key 与 -model 必填")
	}
	c.base = strings.TrimRight(c.base, "/")

	cases := buildCases(c.model, c.stream)
	results := make([]result, 0, len(cases)*c.repeat)
	for _, tc := range cases {
		for i := 0; i < c.repeat; i++ {
			results = append(results, runOne(c, tc))
		}
	}

	report(results)
	if !allOK(results) {
		os.Exit(1)
	}
}

// testCase 是一次具体的调用：用哪个模型名（可能带后缀）、带什么请求体。
type testCase struct {
	name  string
	model string
	body  map[string]any
	// wantOff 表示这个用例期望「不思考」。为 false 时只要求请求成功
	// （不开不关的对照组，不判断思考有没有发生）。
	wantOff bool
	wantOn  bool
}

func buildCases(model string, stream bool) []testCase {
	cases := []testCase{
		{
			name:  "baseline（不改任何参数）",
			model: model,
			body:  map[string]any{},
		},
	}

	modes := []struct {
		name    string
		suffix  string
		wantOff bool
		wantOn  bool
	}{
		{"后缀 @thinking:off", "@thinking:off", true, false},
		{"后缀 @effort:low", "@effort:low", false, true},
		{"后缀 @effort:high", "@effort:high", false, true},
	}

	for _, m := range modes {
		cases = append(cases, testCase{
			name:    m.name,
			model:   model + m.suffix,
			body:    map[string]any{},
			wantOff: m.wantOff,
			wantOn:  m.wantOn,
		})
	}

	// 方言覆盖：强制用 Qwen 的写法发一次关闭。上游若只认 enable_thinking，
	// 这条能成功就说明 @dialect 真的接管了字段选择，而不是沿用自动推断。
	cases = append(cases, testCase{
		name:    "后缀 @dialect:qwen@thinking:off",
		model:   model + "@dialect:qwen@thinking:off",
		body:    map[string]any{},
		wantOff: true,
	})

	if stream {
		cases = append(cases, testCase{
			name:    "流式 @thinking:off",
			model:   model + "@thinking:off",
			body:    map[string]any{"stream": true},
			wantOff: true,
		})
	}
	return cases
}

type result struct {
	c          config
	tc         testCase
	status     int
	err        string
	reasoning  bool
	reasonToks int
	outputToks int
	duration   time.Duration
	snippet    string
}

func runOne(c config, tc testCase) result {
	start := time.Now()
	body := buildBody(c, tc)
	raw, _ := json.Marshal(body)

	url := c.base + pathFor(c.protocol)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return result{c: c, tc: tc, err: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	setAuth(req, c.protocol, c.key)

	client := &http.Client{Timeout: c.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return result{c: c, tc: tc, err: err.Error(), duration: time.Since(start)}
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	out := result{c: c, tc: tc, status: resp.StatusCode, duration: time.Since(start)}
	out.reasoning, out.reasonToks, out.outputToks = parseEvidence(c.protocol, data)
	// 流式用例必须真的收到 SSE。上游（或网关）退回整包 JSON 时，
	// 「没看到 reasoning_content」并不能证明流式路径正确 —— 那只是
	// 走了一条根本没被测试的分支。这种情况要报出来，不能算通过。
	if tc.body["stream"] == true && !isSSE(data) {
		out.err = "expected an SSE stream but got a non-SSE body (streaming path not exercised)"
		return out
	}
	if resp.StatusCode != http.StatusOK {
		out.err = strings.TrimSpace(string(data))
		if len(out.err) > 400 {
			out.err = out.err[:400] + "..."
		}
		return out
	}
	out.snippet = snippet(c.protocol, data)
	return out
}

// isSSE 判断响应体是不是 Server-Sent Events。只看开头若干个非空行里
// 有没有 "data:" 或 SSE 注释行，避免扫描整个大响应。
func isSSE(data []byte) bool {
	head := data
	if len(head) > 2048 {
		head = head[:2048]
	}
	for _, line := range strings.Split(string(head), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return strings.HasPrefix(line, "data:") || strings.HasPrefix(line, ":") ||
			strings.HasPrefix(line, "event:")
	}
	return false
}

func buildBody(c config, tc testCase) map[string]any {
	body := map[string]any{"model": tc.model}
	for k, v := range tc.body {
		body[k] = v
	}

	switch c.protocol {
	case "anthropic":
		body["max_tokens"] = 2048
		body["messages"] = []any{map[string]any{
			"role":    "user",
			"content": prompt(),
		}}
	default:
		body["messages"] = []any{map[string]any{
			"role":    "user",
			"content": prompt(),
		}}
		if c.protocol == "response" {
			body["max_output_tokens"] = 2048
		}
	}
	if tc.body["stream"] == true {
		body["stream"] = true
	}
	return body
}

// prompt 要能稳定触发思考：纯算术题在不思考的模型上也会答对，
// 拿不到「思考了没有」的信号。
func prompt() string {
	return "A train travels 120 km at 80 km/h, then 90 km at 45 km/h. " +
		"Compute the total time in minutes. Show only the final number."
}

func pathFor(protocol string) string {
	switch protocol {
	case "anthropic":
		return "/v1/messages"
	case "response":
		return "/v1/responses"
	default:
		return "/v1/chat/completions"
	}
}

func setAuth(req *http.Request, protocol, key string) {
	if protocol == "anthropic" {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
}

// parseEvidence 从响应里抽三个信号：有没有思考内容、思考 token 数、输出 token 数。
func parseEvidence(protocol string, data []byte) (hasReasoning bool, reasonToks, outputToks int) {
	if protocol == "anthropic" {
		var r struct {
			Content []struct {
				Type     string `json:"type"`
				Thinking string `json:"thinking"`
			} `json:"content"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &r) == nil {
			for _, b := range r.Content {
				if b.Type == "thinking" && strings.TrimSpace(b.Thinking) != "" {
					hasReasoning = true
				}
			}
			return hasReasoning, 0, r.Usage.OutputTokens
		}
		// 流式：找 content_block_start / delta 里的 thinking。
		s := string(data)
		return strings.Contains(s, `"type":"thinking"`), 0, 0
	}

	if protocol == "response" {
		var r struct {
			Output []struct {
				Type    string `json:"type"`
				Summary []struct {
					Text string `json:"text"`
				} `json:"summary"`
			} `json:"output"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
				Details      struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal(data, &r) == nil {
			for _, item := range r.Output {
				if item.Type == "reasoning" {
					hasReasoning = true
				}
			}
			return hasReasoning, r.Usage.Details.ReasoningTokens, r.Usage.OutputTokens
		}
		s := string(data)
		return strings.Contains(s, `"type":"reasoning"`), 0, 0
	}

	var r struct {
		Choices []struct {
			Message struct {
				ReasoningContent *string `json:"reasoning_content"`
				Reasoning        *string `json:"reasoning"`
			} `json:"message"`
			Delta struct {
				ReasoningContent *string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
			Details          struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &r) == nil {
		for _, ch := range r.Choices {
			if s := ch.Message.ReasoningContent; s != nil && strings.TrimSpace(*s) != "" {
				hasReasoning = true
			}
			if s := ch.Message.Reasoning; s != nil && strings.TrimSpace(*s) != "" {
				hasReasoning = true
			}
			if s := ch.Delta.ReasoningContent; s != nil && strings.TrimSpace(*s) != "" {
				hasReasoning = true
			}
		}
		return hasReasoning, r.Usage.Details.ReasoningTokens, r.Usage.CompletionTokens
	}
	s := string(data)
	return strings.Contains(s, `"reasoning_content"`), 0, 0
}

func snippet(protocol string, data []byte) string {
	switch protocol {
	case "anthropic":
		var r struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(data, &r) == nil {
			for _, b := range r.Content {
				if b.Type == "text" {
					return truncate(b.Text, 120)
				}
			}
		}
	case "response":
		var r struct {
			Output []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if json.Unmarshal(data, &r) == nil {
			for _, item := range r.Output {
				for _, c := range item.Content {
					if c.Text != "" {
						return truncate(c.Text, 120)
					}
				}
			}
		}
	default:
		var r struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &r) == nil && len(r.Choices) > 0 {
			return truncate(r.Choices[0].Message.Content, 120)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func report(results []result) {
	fmt.Printf("%-28s %-6s %-6s %-10s %-9s %s\n",
		"用例", "状态", "思考?", "思考tok", "耗时", "备注")
	fmt.Println(strings.Repeat("-", 100))

	verdict := map[string][]bool{}
	for _, r := range results {
		think := "?"
		if r.status == http.StatusOK {
			if r.reasoning {
				think = "有"
			} else {
				think = "无"
			}
		}
		note := r.snippet
		if r.err != "" {
			note = "ERR " + r.err
		}
		fmt.Printf("%-28s %-6d %-6s %-10s %-9s %s\n",
			truncate(r.tc.name, 26), r.status, think,
			fmt.Sprintf("%d", r.reasonToks),
			r.duration.Round(time.Millisecond), truncate(note, 60))

		if r.err == "" && r.status == http.StatusOK && (r.tc.wantOff || r.tc.wantOn) {
			verdict[r.tc.name] = append(verdict[r.tc.name], r.reasoning)
		}
	}

	fmt.Println()
	fmt.Println("判定：")
	names := make([]string, 0, len(verdict))
	for name := range verdict {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := verdict[name]
		ok := true
		wantOff := strings.Contains(name, "off")
		for _, reasoned := range v {
			if wantOff && reasoned {
				ok = false // 要关却还在思考
			}
			if !wantOff && !reasoned {
				ok = false // 要开却没思考
			}
		}
		mark := "通过"
		if !ok {
			mark = "不通过"
		}
		fmt.Printf("  %-28s %s\n", name, mark)
	}
}

func allOK(results []result) bool {
	for _, r := range results {
		if r.err != "" {
			return false
		}
		if r.status != http.StatusOK {
			return false
		}
		if r.tc.wantOff && r.reasoning {
			return false
		}
		if r.tc.wantOn && !r.reasoning {
			return false
		}
	}
	return true
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
