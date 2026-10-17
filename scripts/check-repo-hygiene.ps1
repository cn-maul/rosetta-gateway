# 仓库卫生本地守卫：push 前跑一遍，等价于 CI 里 repo-hygiene job 的那次检查。
#
# # 为什么本地还要一份（CI 已经有同样的 job 了）
#
# CI 是**push 之后**才跑的。而「把 gateway.db 推上去」这个动作一旦完成，
# blob 就进了历史 —— 删掉当前文件只是让它变成 dangling，`git clone` 一样
# 捞得出来，filter-repo / BFG 能改写但要强推、要所有人重新 clone。
# 也就是说：CI 这道闸拦住的只是「下一次别再把脏东西推到主干」，
# 而这一次的泄露已经发生了。
#
# 所以真正的性价比在 push **之前**：本地一条命令，几秒钟，
# 把「索引里已经有脏文件」这件事在推出去之前拦下。
# 用法：
#
#     pwsh -File scripts/check-repo-hygiene.ps1
#     pwsh -File scripts/check-repo-hygiene.ps1 -RepoRoot D:\some\clone
#
# 退出码 0 = 索引干净，可以 push；1 = 有问题（逐条打印文件名与修复命令）。
#
# # 为什么它只读，绝不改任何文件
#
# 它要做的是「报告」，不是「修复」。`git rm --cached` 会把文件从索引里摘掉，
# 这改变了你即将提交的内容 —— 一个检查脚本不该在你不看的时候动你的暂存区。
# 所以脚本只跑 `git ls-files` / `git status` / `git check-ignore` 这类只读命令，
# 把该跑的命令**打印**出来，由人决定要不要执行。
#
# # 为什么用 git ls-files 而不是再检查一遍 .gitignore
#
# .gitignore 只对**未跟踪**文件生效；已经 add 过的文件即使后来被写进
# .gitignore，git 也照样跟踪它（`git add -f` 更是直接绕过）。
# 所以唯一的真相来源是「索引里到底有什么」。ignore 规则管未来，
# 这个脚本管现在，两层缺一不可。

[CmdletBinding()]
param(
    # 要检查的仓库根，默认 = 本脚本所在 scripts/ 的上一级。
    # 用 Split-Path -Parent 而不是 `Join-Path $PSScriptRoot '..\..'` +
    # Resolve-Path：后者在 PowerShell 5.1 上会把末尾的 `..` 也当成一级目录，
    # 多退一层（实测如此）。Split-Path -Parent 是纯字符串运算，结果确定。
    # 暴露成参数是为了能对一份临时克隆做验证（负向测试），
    # 也让「检查另一个 worktree」不必改脚本。
    [string]$RepoRoot = (Split-Path -Parent $PSScriptRoot)
)

$ErrorActionPreference = 'Stop'

$repo = $RepoRoot

# ---- 规则表 ---------------------------------------------------------------
# 与 .github/workflows/ci.yml 的 repo-hygiene job 保持同一口径。
# 每条 = 一类「绝不该入库」的东西，附上它为什么不该入库。
$patterns = @(
    @{
        Id       = 'binary-artifact'
        Label    = '二进制产物'
        Regex    = '\.(exe|dll|so|db|db-shm|db-wal|sqlite|sqlite3|key|pem|crt|log|bak|bak-[0-9].*|oldbak)$'
        Why      = '构建产物 / 数据库 / 私钥 / 日志 / 备份。进了历史就擦不掉，clone 一样能捞出来。'
        CaseIns  = $true
    }
    @{
        Id       = 'session-secret'
        Label    = '会话签名密钥'
        Regex    = '^(.*/)?session_secret(\.oldbak)?$'
        Why      = 'JWT 签名密钥。拿到它可以伪造任意用户的登录会话，无需密码。'
        CaseIns  = $false
    }
    @{
        Id       = 'master-key'
        Label    = '主密钥'
        Regex    = '^(.*/)?master\.key(\.oldbak)?$'
        Why      = '44字符 base64url。拿到它能解出 providers 里所有 api_key。'
        CaseIns  = $false
    }
    @{
        Id       = 'local-config'
        Label    = '本地配置'
        Regex    = '^(.*/)?(admin_auth|config)\.json$'
        Why      = '含 admin_token / providers api_key / routes。入库的那份是 config.example.json。'
        CaseIns  = $false
    }
    @{
        Id       = 'runtime-dir'
        Label    = '运行目录'
        Regex    = '^(data|bin|work|tools)/'
        Why      = '运行数据 / 构建产物 / 上游过程文档 / 一次性调试脚本。源码在 cmd/ 与 internal/，不在这几个目录里。'
        CaseIns  = $false
    }
)

Write-Host "=== 仓库卫生检查（只读，不会修改任何文件） ===" -ForegroundColor Cyan
Write-Host "仓库 : $repo"
Write-Host ""

Push-Location $repo
try {
    # ---- 前置条件：确实在 git 仓库里 --------------------------------------
    if (-not (Test-Path (Join-Path $repo '.git'))) {
        # 允许 worktree / submodule：.git 可能是文件而不是目录
        $inside = & git rev-parse --is-inside-work-tree 2>&1
        if ($LASTEXITCODE -ne 0 -or "$inside".Trim() -ne 'true') {
            Write-Host "[FAIL] 不在 git 仓库内：$repo" -ForegroundColor Red
            exit 1
        }
    }

    # ---- 1. 索引里的脏文件（主检查，与 CI 等价） --------------------------
    $tracked = @(& git ls-files)
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[FAIL] git ls-files 失败" -ForegroundColor Red
        exit 1
    }
    Write-Host "已跟踪文件 : $($tracked.Count) 个"
    Write-Host ""

    $hits = New-Object System.Collections.Generic.List[object]
    foreach ($p in $patterns) {
        $opt = if ($p.CaseIns) { 'IgnoreCase' } else { 'CaseSensitive' }
        $matched = @($tracked | Where-Object { $_ -match $p.Regex } | Sort-Object -Unique)
        foreach ($m in $matched) {
            $hits.Add([pscustomobject]@{
                File   = $m
                Rule   = $p.Id
                Label  = $p.Label
                Why    = $p.Why
                Option = $opt
            })
        }
    }

    if ($hits.Count -gt 0) {
        Write-Host "发现 $($hits.Count) 个不该入库的文件：" -ForegroundColor Red
        Write-Host ""
        foreach ($h in ($hits | Sort-Object File)) {
            Write-Host ("  [{0}] {1}" -f $h.Label, $h.File) -ForegroundColor Red
            Write-Host ("        {0}" -f $h.Why) -ForegroundColor DarkGray
        }
        Write-Host ""
        Write-Host "修复（保留本地文件、只从索引移除）：" -ForegroundColor Yellow
        foreach ($f in ($hits | ForEach-Object { $_.File } | Sort-Object -Unique)) {
            Write-Host "    git rm --cached `"$f`""
        }
        Write-Host ""
        Write-Host "若这些文件已经在历史里，删当前文件不足以消除泄露（clone 仍能取出）：" -ForegroundColor Yellow
        Write-Host "    git filter-repo --path <file> --invert-paths"
        Write-Host "  （会重写历史，需要强推，并让所有协作者重新 clone）"
        Write-Host ""
        # 不在这里 exit：下面的 .gitignore 防线自检同样要做。
        # 两项问题同时存在时不罕见（比如刚把 .gitignore 改成 CRLF，
        # 同时又手滑 `git add -f` 了一个 exe），只报第一项会让人
        # 修完一遍再跑一遍才发现第二项，白跑一轮。
        $indexDirty = $true
    }
    else {
        Write-Host "[PASS] 索引干净：无 exe / db / 密钥 / 数据文件。" -ForegroundColor Green
        $indexDirty = $false
    }

    # ---- 2. 顺带验证 ignore 规则本身仍然有效 ------------------------------
    # 只检查、不修复。它回答的是另一个问题：「第一层防线（.gitignore）还在吗？」
    # 上面的 ls-files 只覆盖已经 add 过的文件；这条覆盖未来的误 add。
    #
    # 每个探针都同时包含「应被忽略」与「绝不该被忽略」两类：
    # 前者证明防线在，后者证明防线没有误伤源码（`gateway/` 曾把
    # `cmd/gateway/` 整个排除，导致 billing.go 从未入库、CI 报
    # `undefined: billingSubscription` —— 这种过度忽略比不忽略更难发现）。
    $probes = @(
        @{ Path = 'gateway.exe';                     ShouldIgnore = $true;  Note = '根目录二进制' }
        @{ Path = 'bin/gateway.exe';                 ShouldIgnore = $true;  Note = 'bin/ 下二进制' }
        @{ Path = 'bin/master.key';                  ShouldIgnore = $true;  Note = '主密钥' }
        @{ Path = 'bin/session_secret';              ShouldIgnore = $true;  Note = '会话密钥' }
        @{ Path = 'bin/data/gateway.db';             ShouldIgnore = $true;  Note = '整库' }
        @{ Path = 'bin/data/gateway.db.oldbak';      ShouldIgnore = $true;  Note = '库备份' }
        @{ Path = 'gateway.exe.bak-20260921-105153'; ShouldIgnore = $true;  Note = '带时间戳的 exe 备份' }
        @{ Path = 'session_secret';                  ShouldIgnore = $true;  Note = '根目录会话密钥' }
        @{ Path = 'config.json';                     ShouldIgnore = $true;  Note = '本地配置' }
        @{ Path = 'work/design.md';                  ShouldIgnore = $true;  Note = 'work/ 过程文档' }
        @{ Path = 'scripts/authz_probe/dbdump.exe';  ShouldIgnore = $true;  Note = '子目录构建产物' }
        @{ Path = 'data/gateway.db';                 ShouldIgnore = $true;  Note = 'data/ 目录' }
        @{ Path = 'cmd/gateway/main.go';             ShouldIgnore = $false; Note = '源码（不能被误伤）' }
        @{ Path = 'cmd/gateway/billing.go';          ShouldIgnore = $false; Note = '源码（曾被误伤过）' }
        @{ Path = 'cmd/migrate-legacy/main.go';      ShouldIgnore = $false; Note = '源码（曾被漏出测试范围）' }
        @{ Path = 'internal/webui/dist/index.html';  ShouldIgnore = $false; Note = 'go:embed 数据（必须入库）' }
        @{ Path = 'config.example.json';             ShouldIgnore = $false; Note = '示例配置（必须入库）' }
    )

    $probeFail = 0
    Write-Host ""
    Write-Host "--- .gitignore 防线自检 ---" -ForegroundColor Cyan
    foreach ($pr in $probes) {
        $out = & git check-ignore -q -- $pr.Path 2>&1
        $ignored = ($LASTEXITCODE -eq 0)
        $ok = ($ignored -eq $pr.ShouldIgnore)
        if (-not $ok) { $probeFail++ }
        $tag = if ($ok) { 'PASS' } else { 'FAIL' }
        $color = if ($ok) { 'Green' } else { 'Red' }
        $expect = if ($pr.ShouldIgnore) { '应忽略' } else { '不忽略' }
        Write-Host ("  [{0}] {1,-8} {2,-38} {3}" -f $tag, $expect, $pr.Path, $pr.Note) -ForegroundColor $color
    }

    # ---- 3. 空行 catch-all 陷阱自检 ----------------------------------------
    # 一个具体踩中过的坑（2026-10-17 在本机复现，机理见 .gitignore 顶部注释）：
    # 只要 .gitignore 是 CRLF 行尾，git 逐行剥行尾时空行的 `\r\n` 剥掉 \n 后
    # 会残留一个 `\r`，而 `\r` 在 git 的 fnmatch 里能匹配任意目录名 ——
    # 于是每个空行都变成一条「匹配任意层级任意目录」的幽灵规则：
    #     $ git check-ignore -v zzz/
    #     .gitignore:43:	zzz/        ← 第 43 行原本是个空行
    # 后果不是「规则失效」而是**假报生效**：看起来该挡的都挡住了，
    # 实际上 `git status` 也在把真实的未跟踪目录藏起来。改回 LF 后
    # 同一份内容立刻恢复正常。
    # 用几个绝不存在的目录名探一下：它们若被「忽略」，说明陷阱复发了。
    Write-Host ""
    Write-Host "--- 空行通配陷阱自检（.gitignore 必须是 LF 行尾） ---" -ForegroundColor Cyan
    $canary = @('zzz/', 'qqq-nonexistent/', 'some/thing/')
    $trap = 0
    foreach ($c in $canary) {
        $out = & git check-ignore -v -- $c 2>&1
        if ($LASTEXITCODE -eq 0) {
            $trap++
            Write-Host ("  [FAIL] {0} 被忽略了 → {1}" -f $c, ($out -join ' ').Trim()) -ForegroundColor Red
        }
        else {
            Write-Host ("  [PASS] {0} 未被忽略（正常）" -f $c) -ForegroundColor Green
        }
    }
    if ($trap -gt 0) {
        Write-Host ""
        Write-Host "  .gitignore 里存在被 git 当成通配目录规则的空行（CRLF 行尾导致）。" -ForegroundColor Red
        Write-Host "  注意它不会让规则失效，而是**假报生效**：该挡的看起来都挡住了，" -ForegroundColor Red
        Write-Host "  但 git status 也在把真实的未跟踪目录藏起来，误以为加了规则就安全。" -ForegroundColor Red
        Write-Host "  修复（PowerShell，转成 LF 后重新提交）：" -ForegroundColor Yellow
        Write-Host "    (Get-Content .gitignore -Raw) -replace \"\`r\`n\",\"\`n\" | Set-Content .gitignore -NoNewline"
    }

    # ---- 汇总 --------------------------------------------------------------
    # 三项（索引 / ignore 有效性 / 空行陷阱）全部跑完后统一判定，
    # 一次性把所有问题说出来，避免「修一项、跑一遍、再发现一项」。
    Write-Host ""
    $problems = @()
    if ($indexDirty) { $problems += '索引里有不该入库的文件' }
    if ($probeFail -gt 0) { $problems += ('.gitignore 防线失效（' + $probeFail + ' 个探针不符）') }
    if ($trap -gt 0) { $problems += '.gitignore 存在空行通配陷阱（CRLF 行尾）' }

    if ($problems.Count -gt 0) {
        Write-Host "=== 结果：FAIL ===" -ForegroundColor Red
        foreach ($p in $problems) { Write-Host "  - $p" -ForegroundColor Red }
        Write-Host ""
        Write-Host "上面的每一节都给了对应的修复命令，逐条执行后重跑本脚本。" -ForegroundColor Yellow
        exit 1
    }

    Write-Host "=== 结果：PASS。索引干净，.gitignore 防线有效，可以 push。 ===" -ForegroundColor Green
    exit 0
}
finally {
    Pop-Location
}
