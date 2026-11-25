param(
    [Parameter(Mandatory)][ValidatePattern('^v[0-9]+[.][0-9]+[.][0-9]+$')][string]$Version
)

$ErrorActionPreference = 'Stop'
$project = Split-Path $PSScriptRoot -Parent
Push-Location -LiteralPath $project
$oldOS = $env:GOOS
$oldArch = $env:GOARCH
$oldCGO = $env:CGO_ENABLED
try {
    $commit = git rev-parse HEAD
    if ($LASTEXITCODE -ne 0 -or $commit -notmatch '^[a-f0-9]{40}$') { throw 'A valid release commit is required' }
    $epoch = git show -s --format=%ct $commit
    if ($LASTEXITCODE -ne 0 -or $epoch -notmatch '^[0-9]+$') { throw 'A valid release timestamp is required' }
    $changes = git status --porcelain
    if ($LASTEXITCODE -ne 0 -or $changes) { throw 'Release archives require a clean worktree' }
    $toolchain = go version
    if ($LASTEXITCODE -ne 0) { throw 'Cannot detect the build toolchain' }
    $destination = (New-Item -ItemType Directory -Path (Join-Path $project ".local/release-$Version") -Force).FullName
    $stage = (New-Item -ItemType Directory -Path (Join-Path $destination ('stage-' + [guid]::NewGuid().ToString('N')))).FullName
    $archiveFiles = @()
    foreach ($os in 'linux', 'windows') {
        $target = "upgrade-agent-$Version-$os-amd64"
        $package = (New-Item -ItemType Directory -Path (Join-Path $stage $target)).FullName
        $env:GOOS = $os
        $env:GOARCH = 'amd64'
        $env:CGO_ENABLED = '0'
        $filename = if ($os -eq 'windows') { 'upgrade-agent.exe' } else { 'upgrade-agent' }
        $executable = Join-Path $package $filename
        go build -trimpath -buildvcs=false -ldflags "-s -w -buildid= -X main.version=$Version -X main.revision=$commit" -o $executable ./cmd/upgrade-agent
        if ($LASTEXITCODE -ne 0) { throw "Could not build $target" }
        foreach ($name in 'README.md', 'LICENSE', 'NOTICE', 'THIRD_PARTY.md', 'licenses', 'docs', 'examples', 'deploy') {
            Copy-Item -LiteralPath (Join-Path $project $name) -Destination $package -Recurse -Force
        }
        @{
            version = $Version; commit = $commit; platform = "$os/amd64"; toolchain = $toolchain
            binary = $filename; sha256 = (Get-FileHash -LiteralPath $executable -Algorithm SHA256).Hash.ToLowerInvariant()
        } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $package 'BUILD.json') -Encoding utf8
        if ($os -eq 'windows') {
            $archive = Join-Path $destination "$target.zip"
            Compress-Archive -LiteralPath $package -DestinationPath $archive -Force
        } else {
            $archive = Join-Path $destination "$target.tar.gz"
            python (Join-Path $PSScriptRoot 'linux-archive.py') $package $archive $epoch
            if ($LASTEXITCODE -ne 0) { throw "Could not archive $target" }
        }
        $archiveFiles += $archive
    }
    $checksums = foreach ($archive in $archiveFiles) {
        $digest = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
        "$digest  $(Split-Path -Leaf $archive)"
    }
    $checksums | Set-Content -LiteralPath (Join-Path $destination 'SHA256SUMS') -Encoding ascii
    Get-ChildItem -LiteralPath $destination -File | Select-Object Name, Length
} finally {
    [Environment]::SetEnvironmentVariable('GOOS', $oldOS, 'Process')
    [Environment]::SetEnvironmentVariable('GOARCH', $oldArch, 'Process')
    [Environment]::SetEnvironmentVariable('CGO_ENABLED', $oldCGO, 'Process')
    Pop-Location
}
