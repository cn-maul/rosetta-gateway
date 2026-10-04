package webui

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
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
// 掩盖真正要测的东西。这里按扩展名扫。
func embeddedBundle(t *testing.T) string {
	t.Helper()
	fsys, err := fs.Sub(StaticFS, "dist/assets")
	if err != nil {
		t.Fatalf("embed 里没有 dist/assets: %v", err)
	}
	ents, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("读 dist/assets: %v", err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".js") {
			b, err := fs.ReadFile(fsys, e.Name())
			if err != nil {
				t.Fatalf("读 %s: %v", e.Name(), err)
			}
			return string(b)
		}
	}
	t.Fatal("embed 的 dist/assets 里没有 .js 文件")
	return ""
}
