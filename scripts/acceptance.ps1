# P10.3 可重复本地交付检查。必须提供真实测试数据库，避免集成测试静默跳过。
# 外部模型/工具调用由各自的显式环境开关控制；本脚本不启用收费或有副作用的调用。
[CmdletBinding()]
param([switch]$SkipRace)
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
if (-not $env:CAGENT_TEST_MONGOD) {
    throw 'Set CAGENT_TEST_MONGOD before acceptance; isolated server failpoints are required.'
}
Push-Location $repo
try {
    # 保留原始 JSON 证据；逐项检查 skip，数据库测试未运行不能获得成功结论。
    $reportDirectory = Join-Path $repo '.local/acceptance'
    New-Item -ItemType Directory -Force -Path $reportDirectory | Out-Null
    $report = Join-Path $reportDirectory 'tests.jsonl'
    & go version
    if ($LASTEXITCODE -ne 0) { throw 'Go toolchain unavailable.' }
    & go test ./... -json -count=1 -timeout=240s | Set-Content -Encoding utf8 $report
    if ($LASTEXITCODE -ne 0) { throw "Tests failed; inspect $report" }
    $allowedSkips = @('TestRealOpenAISmoke', 'TestExternalToolSmoke')
    $skipped = @()
    foreach ($line in Get-Content $report) {
        $event = $line | ConvertFrom-Json
        # Go 对没有测试文件的纯接口包也发 package skip；它不是跳过行为测试。
        if ($event.Action -eq 'skip' -and $event.Test) {
            if ($event.Test -notin $allowedSkips) { throw "Unexpected skipped test: $($event.Package)/$($event.Test)" }
            $skipped += $event.Test
        }
    }
    # race 不受普通测试缓存影响；显式跳过时结果必须携带这一限制。
    if (-not $SkipRace) {
        & go test -race ./internal/observability ./internal/config ./internal/app ./internal/adapter/adk ./internal/adapter/local ./local-tools/read_skill ./pkg/localtool ./internal/adapter/mongodb ./internal/adapter/a2a ./internal/adapter/mcp ./internal/tool ./internal/transport/httpapi ./internal/bootstrap -count=1 -timeout=300s
        if ($LASTEXITCODE -ne 0) { throw 'Race checks failed.' }
    }
    & go vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet failed.' }
    & go build ./...
    if ($LASTEXITCODE -ne 0) { throw 'go build failed.' }
    Write-Host "Local acceptance passed. Report: $report"
    if ($SkipRace) { Write-Warning 'Race checks were explicitly skipped.' }
    if ($skipped.Count) { Write-Warning "External acceptance remains pending: $($skipped -join ', ')" }
    Write-Host 'Production topology, external task lifecycle and credential acceptance require the deployment checklist.'
} finally {
    Pop-Location
}
