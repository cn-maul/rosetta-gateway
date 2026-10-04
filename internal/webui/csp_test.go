package webui

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestIndexHTML_HasNoInlineScript 守住 CSP 与产物之间的一致性。
//
// 管理面的 CSP 是 script-src 'self'，**不含** 'unsafe-inline'
// （见 internal/server/server.go 的 SecurityHeaders）。内联 <script> 会被
// 浏览器静默拦掉 —— 主题防闪功能直接坏掉（暗色用户首帧白闪回归），
// 而浏览器只在控制台报一条违规，Go 侧的 build / vet / test 全绿，
// 前端的 vue-tsc 与 vite build 也全绿。没有任何一道自动化关卡会失败。
//
// 这就是它需要一个测试的原因：不是「这段 HTML 不好看」，而是
// 「内联脚本在当前 CSP 下等于不存在，且失败无声」。
func TestIndexHTML_HasNoInlineScript(t *testing.T) {
	data, err := fs.ReadFile(StaticFS, "dist/index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)

	// 手工扫描 <script ...>...</script>，逐个判断是否带 src。
	// 不用正则：RE2 不支持负向前瞻，而「<script 但没有 src>」正是这里要
	// 匹配的形态。
	for _, tag := range scriptTags(html) {
		open := tag[:strings.Index(tag, ">")+1]
		if strings.Contains(strings.ToLower(open), " src=") {
			continue // 外链脚本，CSP 'self' 放行
		}
		body := strings.TrimSpace(tag[len(open) : len(tag)-len("</script>")])
		if body == "" {
			continue
		}
		t.Errorf("index.html 含内联脚本，CSP script-src 'self' 会拦掉它：\n%s",
			truncate(body, 200))
	}
}

// scriptTags 抽出 HTML 里所有成对的 <script ...>...</script> 片段。
// 大小写不敏感，容忍属性跨行。
func scriptTags(html string) []string {
	var out []string
	lower := strings.ToLower(html)
	for i := 0; ; {
		start := strings.Index(lower[i:], "<script")
		if start < 0 {
			return out
		}
		start += i
		// <scriptfoo 不算。
		if start+7 < len(lower) && !isTagNameBreak(lower[start+7]) {
			i = start + 7
			continue
		}
		gt := strings.IndexByte(html[start:], '>')
		if gt < 0 {
			return out
		}
		openEnd := start + gt + 1
		end := strings.Index(lower[openEnd:], "</script")
		if end < 0 {
			return out
		}
		closeEnd := openEnd + end + len("</script>")
		if gt := strings.IndexByte(html[openEnd:closeEnd], '>'); gt >= 0 {
			closeEnd = openEnd + gt + 1
		}
		out = append(out, html[start:closeEnd])
		i = closeEnd
	}
}

func isTagNameBreak(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '>' || c == '/'
}

// TestThemeInitIsExternal 确认主题防闪脚本确实以独立文件存在。
//
// 断言它被 index.html 同步引用 —— 用 type="module" 引用会让它变成
// defer 语义（文档解析完才执行），那时 CSS 已应用、首帧已画完，
// 防闪就失去意义，所以必须同时是非 module 的 <script src>。
func TestThemeInitIsExternal(t *testing.T) {
	data, err := fs.ReadFile(StaticFS, "dist/index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)

	ref := regexp.MustCompile(`<script\s+src="([^"]*theme-init\.js)"\s*>\s*</script>`)
	m := ref.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("index.html 应以非 module 的同步 <script src> 引用 theme-init.js，实际：\n%s", html)
	}

	// 被引用的文件必须真实存在于 embed FS 中。
	if _, err := fs.ReadFile(StaticFS, "dist/"+strings.TrimPrefix(m[1], "./")); err != nil {
		t.Fatalf("index.html 引用了不存在的 %s: %v", m[1], err)
	}
}

// TestCSPWouldAllowBuiltAssets 是 CSP 与产物的交叉校验：
// index.html 只能引用同源（相对路径）的资源，且不能有任何内联事件处理器
// （onclick=... 之类同样受 script-src 约束，会被静默拦掉）。
func TestCSPWouldAllowBuiltAssets(t *testing.T) {
	data, err := fs.ReadFile(StaticFS, "dist/index.html")
	if err != nil {
		t.Fatalf("read dist/index.html: %v", err)
	}
	html := string(data)

	// 内联事件处理器：script-src 不管这些，unsafe-hashes / script-src-attr
	// 才会 —— 当前 CSP 两者都没有，所以 onclick= 一定被拦。
	inlineHandler := regexp.MustCompile(`(?i)<[a-z][^>]*\son[a-z]+\s*=`)
	if m := inlineHandler.FindString(html); m != "" {
		t.Errorf("index.html 含内联事件处理器，会被 CSP 拦掉：%s", truncate(m, 120))
	}

	// 外链脚本必须同源：base 是 './'，产物里只应出现相对路径。
	scriptSrc := regexp.MustCompile(`<script[^>]*\bsrc="([^"]+)"`)
	for _, m := range scriptSrc.FindAllStringSubmatch(html, -1) {
		src := m[1]
		if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "//") {
			t.Errorf("index.html 引用了外部脚本 %s，CSP script-src 'self' 会拦掉", src)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
