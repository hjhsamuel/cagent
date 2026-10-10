[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
Push-Location $repo
try {
    $toolTargetOS = & go env GOOS
    if ($LASTEXITCODE -ne 0) { throw 'Go toolchain unavailable.' }
    $suffix = if ($toolTargetOS -eq 'windows') { '.exe' } else { '' }
    foreach ($name in @('echo', 'read_skill')) {
        $bin = Join-Path $repo "local-tools/$name/bin"
        New-Item -ItemType Directory -Force -Path $bin | Out-Null
        & go build -buildvcs=false -o (Join-Path $bin "$name$suffix") "./local-tools/$name"
        if ($LASTEXITCODE -ne 0) { throw "Failed to build local tool: $name" }
    }
} finally {
    Pop-Location
}
