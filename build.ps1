#Requires -Version 5.1
<#
build.ps1 — rosetta-gateway 纯编译脚本（只构建，不启动）

产物：bin\gateway.exe（已内嵌管理后台 SPA），与 gateway.ps1 默认路径一致，
     编译完直接 `.\gateway.ps1 -NoBuild` 即可运行。

用法：
  .\build.ps1                # 构建前端（npm build + sync）+ 编译 Go
  .\build.ps1 -NoWeb         # 跳过前端，用现有 internal\webui\dist 编译 Go
  .\build.ps1 -SkipInstall   # 已装过依赖时跳过 npm install
  .\build.ps1 -Typecheck     # 前端构建前先跑 vue-tsc --noEmit
  .\build.ps1 -Vet           # Go 编译前先跑 go vet ./...
  .\build.ps1 -Out ..\x\g.exe # 自定义产物路径（默认 bin\gateway.exe）
#>
param(
  [switch]$NoWeb,
  [switch]$SkipInstall,
  [switch]$Typecheck,
  [switch]$Vet,
  [string]$Out = ""
)

$ErrorActionPreference = 'Stop'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch {}

$Root      = $PSScriptRoot
$Bin       = if ($Out) { $Out } else { Join-Path $Root 'bin\gateway.exe' }
$WebDir    = Join-Path $Root 'web'
$EmbedDist = Join-Path $Root 'internal\webui\dist'

function Die([string]$msg) { Write-Host "[X] $msg" -ForegroundColor Red; exit 1 }

# 5.1 下 EAP=Stop 时，原生命令（npm/go/node）把正常输出写到 stderr 会被当成终止错误。
# 包裹一段执行：临时放宽 EAP，跑完按 $LASTEXITCODE 判定成败。
function Invoke-Native([scriptblock]$Block, [string]$failMsg) {
  $ErrorActionPreference = 'Continue'
  try {
    & $Block
    $code = $LASTEXITCODE
  } finally {
    $ErrorActionPreference = 'Stop'
  }
  if ($code -ne 0) { Die "$failMsg（exit $code）" }
}

# ---------- 前端构建（web/dist → internal\webui\dist，go:embed 数据源） ----------
if ($NoWeb) {
  if (-not (Test-Path $EmbedDist)) {
    Die "缺少前端产物 $EmbedDist，无法跳过前端构建。去掉 -NoWeb 让脚本代劳。"
  }
  Write-Host "[i] 跳过前端构建（用现有 internal\webui\dist）"
} else {
  if (-not (Test-Path (Join-Path $WebDir 'package.json'))) {
    Die "找不到 $WebDir\package.json，前端源码缺失。"
  }
  if (-not (Get-Command npm -ErrorAction SilentlyContinue)) {
    Die "未找到 npm。请安装 Node.js 并加入 PATH，或用 -NoWeb 只编译 Go。"
  }

  Push-Location $WebDir
  try {
    if (-not $SkipInstall -or -not (Test-Path (Join-Path $WebDir 'node_modules'))) {
      Write-Host "-> 安装前端依赖（npmmirror）..."
      Invoke-Native { npm install --registry=https://registry.npmmirror.com --no-audit --no-fund } "npm install 失败"
    } else {
      Write-Host "[i] 跳过 npm install（node_modules 已存在）"
    }

    if ($Typecheck) {
      Write-Host "-> 类型检查（vue-tsc）..."
      Invoke-Native { npm run typecheck } "类型检查失败"
    }

    Write-Host "-> 构建管理界面（vite build）..."
    Invoke-Native { npm run build } "前端构建失败"

    Write-Host "-> 同步前端产物到 go:embed 目录..."
    Invoke-Native { node sync-embed.mjs } "sync-embed 失败"
  } finally {
    Pop-Location
  }
  Write-Host "[OK] 前端产物已同步：$EmbedDist" -ForegroundColor Green
}

# ---------- 编译 Go ----------
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  Die "未找到 go。请安装 Go 并加入 PATH。"
}

New-Item -ItemType Directory -Force -Path (Split-Path $Bin) | Out-Null

if ($Vet) {
  Write-Host "-> 静态检查（go vet ./...）..."
  Invoke-Native { go vet ./... } "go vet 失败"
}

Write-Host "-> 编译中（go build ./cmd/gateway）..."
Push-Location $Root
try {
  Invoke-Native { go build -trimpath -o $Bin './cmd/gateway' } "编译失败"
} finally {
  Pop-Location
}

$sizeMb = [math]::Round((Get-Item $Bin).Length / 1MB, 1)
Write-Host ""
Write-Host "[OK] 编译完成：$Bin（$sizeMb MB）" -ForegroundColor Green
Write-Host "  直接运行：.\bin\gateway.exe          # 无需参数，配置自动落在 bin\config.json" -ForegroundColor DarkGray
Write-Host "  首次进入：http://127.0.0.1:8080/admin/" -ForegroundColor DarkGray
Write-Host "  或走脚本：.\gateway.ps1 -NoBuild     # 前台（注入密钥/落日志）；加 -Detached 挂后台" -ForegroundColor DarkGray
