# 开发环境与协作约定

> 本文只保留**无法从代码与 CI 里直接读出来**的环境事实与工作约定。
> 架构与设计决策见 [DESIGN.md](DESIGN.md)；多用户/权限模型的历史沿革见
> [MULTIUSER.md](MULTIUSER.md)（冻结归档，代码里有多处注释以它的章节号为锚点）。
>
> 项目**状态类**信息（当前版本、未完成项）不在这里维护 —— 那属于 git 历史与
> 提交信息，放进文档只会变成下一次就过时的陈述。

---

## 一、本机环境事实

### `-race` 必须显式指定 CC

```powershell
$env:CC="C:/mingw64/bin/gcc.exe"
go test -race -count=1 ./...
```

默认 PATH 上的 MinGW 装在 `C:\Program Files\mingw64`，**路径含空格**，
`ld.exe` 解析不了（报 `cannot find C:/Program: No such file or directory`），
于是 `go test -race` 在**链接阶段**就失败。

这**不等于本机跑不了 race 检测** —— 机器上另有一份无空格的
`C:\mingw64\bin\gcc.exe`，显式指定即可。同样的说明也记在
`.github/workflows/ci.yml` 的对应步骤旁。

> 注意这份「无空格副本」是**本机事实，不是仓库保证**：换一台机器、
> 或重装 MinGW 之后 `C:\mingw64\bin\gcc.exe` 可能不存在。照抄这条命令前
> 先 `Test-Path` 一下；真缺就把带空格那份用引号包起来
> （`$env:CC='"C:\Program Files\mingw64\bin\gcc.exe"'`）—— 内层引号是
> 给 Go 的工具链解析器看的，PowerShell 会把它当整体传下去。

CI 跑在 ubuntu，用默认 gcc 即可，不需要这个变量。

> 为什么产物不受影响：race detector 只支持 cgo，所以**测试**需要 CGO；
> 而 `modernc.org/sqlite` 是纯 Go 实现（见下），**产物**用
> `CGO_ENABLED=0` 就能交叉编译。「测试需要 gcc」与「产物纯 Go」不矛盾。

### 产物是纯 Go，可直接交叉编译

`modernc.org/sqlite` 是 SQLite 的 Go 翻译实现（把 C 转写为 Go），不是绑定原生库。
仓库里没有任何 `import "C"`。实测：

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build ./cmd/gateway   # OK
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build ./cmd/gateway   # OK
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build ./cmd/gateway   # OK
```

无需交叉编译工具链，这是选它而不是 `mattn/go-sqlite3` 的主要原因。

### 依赖拉取：goproxy.cn 的 sumdb 对本项目返回 404

`goproxy.cn` 的 sumdb lookup 对 `github.com/cn-maul/*` 返回 **404**，
拉新版本需要：

```bash
GOSUMDB=off GOPRIVATE=github.com/cn-maul/* go get ...
```

CI 侧需同样配置，否则升级依赖会红。

### Git 走 HTTPS + 7897 代理（不是 SSH）

这台机器的 `~/.ssh` 里**只有 `known_hosts`，没有任何私钥**，也没有运行
ssh-agent。代理只解决网络出口，不解决 SSH 认证 —— 所以 `git@github.com` 必然失败。

`origin` 已改为 `https://github.com/cn-maul/rosetta-gateway.git`，
凭据来自 `~/.git-credentials`。走代理拉取时给 git 带上 `-c http.proxy` /
`-c https.proxy` 指向 `127.0.0.1:7897`。

若将来补上私钥想改回 SSH，**先验证再改**：

```powershell
git remote set-url origin git@github.com:cn-maul/rosetta-gateway.git
ssh -T git@github.com    # 确认认证通过后再改，否则会把自己锁在门外
```

---

## 二、前端约定

### 改完前端必须 sync-embed

```bash
cd web
npm ci
npm run typecheck
npm run build
node sync-embed.mjs     # 把产物同步进 internal/webui/dist（go:embed 的数据源）
```

**忘了这一步的后果是静默的**：Docker 镜像只 `COPY . .` + `go build`，
打包的是仓库里**已入库**的 `internal/webui/dist`，**从不**重新构建前端。
于是改完 `web/src` 却忘了 sync，就会打出一个带**旧 UI** 的 tag。
CI 的 `webui-sync` job 会拦这个（重新构建一遍，与入库产物比对），
但本地就绪不等于 tag 就绪。

### 行尾：Windows 上编辑 `.vue` 极易引入 CRLF

CRLF 会让产物 hash 与 CI 上算出的不一致，触发 webui-sync 红灯。
改完前端用 `git diff --check` 确认没有 CRLF。

> 例外：`DOCKER.md` 是仓库里**唯一**以 CRLF 入库的 Markdown（历史原因）。
> 对它执行 `git diff --check` 会报「每行末尾有 CR」，**这条可以接受** ——
> 转 LF 会产生数百行纯换行符噪声。曾尝试加 `.gitattributes` 修正，已撤销。

### TypeScript 钉在 6.x，不要升到 7.x

TypeScript 7 是 Go 原生重写版（tsgo），**不再暴露 `typescript/lib/tsc`
这个子路径**，而 `vue-tsc` 恰恰依赖它：

```text
Error [ERR_PACKAGE_PATH_NOT_EXPORTED]: Package subpath './lib/tsc'
  is not defined by "exports" in .../typescript/package.json
```

这不是警告，是**工具链直接起不来**，`npm run typecheck` 与 CI 的
webui-sync job 都会红。tsgo 没有内置替代品 —— 它是编译器，不解析
`.vue` 单文件组件，那部分是 vue-tsc（Volar）的职责。

因此 `package.json` 用 `~6.0.3` 钉住。将来 vue-tsc 官方支持 tsgo 之后
再单独评估，不要跟着 npm 顺手升上去。

### Node 至少 22.12（Vite 8 的 engines 下限）

`vite` 已升到 8.x，它的 `engines` 是 `^20.19.0 || >=22.12.0` ——
CI 因此把 `node-version` 写成**具体的 `22.12`** 而不是 `'22'`
（`.github/workflows/ci.yml` 的 `Set up Node` 步骤旁有完整说明）。
写 `'22'` 会解析到当时最新的 22.x、恰好满足下限，那是靠浮动侥幸：
哪天有人写成 `22.0` 或某个旧 minor，构建会直接失败，且报错来自 npm 的
`engines` 检查，与被测代码无关，很难一眼归因。

本机没有 Node 22.12+ 时的表现是 `npm install` 阶段就报 `EBADENGINE`，
而不是编译报错 —— 看到它先去核对 `node -v`，别去查代码。

### lockfile 的 registry

`web/package-lock.json` 的 `resolved` 曾全部指向 `registry.npmmirror.com`，
已于 2026-10-15 改回 `https://registry.npmjs.org`。理由：

- CI 跑在 GitHub Actions（美国），跨境访问镜像站慢且不稳；
- 镜像站长期不可用时 CI 会直接挂，且表象看起来像「网络问题」。

若将来需要换源，改完必须跑 `npm ci` 验证 **integrity 哈希仍然通过**
（镜像站有时会重新打包 tarball，导致哈希对不上）。

---

## 三、代码风格

- 注释**中文、高密度**，每个非显然的决策都解释「**为什么**」——
  尤其是「看起来可以简化」的地方，要写清不简化的原因。
- 注释里的数字**引用常量名**而不是重述数值（如写 `MicrosPerCent` 而非「乘 1e6」）。
- 测试断言要钉**行为**，不是猜测出来的值。反例：不要断言「上游没被调用」，
  除非先证明它**能**被调用 —— 否则那条断言在「根本没走到」时也会通过，是假绿。
- 探测（探针）用完即删，但**结论写进注释或正式文档**。

---

## 四、提交前门禁

```bash
# 后端
gofmt -l cmd/ internal/          # 必须无输出
go build ./cmd/... ./internal/...
go vet ./cmd/... ./internal/...
go test ./cmd/... ./internal/... -count=1
$env:CC="C:/mingw64/bin/gcc.exe"; go test -race ./cmd/... ./internal/... -count=1   # 本机需带 CC

# 前端
cd web && npm ci && npm run typecheck && npm run build && node sync-embed.mjs
```

**按批次验证，门禁跑全量。** 开发小改时不要求每改一行就完整跑一遍；同一组相关
改动完成、准备提交或切换任务时再跑上述门禁。门禁本身仍要跑完整包，而不是只跑
相关的 `-run TestXxx`：历史上出过两次「只跑子集，cmd/gateway 整包红一轮」的事故，
契约改动必须全量跑来兜底。CI 里也是这么配的。

这里的 `./cmd/... ./internal/...` 刻意不包含 `work/`：仓库里的审计/实验目录可能
有独立的 `main` 代码，但它不属于产品构建与测试门禁。

---

## 五、接手时先做两件事

1. **`git fetch` 校准本地跟踪引用。** 不要信本地已有的 `origin/main` ——
   它可能陈旧且与远端不符，这时 `git status` 会给出「ahead/behind N」的错误结论。
2. **读代码，不读旧结论。** 文档与注释里都可能有过时描述（这个仓库的历史已经
   出现过多次「文档说已修，代码里其实没修」）。判断某结论是否成立，
   以对应源码的 `file:line` 为准。

