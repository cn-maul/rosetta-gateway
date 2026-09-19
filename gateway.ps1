#Requires -Version 5.1
<#
gateway.ps1 — rosetta-gateway 一键脚本：编译 + 启动 + 停止

用法：
  .\gateway.ps1                  # 构建前端+编译，前台运行（默认），实时日志，Ctrl+C 停止
  .\gateway.ps1 -Detached        # 构建并后台启动：独立隐藏控制台，关终端不影响，日志落文件
  .\gateway.ps1 -NoBuild         # 跳过前端构建与 Go 编译，直接用 bin\gateway.exe 启动
  .\gateway.ps1 -NoWeb           # 跳过前端构建（仍编译 Go），用现有 internal\webui\dist
  .\gateway.ps1 -Stop            # 停止后台网关（按镜像名+路径精确匹配，端口兜底）

密钥来源（优先级：命令行参数 > 环境变量 > bin\master.key > 自动生成并保存）：
  -MasterKey     网关主密钥。缺省时自动生成并保存到 bin\master.key，后续启动自动复用
                 （库里凭据用建库时的主密钥加密，key 变了会解密失败——所以必须固定）
  -DeepseekKey   DEEPSEEK_API_KEY。缺省留空：网关能启动，转发请求会报上游鉴权失败
  -AnthropicKey  ANTHROPIC_API_KEY。同上

配置文件：
  网关不再接受任何命令行参数。配置固定在【可执行文件同级】(bin\config.json)，
  首次启动若该文件不存在，网关会自动生成一份默认配置（空 provider/route，全部在前端添加）。
  配置里的相对路径（如 db_path）按可执行文件所在目录解析，与当前工作目录无关——
  保证无论从哪个目录启动，都命中同一份配置和同一个数据库。
#>
param(
  [string]$MasterKey = "",
  [string]$DeepseekKey = "",
  [string]$AnthropicKey = "",
  [switch]$NoBuild,
  [switch]$NoWeb,
  [switch]$Detached,
  [switch]$Stop
)

$ErrorActionPreference = 'Stop'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch {}

$Root    = $PSScriptRoot
$Bin     = Join-Path $Root 'bin\gateway.exe'
$PidFile = Join-Path $Root 'bin\gateway.pid'
$LogDir  = Join-Path $Root 'bin\logs'

function Die([string]$msg) { Write-Host "[X] $msg" -ForegroundColor Red; exit 1 }

# ---------- 配置：由网关自身管理，位于可执行文件同级（bin\config.json） ----------
# 网关不接受命令行参数；首次启动若无配置会自动生成默认文件。
# 这里只为端口探测 / 停止旧进程读取该配置，缺失时按默认 8080 处理即可。
$ConfigPath = Join-Path (Split-Path $Bin) 'config.json'

# 从 listen 字段提取端口（"127.0.0.1:8080" -> 8080），解析失败按默认 8080
$Port = 8080
if (Test-Path $ConfigPath) {
  try {
    $cfg = Get-Content $ConfigPath -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($cfg.listen) { $Port = [int]($cfg.listen -replace '^.*:', '') }
  } catch { Write-Host "[i] 配置解析失败，端口按默认 $Port 处理" -ForegroundColor Yellow }
}

# ---------- 找到占用端口的进程 ----------
function Get-ListenerPid([int]$P) {
  try {
    $c = Get-NetTCPConnection -LocalPort $P -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($c) { return [int]$c.OwningProcess }
  } catch { }
  # 兜底：老系统没有 Get-NetTCPConnection 时解析 netstat
  $line = netstat -ano | Select-String (':{0}\s+.+LISTENING' -f $P) | Select-Object -First 1
  if ($line) { return [int](($line.ToString().Trim() -split '\s+')[-1]) }
  return $null
}

# ---------- -Stop：停止网关 ----------
if ($Stop) {
  $stopped = $false
  # 主手段：按镜像名+路径精确匹配本项目的 gateway.exe（后台模式是 cmd 的子进程，
  # 只杀 cmd 包装层杀不死网关，必须直接找 gateway 本体）
  Get-Process -Name 'gateway' -ErrorAction SilentlyContinue |
    Where-Object { $_.Path -eq $Bin } |
    ForEach-Object {
      Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue
      Write-Host "[OK] 已停止网关（PID $($_.Id)）" -ForegroundColor Green
      $stopped = $true
    }
  # 清理 cmd 包装层与 pid 文件
  if (Test-Path $PidFile) {
    $oldPid = (Get-Content $PidFile -ErrorAction SilentlyContinue) -join ''
    if ($oldPid -match '^\d+$') {
      Stop-Process -Id ([int]$oldPid) -Force -ErrorAction SilentlyContinue
    }
    Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
  }
  # 双保险：按端口兜底（覆盖改名/复制出去的二进制）
  $lp = Get-ListenerPid $Port
  if ($lp -and $lp -ne $PID) {
    Stop-Process -Id $lp -Force -ErrorAction SilentlyContinue
    Write-Host "[OK] 已停止占用端口 $Port 的进程（PID $lp）" -ForegroundColor Green
    $stopped = $true
  }
  if (-not $stopped) { Write-Host "[i] 没有发现正在运行的网关" }
  exit 0
}

# ---------- 前端构建（web/ → internal\webui\dist，go:embed 数据源） ----------
$WebDir    = Join-Path $Root 'web'
$EmbedDist = Join-Path $Root 'internal\webui\dist'

if ($NoWeb -or $NoBuild -or -not (Test-Path (Join-Path $WebDir 'package.json'))) {
  if (-not (Test-Path $EmbedDist)) {
    Die "缺少前端产物 $EmbedDist。请先在 web\ 目录执行 npm install && npm run build && npm run sync，或去掉 -NoBuild/-NoWeb 让脚本代劳。"
  }
  if (-not $NoBuild -and (Test-Path (Join-Path $WebDir 'package.json'))) {
    Write-Host "[i] 跳过前端构建（用现有 internal\webui\dist）"
  }
} else {
  $npmCmd = Get-Command npm -ErrorAction SilentlyContinue
  if (-not $npmCmd) { Die "未找到 npm。请安装 Node.js 并加入 PATH，或加 -NoWeb 跳过前端构建。" }
  if (-not (Test-Path (Join-Path $WebDir 'node_modules'))) {
    Write-Host "-> 安装前端依赖（npmmirror）..."
    $ErrorActionPreference = 'Continue'
    Push-Location $WebDir
    & npm install --registry=https://registry.npmmirror.com --no-audit --no-fund
    $npmExit = $LASTEXITCODE
    Pop-Location
    $ErrorActionPreference = 'Stop'
    if ($npmExit -ne 0) { Die "npm install 失败（exit $npmExit）" }
  }
  Write-Host "-> 构建管理界面（vite build）..."
  $ErrorActionPreference = 'Continue'
  Push-Location $WebDir
  & npm run build
  $npmExit = $LASTEXITCODE
  if ($npmExit -eq 0) {
    & node sync-embed.mjs
    $npmExit = $LASTEXITCODE
  }
  Pop-Location
  $ErrorActionPreference = 'Stop'
  if ($npmExit -ne 0) { Die "前端构建失败（exit $npmExit）" }
  Write-Host "[OK] 前端产物已同步：$EmbedDist" -ForegroundColor Green
}

# ---------- 编译 ----------
if (-not $NoBuild) {
  if (-not (Get-Command go -ErrorAction SilentlyContinue)) { Die "未找到 go。请安装 Go 并加入 PATH，或加 -NoBuild 跳过编译。" }
  New-Item -ItemType Directory -Force -Path (Join-Path $Root 'bin') | Out-Null
  Write-Host "-> 编译中（go build ./cmd/gateway）..."
  # 注意：5.1 下 EAP=Stop 时原生命令写 stderr 会变成终止错误，编译段临时放宽
  $ErrorActionPreference = 'Continue'
  & go build -o $Bin './cmd/gateway'
  $buildExit = $LASTEXITCODE
  $ErrorActionPreference = 'Stop'
  if ($buildExit -ne 0) { Die "编译失败（exit $buildExit）" }
  Write-Host "[OK] 编译完成：$Bin" -ForegroundColor Green
}
if (-not (Test-Path $Bin)) { Die "二进制不存在：$Bin（去掉 -NoBuild 先编译一次）" }

# ---------- 密钥 ----------
# 主密钥优先级：参数 > 环境变量 > bin\master.key（本地密钥文件）> 自动生成并保存。
# 落盘复用很关键：库里凭据是用建库时的主密钥加密的，每次换 key 都会解密失败。
$KeyFile = Join-Path $Root 'bin\master.key'
if (-not $MasterKey)    { $MasterKey    = $env:ROSETTA_GW_MASTER_KEY }
if (-not $DeepseekKey)  { $DeepseekKey  = $env:DEEPSEEK_API_KEY }
if (-not $AnthropicKey) { $AnthropicKey = $env:ANTHROPIC_API_KEY }

if (-not $MasterKey) {
  if (Test-Path $KeyFile) {
    $MasterKey = (Get-Content $KeyFile -Raw -ErrorAction SilentlyContinue) -join ''
    $MasterKey = $MasterKey.Trim()
  }
  if (-not $MasterKey) {
    $MasterKey = -join ((1..32) | ForEach-Object { '{0:x2}' -f (Get-Random -Maximum 256) })
    New-Item -ItemType Directory -Force -Path (Join-Path $Root 'bin') | Out-Null
    Set-Content -Path $KeyFile -Value $MasterKey -Encoding ASCII
    Write-Host "[i] 主密钥已自动生成并保存到 $KeyFile（后续启动自动复用）" -ForegroundColor Yellow
    Write-Host "    显式传 -MasterKey 或设 ROSETTA_GW_MASTER_KEY 可覆盖" -ForegroundColor Yellow
  }
}
if (-not $DeepseekKey -or -not $AnthropicKey) {
  Write-Host "[i] 上游 API Key 不齐：网关可启动，但转发请求会报上游鉴权失败。" -ForegroundColor Yellow
}
# 子进程靠继承环境变量拿到密钥（Start-Process 无跨版本通用的 -Environment）
$env:ROSETTA_GW_MASTER_KEY = $MasterKey
$env:DEEPSEEK_API_KEY      = $DeepseekKey
$env:ANTHROPIC_API_KEY     = $AnthropicKey

# ---------- 启动前：清掉占用端口的旧进程（避免"改了代码却连到旧二进制"） ----------
$lp = Get-ListenerPid $Port
if ($lp -and $lp -ne $PID) {
  Write-Host "-> 端口 $Port 被占用（PID $lp），先停止旧进程"
  Stop-Process -Id $lp -Force -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 800
}

# ---------- 启动 ----------
New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
$outLog = Join-Path $LogDir 'gateway.out.log'
$errLog = Join-Path $LogDir 'gateway.err.log'

if (-not $Detached) {
  # 前台（默认）：像 dev server 一样占住终端，实时日志，Ctrl+C 停止。
  # 注意 5.1 下 EAP=Stop 时原生命令写 stderr 会变成终止错误，执行段临时放宽。
  Write-Host "-> 启动 http://127.0.0.1:$Port/admin/（前台运行，Ctrl+C 停止；挂后台用 -Detached）"
  Write-Host ""
  $ErrorActionPreference = 'Continue'
  & $Bin
  $gwExit = $LASTEXITCODE
  $ErrorActionPreference = 'Stop'
  Write-Host ""
  Write-Host "[i] 网关已退出（exit $gwExit）" -ForegroundColor DarkGray
  exit $gwExit
}

# 后台模式（-Detached）：用 cmd /c 在【隐藏的新控制台】里拉起网关并重定向日志。
# 为什么不用 Start-Process -NoNewWindow：那样网关挂在当前控制台上，
# 启动它的终端一关，网关跟着被杀（冒烟测试踩过）。
# cmd 包装层给网关一个独立的隐藏控制台：终端关闭不影响运行，日志照常落文件。
# 代价：cmd 会等子进程退出（/c 语义），所以 pid 文件记的是 cmd 的 PID，
# 网关本体由 -Stop 按"镜像名 gateway + 路径匹配"来找。
$cmdLine = '/c ""' + $Bin + '"' +
           ' > "' + $outLog + '" 2> "' + $errLog + '""'
$proc = Start-Process -FilePath $env:ComSpec -ArgumentList $cmdLine `
  -WorkingDirectory $Root -WindowStyle Hidden -PassThru
Set-Content -Path $PidFile -Value $proc.Id

Start-Sleep -Seconds 2
if ($proc.HasExited) {
  Die "网关启动后立即退出。错误日志：$errLog"
}

# ---------- 健康检查 ----------
$gwPid = Get-ListenerPid $Port
$health = '[?] 未知'
try {
  $null = Invoke-WebRequest -Uri "http://127.0.0.1:$Port/admin/" -UseBasicParsing -TimeoutSec 5
  $health = '[OK] 健康检查通过'
} catch {
  $health = '[!] 健康检查未通过：' + $_.Exception.Message
}

Write-Host ""
Write-Host "[OK] 网关已启动（后台运行）" -ForegroundColor Green
if ($gwPid) { Write-Host "  网关 PID:   $gwPid" }
Write-Host "  管理界面:   http://127.0.0.1:$Port/admin/"
Write-Host "  配置:       $ConfigPath"
Write-Host "  日志:       $outLog / $errLog"
Write-Host "  停止:       .\gateway.ps1 -Stop"
Write-Host $health

# 凭据解密失败 = 当前主密钥与建库时不一致，转发必然 502 —— 明说，别让人去猜
Start-Sleep -Milliseconds 300
$logTail = (Get-Content $outLog -Tail 60 -ErrorAction SilentlyContinue) -join "`n"
if ($logTail -match 'failed to decrypt credential') {
  Write-Host ""
  Write-Host "[!] 数据库凭据解密失败：当前主密钥与首次建库时不一致，转发请求会报上游鉴权失败。" -ForegroundColor Yellow
  Write-Host "    修复：停掉网关后删除数据库文件（配置里的 db_path），重启时带上真实上游 Key 重新 bootstrap。" -ForegroundColor Yellow
}

# 把"为什么脚本直接退出了"说在明面上——这是后台模式的设计，不是故障
Write-Host ""
Write-Host "[i] 脚本到此正常退出属预期：网关已在后台独立运行，关闭本终端也不影响。" -ForegroundColor DarkGray
Write-Host    "    实时跟踪日志：Get-Content '$outLog' -Wait" -ForegroundColor DarkGray
