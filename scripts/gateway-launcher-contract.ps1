param(
  [Parameter(Mandatory = $true)]
  [string]$LauncherPath
)

$ErrorActionPreference = 'Stop'
. (Resolve-Path -LiteralPath $LauncherPath)

$expected = 'C:\project\bin\gateway.exe'
$result = [ordered]@{
  exactBinary   = [RosettaGatewayLauncher.ProcessOwnership]::IsGatewayExecutable(
    'C:\PROJECT\BIN\GATEWAY.EXE', $expected)
  differentPath = [RosettaGatewayLauncher.ProcessOwnership]::IsGatewayExecutable(
    'C:\other\gateway.exe', $expected)
  wrapper       = [RosettaGatewayLauncher.ProcessOwnership]::IsLauncherWrapper(
    'cmd.exe /c ""C:\project\bin\gateway.exe" > log 2> err"', $expected)
  unrelated     = [RosettaGatewayLauncher.ProcessOwnership]::IsLauncherWrapper(
    'C:\other\worker.exe --port 8666', $expected)
}
$result | ConvertTo-Json -Compress
