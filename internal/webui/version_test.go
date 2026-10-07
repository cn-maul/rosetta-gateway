package webui

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEmbeddedVersionMatchesPackageJSON 守住版本号的**单一来源**。
//
// 版本号的唯一来源是 web/package.json 的 version，它经两条路径分叉：
//   - vite.config.ts 的 define → __APP_VERSION__ → 页脚 `rosetta-gateway v1.4.0`
//   - build.ps1 的 -ldflags  → main.buildVersion → 启动日志 / 二进制字符串
//
// 这里守的是前一条：**embed 进二进制的产物必须与 package.json 同版本**。
//
// 为什么要专门测 —— 这个脱节是本仓踩过的真实坑，且**没有任何其他关卡会报错**：
//
//	cd web && npm run build && node sync-embed.mjs   # 产物已是 1.4.0
//	go build ./cmd/gateway# 忘了？embed 用的还是上一次的 dist
//
// //go:embed 是**编译期**固化：sync-embed 只改磁盘文件，不重编译就等于没生效。
// 此时 web/dist 对、internal/webui/dist 对、go vet / go test / vue-tsc 全绿，
// 只有人打开浏览器点开页脚，才会发现界面还写着旧版本 ——
// 而这恰好是最容易被当成「浏览器缓存了」而略过的现象（本轮就发生过一次）。
//
// 所以这里直接读 embed 内容（StaticFS），而不是读 web/dist：
// 前者是浏览器实际拿到的那份，后者可能压根没被编进二进制。
func TestEmbeddedVersionMatchesPackageJSON(t *testing.T) {
	pkgPath := filepath.Join("..", "..", "web", "package.json")
	raw, err := os.ReadFile(pkgPath)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", pkgPath, err)
	}
	var pkg map[string]any
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("解析 package.json: %v", err)
	}
	want, _ := pkg["version"].(string)
	if want == "" {
		t.Fatal("package.json 里没有 version 字段")
	}

	js := embeddedBundle(t)
	if !strings.Contains(js, `"`+want+`"`) {
		t.Errorf("内嵌前端产物里找不到版本号 %q —— "+
			"请重新执行 `cd web && npm run build && node sync-embed.mjs` "+
			"**并重新 go build**（//go:embed 是编译期固化，不重编译等于没生效）", want)
	}
}

// embeddedBundle 返回 embed 进去的那份 JS bundle 源码。
//
// 不硬编码 dist/assets 下的文件名：文件名带 content hash，版本升级后会变，
// 硬编码会让这个测试在每次版本升级时以「文件不存在」的形式失败，
// 掩盖真正要测的东西。
//
// **按 index.html 的引用来取，而不是「目录里第一个 .js」**：
// 路由改成懒加载后产物是一个入口 chunk 加十几个页面 chunk，版本号由
// vite 的 define 注入、只存在于**入口**（__APP_VERSION__ 在 main.ts / App.vue
// 这条依赖链上）。按文件名排序取第一个会命中 `_plugin-vue_export-helper-*.js`
// 或某个页面 chunk —— 里面根本没有版本号，于是这个测试会以
// 「版本对不上」为理由变红，而真实版本其实完全正确。这是比原缺陷更难查的
// 一种假故障：看报错像是「忘了重新 build」，实际是取错了文件。
// 而它恰恰是本测试想守住的那条纪律（「产物与源码同版本」）被自身实现破坏的样子，
// 所以必须在同一处改对。
func embeddedBundle(t *testing.T) string {
	t.Helper()

	index := readEmbedded(t, "dist/index.html")
	ref := regexp.MustCompile(`<script[^>]*\ssrc="([^"]*?/)?(index-[^"]+\.js)"`)
	m := ref.FindStringSubmatch(index)
	if m == nil {
		t.Fatalf("index.html 里找不到入口 bundle 的 <script src>：\n%s", index)
	}
	return readEmbedded(t, "dist/assets/"+m[2])
}

// readEmbedded 读 embed 进来的某个文件并转成字符串。
func readEmbedded(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(StaticFS, name)
	if err != nil {
		t.Fatalf("读 %s: %v", name, err)
	}
	return string(b)
}
