package webui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestLockfileVersionMatchesPackageJSON 守住 package-lock 与 package.json 同版本。
//
// # 为什么要单独测（2026-10-21 修 A11-P2-2）
//
// 版本号在本仓有两处副本：package.json 的 version，以及 package-lock.json 的
// 根 version 与根 packages 条目的 version。三者的读者各不相同：
// CI 的 npm ci 读 lockfile，Docker 的版本解析读 package.json，内嵌版本测试
// 读 package.json 与 embed 产物。没有任何一处会拿这几样互相比对，
// 于是漂移发生时全绿：二进制带着 package.json 的版本号，依赖解析走的是
// lockfile 的旧版本，页脚、镜像 tag、npm ci 三方对不上，排查时最先被
// 怀疑的反而是 CI 缓存。本仓确实漂移过一次（package.json 1.6.5 / lockfile 1.6.0）。
//
// # 为什么只校验根字段，不动 packages 下各项
//
// node_modules/@jridgewell/sourcemap-codec 的 version 是依赖自己的
// 版本号，与工程版本无关，被一起改掉反而是破坏。
func TestLockfileVersionMatchesPackageJSON(t *testing.T) {
	pkgPath := filepath.Join("..", "..", "web", "package.json")
	praw, err := os.ReadFile(pkgPath)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", pkgPath, err)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(praw, &pkg); err != nil {
		t.Fatalf("解析 package.json: %v", err)
	}
	if pkg.Version == "" {
		t.Fatal("package.json 里没有 version 字段")
	}

	lockPath := filepath.Join("..", "..", "web", "package-lock.json")
	lraw, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", lockPath, err)
	}
	// lock 只解出两个位置：根 version 与 packages 的根条目（键是空字符串）。
	// 用 map 而不是给根条目配一个具名结构体：那个键名在字面量里是空的，
	// 写出来要靠转义绕，读的人第一眼也看不出在解什么；map 直接让
	// lock.Packages[""] 自己说话。其余依赖项不会被解到，正好不需要。
	var lock struct {
		Version  string `json:"version"`
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(lraw, &lock); err != nil {
		t.Fatalf("解析 package-lock.json: %v", err)
	}

	root, ok := lock.Packages[""]
	if !ok {
		t.Fatal("package-lock.json 的 packages 里没有根条目（键为空字符串）")
	}

	if lock.Version != pkg.Version {
		t.Errorf("package-lock.json 的 version = %q，package.json = %q -- "+
			"CI 用 npm ci 解析依赖、Docker 读 package.json 取版本号，两边漂移时 "+
			"没有任何关卡会报错。请用 package.json 的版本号同步 lockfile 的根 version。",
			lock.Version, pkg.Version)
	}
	if root.Version != pkg.Version {
		t.Errorf("package-lock.json 根 packages 条目的 version = %q，package.json = %q -- "+
			"npm ci 在依赖树根节点读的是这个字段，它与根 version 不一致会让 npm "+
			"认为 lockfile 与 package.json 不同步。",
			root.Version, pkg.Version)
	}
}
