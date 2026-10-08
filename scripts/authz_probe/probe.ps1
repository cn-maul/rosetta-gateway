# 权限边界的**真实进程**探针。
#
# # 为什么需要它（以及它只补哪一块）
#
# 绝大部分权限边界已经在 internal/server/authz_matrix_test.go 里以进程内方式
# 覆盖（真实的 UserAuth → AdminGateGuard → 真实 handler，真实签发的 JWT）。
# 进程内测试有一个**结构性**盲区：`denyAdminKeyCreate` 定义在 package main 里，
# 外部测试包无法引用它。
#
# 所以本脚本补的就是那一块：启动**真实二进制**，用真实 HTTP 打
# POST /admin/api/keys，确认管理员被拦且**库里没多出 key**。
# 顺带把其余几条边界在真实进程上再验一遍（同一份代码、不同的构建路径，
# 能抓到「测试里用的构造顺序与 main.go 不一致」这类问题）。
#
# # 为什么不会碰 bin/data/gateway.db
#
# 用 ROSETTA_GW_HOME 把状态根目录整体重定向到本目录下的 run/。
# 于是 config.json / master.key / session_secret / gateway.db 全部落在隔离目录，
# 仓库里的 bin/data/gateway.db **一个字节都不会被读或写**。
# 这一点在脚本开头就会把两个路径都打印出来供核对。
#
# # 用法
#
#   pwsh -File scripts/authz_probe/probe.ps1
#
# 退出码 0 = 全部断言通过；1 = 有断言失败（逐条打印实测证据）。

$ErrorActionPreference = 'Stop'

$repo = Resolve-Path (Join-Path $PSScriptRoot '..\..')
$probeDir = Join-Path $PSScriptRoot 'run'
$exe = Join-Path $PSScriptRoot 'gateway-probe.exe'
$port = 18771
$base = "http://127.0.0.1:$port"

Write-Host "=== 权限边界真实进程探针 ===" -ForegroundColor Cyan
Write-Host "隔离状态目录 : $probeDir"
Write-Host "真实库(不碰) : $repo\bin\data\gateway.db"
Write-Host ""

# ---- 清理上一次的残留 -------------------------------------------------------
if (Test-Path $probeDir) { Remove-Item $probeDir -Recurse -Force }
New-Item -ItemType Directory -Path $probeDir -Force | Out-Null

# ---- 构建探针二进制（独立路径，不覆盖 bin/gateway.exe）----------------------
if (-not (Test-Path $exe)) {
    Write-Host "构建探针二进制…"
    Push-Location $repo
    try { & go build -o $exe ./cmd/gateway } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
}

# ---- 隔离配置 ---------------------------------------------------------------
@'
{
  "listen": "127.0.0.1:18771",
  "db_path": "./data/gateway.db",
  "log_level": "warn",
  "master_key_env": "ROSETTA_GW_MASTER_KEY",
  "defaults": { "upstream_timeout_ms": 120000, "stream_idle_timeout_ms": 60000, "max_retries": 2, "max_request_body_bytes": 33554432, "stream_first_token_timeout_ms": 30000, "failover_max_targets": 3, "failover_failure_threshold": 3 },
  "bootstrap": { "providers": null, "routes": null }
}
'@ | Set-Content -Path (Join-Path $probeDir 'config.json') -Encoding utf8

# ---- 启动 -------------------------------------------------------------------
$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $exe
$psi.UseShellExecute = $false
$psi.RedirectStandardOutput = $true
$psi.RedirectStandardError = $true
$psi.EnvironmentVariables['ROSETTA_GW_HOME'] = $probeDir
$proc = [System.Diagnostics.Process]::Start($psi)
Write-Host "已启动 pid=$($proc.Id)（状态隔离在 $probeDir）"

# 退出时无论如何都要收拾干净：残留进程占着端口会让下一次运行莫名其妙地失败。
$cleanup = {
    if ($proc -and -not $proc.HasExited) {
        try { $proc.Kill(); $proc.WaitForExit(5000) | Out-Null } catch {}
    }
}

try {
    # ---- 等就绪 -------------------------------------------------------------
    $ready = $false
    foreach ($i in 1..50) {
        Start-Sleep -Milliseconds 200
        if ($proc.HasExited) { throw "网关启动即退出（exit=$($proc.ExitCode)），见 $probeDir" }
        try {
            $r = Invoke-WebRequest "$base/admin/api/session" -SkipHttpErrorCheck -TimeoutSec 2
            if ($r.StatusCode -eq 200) { $ready = $true; break }
        } catch {}
    }
    if (-not $ready) { throw "网关 10 秒内未就绪" }
    Write-Host "网关就绪。`n"

    # ---- HTTP helper --------------------------------------------------------
    # UseCookies=false：会话令牌一律走 Authorization 头。
    # 若让 handler 存 cookie，sessionToken() 是 **cookie 优先**的，
    # 后续以别的身份发请求时会被上一个身份的 cookie 顶掉，测出假结果。
    $handler = New-Object System.Net.Http.HttpClientHandler
    $handler.UseCookies = $false
    $handler.AllowAutoRedirect = $false
    $http = New-Object System.Net.Http.HttpClient($handler)
    $http.Timeout = [TimeSpan]::FromSeconds(30)

    function Send-Probe {
        param([string]$Method, [string]$Path, [string]$Token, [string]$Body)
        $req = New-Object System.Net.Http.HttpRequestMessage([System.Net.Http.HttpMethod]::new($Method), "$base$Path")
        if ($Token) { $req.Headers.Add('Authorization', "Bearer $Token") }
        if ($Body)  { $req.Content = New-Object System.Net.Http.StringContent($Body, [Text.Encoding]::UTF8, 'application/json') }
        $resp = $http.SendAsync($req).GetAwaiter().GetResult()
        $text = $resp.Content.ReadAsStringAsync().GetAwaiter().GetResult()
        return [pscustomobject]@{ Status = [int]$resp.StatusCode; Body = $text }
    }

    $results = New-Object System.Collections.Generic.List[object]
    function Check {
        param([string]$Id, [string]$Detail, [bool]$Pass)
        $results.Add([pscustomobject]@{ Id = $Id; Detail = $Detail; Pass = $Pass })
        $tag = if ($Pass) { 'PASS' } else { 'FAIL' }
        $color = if ($Pass) { 'Green' } else { 'Red' }
        Write-Host ("  [{0}] {1} — {2}" -f $tag, $Id, $Detail) -ForegroundColor $color
    }

    Write-Host "--- 准备身份 ---" -ForegroundColor Cyan

    # 全新库 → ensureBootstrapAdmin 建出空密码 admin → bootstrap 设密并直接登录。
    $adminPass = 'AdminPassw0rd!'
    $bs = Send-Probe 'POST' '/admin/api/bootstrap' $null (@{ password = $adminPass } | ConvertTo-Json -Compress)
    if ($bs.Status -ne 200) { throw "bootstrap 失败: $($bs.Status) $($bs.Body)" }
    $adminTok = ($bs.Body | ConvertFrom-Json).token
    Write-Host "  管理员已引导并登录"

    $me = Send-Probe 'GET' '/admin/api/me' $adminTok $null
    # 注意：/admin/api/me **刻意不返回 id**（它有 username/role，但没有 id）。
    # 首轮探针实测踩到了这一点：把 $me.id 当管理员 id → 空串 →
    # B2 的 URL 变成 /users//balance（307 重定向）、B3 的 user_id 传了 null。
    # 所以管理员 id 必须从 /admin/api/users 里按 role 找（那里有 id）。
    $adminList = (Send-Probe 'GET' '/admin/api/users' $adminTok $null).Body | ConvertFrom-Json
    $adminRow = $adminList | Where-Object { $_.role -eq 'admin' } | Select-Object -First 1
    if (-not $adminRow) { throw "在 /admin/api/users 里找不到管理员行" }
    $adminId = $adminRow.id
    Write-Host "  管理员 username = $($adminRow.username)  id = $adminId  (来自 /users，/me 不含 id)"

    # 建一个普通用户（这正是管理员拿到数据面凭据的**唯一**正当途径）。
    $userPass = 'UserPassw0rd!'
    $cu = Send-Probe 'POST' '/admin/api/users' $adminTok (@{ username = 'probeuser'; password = $userPass; role = 'user' } | ConvertTo-Json -Compress)
    if ($cu.Status -ne 201) { throw "建普通用户失败: $($cu.Status) $($cu.Body)" }
    $userId = ($cu.Body | ConvertFrom-Json).id
    Write-Host "  普通用户 id = $userId"

    $lg = Send-Probe 'POST' '/admin/api/login' $null (@{ username = 'probeuser'; password = $userPass } | ConvertTo-Json -Compress)
    if ($lg.Status -ne 200) { throw "普通用户登录失败: $($lg.Status) $($lg.Body)" }
    $userTok = ($lg.Body | ConvertFrom-Json).token
    Write-Host "  普通用户已登录`n"

    # =====================================================================
    Write-Host "--- B. 管理员退出数据面：四条边界（真实进程）---" -ForegroundColor Cyan
    # =====================================================================

    # 副作用断言一律做「前后对比」，而不是「等于某个我猜的值」。
    #
    # 首轮探针实测踩到了这一点：我把断言写成「管理员 balance_cents 应为 NULL」，
    # 实际值是 0 —— ensureBootstrapAdmin 建号时没置 Unlimited，于是落的是 0
    # 而不是 NULL。那个 0 本身无害（管理员被挡在 /v1 之外，API 层又把管理员
    # 余额归一成「不限额」），却让一条**本该通过**的断言误报失败。
    # 正确的不变量是「被拒的操作没有改动它」，与它的具体取值无关。
    $dbFile = Join-Path $probeDir 'data\gateway.db'
    $dbdump = Join-Path $PSScriptRoot 'dbdump.exe'
    if (-not (Test-Path $dbdump)) {
        Push-Location $repo
        try { & go build -o $dbdump .\scripts\authz_probe\dbdump } finally { Pop-Location }
    }
    function Get-DbDump { return (& $dbdump $dbFile $adminId $userId) }
    function Get-DbSnapshot {
        $snap = @{}
        foreach ($l in (Get-DbDump)) { if ($l -match '^([^=]+)=(.*)$') { $snap[$Matches[1]] = $Matches[2] } }
        return $snap
    }
    $dbBaseline = Get-DbSnapshot
    Write-Host "  B 组动作前的库基线: $((Get-DbDump) -join '  ')"

    # B1. 管理员不能建 key（本脚本存在的首要理由：denyAdminKeyCreate 在 package main）
    $keysBefore = (Send-Probe 'GET' '/admin/api/keys' $adminTok $null).Body | ConvertFrom-Json
    $b1 = Send-Probe 'POST' '/admin/api/keys' $adminTok '{"name":"admin-self-key-probe"}'
    $keysAfter = (Send-Probe 'GET' '/admin/api/keys' $adminTok $null).Body | ConvertFrom-Json
    Check 'B1-status' "管理员 POST /keys → $($b1.Status) body=$($b1.Body)" ($b1.Status -eq 403)
    Check 'B1-message' "错误消息指向正确出路（含「普通用户」）" ($b1.Body -like '*普通用户*')
    Check 'B1-no-side-effect' "库里 key 数 $($keysBefore.Count) → $($keysAfter.Count)（不得多出）" ($keysAfter.Count -eq $keysBefore.Count)

    # B2. 管理员不能给自己充值
    $balBefore = (Send-Probe 'GET' '/admin/api/me' $adminTok $null).Body | ConvertFrom-Json
    $b2 = Send-Probe 'PUT' "/admin/api/users/$adminId/balance" $adminTok '{"delta_cents":100000}'
    $balAfter = (Send-Probe 'GET' '/admin/api/me' $adminTok $null).Body | ConvertFrom-Json
    Check 'B2-status' "管理员给自己充值 → $($b2.Status) body=$($b2.Body)" ($b2.Status -eq 400)
    Check 'B2-message' "错误消息说明管理员不参与计费" ($b2.Body -like '*管理员*')
    Check 'B2-no-side-effect' "管理员余额 $($balBefore.balance_cents) → $($balAfter.balance_cents)（不得变化）" ($balAfter.balance_cents -eq $balBefore.balance_cents)

    # B3. 管理员不能被认领 key
    #     先让普通用户建一把 key，管理员再试图认领给自己。
    $uk = Send-Probe 'POST' '/admin/api/keys' $userTok '{"name":"user-probe-key"}'
    if ($uk.Status -ne 201) { throw "普通用户建 key 失败: $($uk.Status) $($uk.Body)" }
    $ukId = ($uk.Body | ConvertFrom-Json).id
    Check 'B3-precondition' "普通用户自助建 key → $($uk.Status)（这是管理员取得数据面凭据的唯一正当途径的反面）" ($uk.Status -eq 201)

    $b3 = Send-Probe 'PATCH' "/admin/api/keys/$ukId" $adminTok (@{ user_id = $adminId } | ConvertTo-Json -Compress)
    $after3 = (Send-Probe 'GET' '/admin/api/keys' $adminTok $null).Body | ConvertFrom-Json
    $owner3 = ($after3 | Where-Object { $_.id -eq $ukId }).user_id
    Check 'B3-status' "管理员把 key 认领给自己 → $($b3.Status) body=$($b3.Body)" ($b3.Status -eq 400)
    Check 'B3-message' "错误消息指向认领给普通用户" ($b3.Body -like '*普通用户*')
    Check 'B3-no-side-effect' "key 归属仍为 $owner3（不得变成管理员）" ($owner3 -eq $userId)

    # B4. 管理员不能调 /v1
    #     管理员无法通过任何 API 取得数据面 key（B1 拦住建、B3 拦住认领），
    #     所以「管理员带 key 调 /v1」在真实进程上**无法构造** —— 这本身就是
    #     最强的结论。这里验证普通用户的 key 照常可用（反向），
    #     以及 auth 层拦截已由进程内 TestAuthzAdminBoundary_CannotCallV1 直接覆盖。
    $v1user = Send-Probe 'POST' '/v1/chat/completions' $null $null
    Check 'B4-anon-v1' "匿名调 /v1 → $($v1user.Status)（无 key 必须 401）" ($v1user.Status -eq 401)

    # 普通用户 key 走真实数据面鉴权：必须**不是** invalid_api_key / admin_cannot_call_model。
    $reqV1 = New-Object System.Net.Http.HttpRequestMessage([System.Net.Http.HttpMethod]::new('POST'), "$base/v1/chat/completions")
    $reqV1.Headers.Add('Authorization', "Bearer $($uk.Body | ConvertFrom-Json | Select-Object -ExpandProperty plaintext_key)")
    $reqV1.Content = New-Object System.Net.Http.StringContent('{"model":"flash","messages":[{"role":"user","content":"hi"}]}', [Text.Encoding]::UTF8, 'application/json')
    $v1resp = $http.SendAsync($reqV1).GetAwaiter().GetResult()
    $v1text = $v1resp.Content.ReadAsStringAsync().GetAwaiter().GetResult()
    $v1code = [int]$v1resp.StatusCode
    $isAdminReject = $v1text -like '*admin_cannot_call_model*'
    $isInvalidKey = $v1text -like '*invalid_api_key*'
    Check 'B4-user-v1-auth-passes' "普通用户 key 通过数据面鉴权（status=$v1code，非 admin_cannot_call_model / 非 invalid_api_key）" ((-not $isAdminReject) -and (-not $isInvalidKey))
    Write-Host "         /v1 body=$($v1text.Substring(0, [Math]::Min(160, $v1text.Length)))"

    # =====================================================================
    Write-Host "`n--- A. 权限矩阵抽样（真实进程）---" -ForegroundColor Cyan
    # =====================================================================
    # 用分号分隔的简单字符串表，避免 PowerShell 对 `?` / `{}` 的解析歧义：
    # 每行格式 "id|身份|方法|路径|期望状态码"
    $matrix = @(
        'A-anon-me|anon|GET|/admin/api/me|401',
        'A-anon-keys|anon|GET|/admin/api/keys|401',
        'A-anon-users|anon|GET|/admin/api/users|401',
        'A-user-users|user|GET|/admin/api/users|403',
        'A-user-settings|user|GET|/admin/api/settings|403',
        'A-user-routes|user|GET|/admin/api/routes|403',
        'A-user-providers|user|GET|/admin/api/providers|403',
        'A-user-audit|user|GET|/admin/api/audit|403',
        'A-user-byuser|user|GET|/admin/api/usage/by-user?from=0|403',
        'A-user-usage|user|GET|/admin/api/usage?from=0|200',
        'A-user-prune|user|POST|/admin/api/usage/prune|403',
        'A-admin-users|admin|GET|/admin/api/users|200',
        'A-admin-byuser|admin|GET|/admin/api/usage/by-user?from=0|200'
    )
    foreach ($line in $matrix) {
        $f = $line.Split('|')
        $tok = switch ($f[1]) { 'anon' { $null } 'user' { $userTok } 'admin' { $adminTok } }
        $body = if ($f[2] -eq 'POST') { '{}' } else { $null }
        $r = Send-Probe $f[2] $f[3] $tok $body
        $ok = ($r.Status -eq [int]$f[4])
        $detail = "$($f[2]) $($f[3]) as $($f[1]) → $($r.Status) (期望 $($f[4]))"
        if (-not $ok) { $detail += " body=$($r.Body)" }
        Check $f[0] $detail $ok
    }

    # 普通用户对**自己的**余额端点也要 403（充值只能由管理员发起）。
    $userBal = Send-Probe 'PUT' "/admin/api/users/$userId/balance" $userTok '{"delta_cents":100}'
    Check 'A-user-self-balance' "普通用户尝试给自己充值 → $($userBal.Status) (期望 403)" ($userBal.Status -eq 403)

    # bootstrap 窗口在引导完成后必须关闭（普通用户/匿名都不能再设密码）。
    $bsAgain = Send-Probe 'POST' '/admin/api/bootstrap' $null '{"password":"Hijack123!"}'
    Check 'A-bootstrap-closed' "引导完成后再 POST /bootstrap → $($bsAgain.Status) (期望 409，窗口一次性)" ($bsAgain.Status -eq 409)

    # =====================================================================
    Write-Host "`n--- C. 作用域收窄：普通用户只看得到自己（真实进程）---" -ForegroundColor Cyan
    # =====================================================================
    # 给两个用户各造一条充值，确认普通用户看不到别人的金额。
    Send-Probe 'PUT' "/admin/api/users/$userId/balance" $adminTok '{"delta_cents":12345}' | Out-Null
    $mine = Send-Probe 'GET' '/admin/api/topups?limit=50' $userTok $null
    $mineJson = $mine.Body | ConvertFrom-Json
    $mineIds = @($mineJson.records | ForEach-Object { $_.user_id } | Sort-Object -Unique)
    Check 'C-topups-scoped' "普通用户 /topups 只含自己（user_ids=$($mineIds -join ','), total=$($mineJson.total)）" (($mineIds.Count -eq 1) -and ($mineIds[0] -eq $userId))

    $all = Send-Probe 'GET' '/admin/api/topups?limit=50' $adminTok $null
    $allJson = $all.Body | ConvertFrom-Json
    Check 'C-topups-admin-all' "管理员 /topups 看到全站（total=$($allJson.total)）" ($allJson.total -ge 1)

    # 越权尝试：普通用户显式指定 user_id 查询别人的流水 —— 必须被忽略。
    $peek = Send-Probe 'GET' "/admin/api/topups?user_id=$adminId&limit=50" $userTok $null
    $peekJson = $peek.Body | ConvertFrom-Json
    $peekIds = @($peekJson.records | ForEach-Object { $_.user_id } | Sort-Object -Unique)
    Check 'C-topups-userid-ignored' "普通用户传 ?user_id=管理员 时作用域仍是自己（user_ids=$($peekIds -join ',')）" (($peekIds.Count -eq 0) -or (($peekIds.Count -eq 1) -and ($peekIds[0] -eq $userId)))

    # =====================================================================
    Write-Host "`n--- D. 会话 ---" -ForegroundColor Cyan
    # =====================================================================
    $beforeLogout = Send-Probe 'GET' '/admin/api/me' $userTok $null
    Send-Probe 'POST' '/admin/api/logout' $userTok $null | Out-Null
    $afterLogout = Send-Probe 'GET' '/admin/api/me' $userTok $null
    Check 'D-logout-revokes' "登出前 /me=$($beforeLogout.Status)，登出后同一令牌 /me=$($afterLogout.Status)（服务端吊销）" (($beforeLogout.Status -eq 200) -and ($afterLogout.Status -eq 401))

    # =====================================================================
    Write-Host "`n--- E. 越权尝试的副作用检查（直接查库，只读）---" -ForegroundColor Cyan
    # =====================================================================
    # 用 dbdump（Go 写的只读助手）直接查 gateway.db，而不是靠 API 自述。
    # 本机没有 sqlite3 CLI（实测 Get-Command sqlite3 为空），而这条断言
    # **必须**查库：一个「先建了资源再回 403」的实现，API 侧状态码断言照样绿。
    if (Test-Path $dbdump) {
        $dump = Get-DbSnapshot
        Write-Host "  库快照: $((Get-DbDump) -join '  ')"
        Check 'E-db-keys-total' "access_keys 共 $($dump['access_keys_total']) 行（仅普通用户自建的那把）" ($dump['access_keys_total'] -eq '1')
        Check 'E-db-no-admin-key' "归属管理员的 key = $($dump['access_keys_owned_by_admin']) 行（B1 被拒后不得落库）" ($dump['access_keys_owned_by_admin'] -eq '0')
        Check 'E-db-no-ghost-key' "被拒的 admin-self-key-probe = $($dump['access_keys_named_probe']) 行（必须 0）" ($dump['access_keys_named_probe'] -eq '0')
        Check 'E-db-user-key-kept' "普通用户的 key = $($dump['access_keys_owned_by_user']) 行（正当途径未被误伤）" ($dump['access_keys_owned_by_user'] -eq '1')
        # 与 B 之前的基线逐值对比（而不是断言某个猜测值，见 B 组开头的说明）。
        Check 'E-db-admin-balance-unchanged' "管理员 balance_cents = $($dump['admin_balance_cents'])（基线 $($dbBaseline['admin_balance_cents'])：B2 被拒后未变）" ($dump['admin_balance_cents'] -eq $dbBaseline['admin_balance_cents'])
        Check 'E-db-no-admin-topup-row' "balance_topups 中属于管理员的流水 = $($dump['balance_topups_for_admin']) 行（B2 被拒后不得留痕）" ($dump['balance_topups_for_admin'] -eq '0')
        # 普通用户那次充值**必须**留下恰好一条流水（否则「没有管理员流水」可能只是
        # 流水表压根没在写 —— 那样 B2 的 no-side-effect 会因为错误的原因通过）。
        Check 'E-db-topup-count' "balance_topups 共 $($dump['balance_topups_total']) 行（仅普通用户那次充值）" ($dump['balance_topups_total'] -eq '1')
    } else {
        Write-Host "  [SKIP] dbdump 构建失败：E 组的直接查库断言跳过（API 侧 no-side-effect 断言仍已执行）" -ForegroundColor Yellow
    }

    # ---- 汇总 ---------------------------------------------------------------
    $failed = @($results | Where-Object { -not $_.Pass })
    Write-Host ""
    Write-Host "=== 汇总：$($results.Count) 条断言，$($results.Count - $failed.Count) 通过，$($failed.Count) 失败 ===" -ForegroundColor $(if ($failed.Count -eq 0) { 'Green' } else { 'Red' })
    if ($failed.Count -gt 0) {
        foreach ($f in $failed) { Write-Host "  FAIL $($f.Id): $($f.Detail)" -ForegroundColor Red }
        exit 1
    }
    exit 0
}
finally {
    & $cleanup
    Write-Host "`n已停止探针进程（pid=$($proc.Id)）。"
    # 清理构建产物与隔离状态，避免把 20MB 二进制和测试库留在工作区里。
    foreach ($f in @($exe, (Join-Path $PSScriptRoot 'dbdump.exe'))) {
        if (Test-Path $f) { Remove-Item $f -Force -ErrorAction SilentlyContinue }
    }
    if (Test-Path $probeDir) { Remove-Item $probeDir -Recurse -Force -ErrorAction SilentlyContinue }
    Write-Host "已清理构建产物与隔离状态目录 $probeDir"
}
