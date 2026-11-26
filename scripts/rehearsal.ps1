param(
    [Parameter(Mandatory)][string]$PreviousCLI,
    [Parameter(Mandatory)][string]$EvidenceDirectory,
    [Parameter(Mandatory)][string]$FixtureDirectory,
    [Parameter(Mandatory)][string]$MySQLBin,
    [Parameter(Mandatory)][int]$Port,
    [Parameter(Mandatory)][string]$MySQLVersion
)

$ErrorActionPreference = 'Stop'
if (!$env:MYSQL_PWD -or $env:UPGRADE_TEST_MYSQL_DSN -cne "test_upgrade:$($env:MYSQL_PWD)@tcp(127.0.0.1:$Port)/upgrade_test") {
    throw 'Run the rehearsal through scripts/test-mysql.ps1'
}
$project = Split-Path $PSScriptRoot -Parent
$sample = Join-Path $project 'examples/legacy-pricing'
$current = Join-Path $FixtureDirectory 'upgrade-agent-current.exe'
$dumpFile = Join-Path $FixtureDirectory 'workspace.sql'
$client = @('--no-defaults', '--protocol=TCP', '--host=127.0.0.1', "--port=$Port", '--user=test_upgrade')
$mysql = Join-Path $MySQLBin 'mysql.exe'
$dsnEnv = [Environment]::GetEnvironmentVariable('UPGRADE_MYSQL_DSN', 'Process')
$servers = [Collections.Generic.List[Diagnostics.Process]]::new()

function Use-Schema([string]$Schema) {
    $env:UPGRADE_MYSQL_DSN = "test_upgrade:$($env:MYSQL_PWD)@tcp(127.0.0.1:$Port)/$Schema"
}

function Invoke-CLI([string]$Executable, [string[]]$Arguments) {
    $lines = & $Executable @Arguments
    if ($LASTEXITCODE -ne 0) { throw "CLI command failed: $($Arguments[0])" }
    $lines -join "`n"
}

function Start-Workspace([string]$Executable, [string]$Stage) {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $listener.Start()
    try { $localPort = $listener.LocalEndpoint.Port } finally { $listener.Stop() }
    $hostAddress = "127.0.0.1:$localPort"
    $workspaceServer = Start-Process -FilePath $Executable -ArgumentList @('serve', '-listen', $hostAddress) `
        -PassThru -WindowStyle Hidden -RedirectStandardOutput (Join-Path $FixtureDirectory "$Stage.out.log") `
        -RedirectStandardError (Join-Path $FixtureDirectory "$Stage.error.log")
    $servers.Add($workspaceServer)
    $origin = "http://$hostAddress"
    for ($attempt = 0; $attempt -lt 40; $attempt++) {
        if ($workspaceServer.HasExited) { break }
        try {
            if ((Invoke-RestMethod "$origin/healthz" -TimeoutSec 1).state -eq 'ready') {
                return @{ Origin = $origin; Process = $workspaceServer }
            }
        } catch {}
        Start-Sleep -Milliseconds 100
    }
    throw "$Stage workspace did not become ready"
}

function Stop-Workspace([Diagnostics.Process]$Process) {
    if (!$Process.HasExited) { Stop-Process -InputObject $Process }
    if (!$Process.WaitForExit(5000)) { throw 'Rehearsal workspace did not stop' }
}

function Invoke-Workspace($Origin, $Token, $Path, $Method = 'GET', $Key = '', $Data = $null, $ExpectedStatus = 200) {
    $headers = @{ Authorization = "Bearer $Token" }
    $options = @{Uri = "$Origin/api/v1/$Path"; Headers = $headers; Method = $Method; TimeoutSec = 20; SkipHttpErrorCheck = $true}
    if ($Method -eq 'POST') {
        $headers['Idempotency-Key'] = $Key
        $options['ContentType'] = 'application/json'
        $options['Body'] = ConvertTo-Json -InputObject $Data -Depth 80 -Compress
    }
    $response = Invoke-WebRequest @options
    if ([int]$response.StatusCode -ne $ExpectedStatus) {
        throw "$Method $Path returned HTTP $($response.StatusCode), expected $ExpectedStatus"
    }
    if ($response.Headers['Content-Type'] -match 'text/x-diff') {
        return [Text.Encoding]::UTF8.GetString($response.RawContentStream.ToArray())
    }
    return $response.Content | ConvertFrom-Json -Depth 90 -AsHashtable -NoEnumerate
}

function Get-TextDigest([string]$Text) {
    [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($Text))).ToLowerInvariant()
}

function Get-WorkspaceEvidence($Server) {
    $origin = $Server.Origin
    $report = Invoke-Workspace $origin $maintainer "validations/$($validation.artifact)"
    $patch = Invoke-Workspace $origin $maintainer "candidates/$($sale.artifact)/patch"
    $candidate = Invoke-Workspace $origin $maintainer "candidates/$($sale.artifact)"
    $plan = Invoke-Workspace $origin $maintainer "plans/$($created.sha256)"
    $workspace = Invoke-Workspace $origin $reviewer 'workspace'
    $job = Invoke-Workspace $origin $maintainer 'jobs/legacy-validate-currency'
    return [ordered]@{
        baseline = $workspace.baseline; revision = $workspace.revision; plan = $plan.sha256
        candidate = $candidate.sha256; candidate_patch_sha256 = Get-TextDigest $patch
        validation = $report.sha256; validation_state = $report.state; validation_job = $job.request_id
        validation_payload_sha256 = Get-TextDigest (ConvertTo-Json -InputObject $report -Depth 90 -Compress)
    }
}

try {
    go build -trimpath -o $current ./cmd/upgrade-agent
    if ($LASTEXITCODE -ne 0) { throw 'Current build failed' }
    $previousVersion = Invoke-CLI $PreviousCLI @('version')
    $currentVersion = Invoke-CLI $current @('version')
    Use-Schema 'upgrade_rehearsal'
    $null = Invoke-CLI $PreviousCLI @('migrate')
    $schemaBefore = & $mysql @client --database=upgrade_rehearsal --batch --skip-column-names '--execute=SELECT version FROM schema_migrations'
    if ($LASTEXITCODE -ne 0 -or $schemaBefore -ne '2') { throw 'Expected the v1.0.0 schema' }
    $scenario = @('-workspace', 'pricing', '-actor', 'fixture-reviewer', '-ttl', '1h')
    $maintainer = (Invoke-CLI $PreviousCLI (@('provision', '-tenant', 'rehearsal', '-role', 'maintainer', '-workspace', 'pricing', '-actor', 'fixture-maintainer', '-ttl', '1h')) | ConvertFrom-Json).token
    $reviewer = (Invoke-CLI $PreviousCLI (@('provision', '-tenant', 'rehearsal', '-role', 'reviewer') + $scenario) | ConvertFrom-Json).token
    $outsider = (Invoke-CLI $PreviousCLI (@('provision', '-tenant', 'isolated', '-role', 'reviewer') + $scenario) | ConvertFrom-Json).token
    $old = Start-Workspace $PreviousCLI 'previous'
    $capture = Invoke-CLI $PreviousCLI @('capture', '-root', $sample, '-ref', 'pricing-v1') | ConvertFrom-Json -Depth 90 -AsHashtable
    $pinned = Invoke-Workspace $old.Origin $maintainer 'snapshots' 'POST' 'legacy-snapshot' $capture
    $options = Get-Content (Join-Path $sample 'plan-options.json') -Raw | ConvertFrom-Json -Depth 80 -AsHashtable
    $created = Invoke-Workspace $old.Origin $maintainer 'plans' 'POST' 'legacy-plan' @{baseline = $pinned.baseline; options = $options}
    $sale = Invoke-Workspace $old.Origin $maintainer "plans/$($created.sha256)/batches/rule-sale-price/candidates" 'POST' 'legacy-sale' @{source = 'rule'}
    $currency = Invoke-Workspace $old.Origin $maintainer "plans/$($created.sha256)/batches/rule-currency-display/candidates" 'POST' 'legacy-currency' @{source = 'rule'}
    $validation = Invoke-Workspace $old.Origin $maintainer "candidates/$($currency.artifact)/validations" 'POST' 'legacy-validate-currency' @{}
    $before = Get-WorkspaceEvidence $old
    if ($before.validation_state -ne 'blocked' -or $before.validation_job -ne 'legacy-validate-currency' -or
        $before.candidate -cne $sale.artifact -or $before.plan -cne $created.sha256) {
        throw 'The previous version did not persist all expected records'
    }
    Stop-Workspace $old.Process

    $null = Invoke-CLI $current @('migrate')
    $schemaAfter = & $mysql @client --database=upgrade_rehearsal --batch --skip-column-names '--execute=SELECT version FROM schema_migrations'
    if ($LASTEXITCODE -ne 0 -or $schemaAfter -ne '4') { throw 'Expected the migrated schema' }
    $upgraded = Start-Workspace $current 'upgraded'
    $after = Get-WorkspaceEvidence $upgraded
    if ((ConvertTo-Json $before -Compress) -cne (ConvertTo-Json $after -Compress)) { throw 'Upgrade changed existing workbench evidence' }
    $plans = Invoke-Workspace $upgraded.Origin $reviewer 'plans?package=upgrade-lab%2Fframework&version=%5E2.0'
    if ($plans.items.Count -ne 1 -or $plans.items[0].sha256 -cne $created.sha256) {
        throw 'Upgrade did not index the old plan target'
    }

    $restoreInput = @{revision = $after.revision; items = @(@{
        baseline = $after.baseline; candidate = $currency.artifact; action = 'reject'; reason = 'Restore the original review selection'
    })}
    $pending = Invoke-Workspace $upgraded.Origin $reviewer 'review-batches' 'POST' 'restore-pending' $restoreInput
    if ($pending.completed -ne 0 -or $pending.items.Count -ne 1) { throw 'Expected a saved pending batch' }
    Stop-Workspace $upgraded.Process

    $dump = Join-Path $MySQLBin 'mysqldump.exe'
    & $dump @client --single-transaction --hex-blob --no-tablespaces "--result-file=$dumpFile" upgrade_rehearsal
    if ($LASTEXITCODE -ne 0) { throw 'Failed to back up the rehearsal schema' }
    $dumpDigest = (Get-FileHash -LiteralPath $dumpFile -Algorithm SHA256).Hash.ToLowerInvariant()
    $import = Start-Process -FilePath $mysql -ArgumentList ($client + '--database=upgrade_restore') `
        -RedirectStandardInput $dumpFile -PassThru -Wait -WindowStyle Hidden
    if ($import.ExitCode -ne 0) { throw 'Failed to restore the workspace to the isolated schema' }
    Use-Schema 'upgrade_restore'
    $restored = Start-Workspace $current 'restored'
    $recovered = Get-WorkspaceEvidence $restored
    if ((ConvertTo-Json $before -Compress) -cne (ConvertTo-Json $recovered -Compress)) { throw 'Backup restore changed upstream evidence' }
    $recoveredBatch = Invoke-Workspace $restored.Origin $reviewer 'review-batches/restore-pending'
    if ((ConvertTo-Json $pending -Depth 20 -Compress) -cne (ConvertTo-Json $recoveredBatch -Depth 20 -Compress)) {
        throw 'Backup restore changed the original ordered review selection'
    }
    $null = Invoke-Workspace $restored.Origin $outsider 'review-batches/restore-pending' 'GET' '' $null 404
    $null = Invoke-Workspace $restored.Origin $outsider "candidates/$($sale.artifact)/patch" 'GET' '' $null 404
    $null = Invoke-Workspace $restored.Origin $outsider "plans/$($created.sha256)/exports" 'POST' 'outside-export' @{baseline = $after.baseline} 404
    $resumed = Invoke-Workspace $restored.Origin $reviewer 'review-batches/restore-pending/run' 'POST' 'restore-run' @{}
    $stillPending = & $mysql @client --database=upgrade_rehearsal --batch --skip-column-names `
        "--execute=SELECT JSON_UNQUOTE(JSON_EXTRACT(payload,'$.completed')) FROM review_batches WHERE request_id='restore-pending'"
    if ($LASTEXITCODE -ne 0 -or $stillPending -ne '0' -or $resumed.completed -ne 1 -or $resumed.items[0].state -ne 'applied') {
        throw 'Restored execution must change only the restored schema'
    }

    $proposedFile = Join-Path $FixtureDirectory 'proposed.json'
    $competingFile = Join-Path $FixtureDirectory 'competing.json'
    Invoke-CLI $PreviousCLI @('capture', '-root', $sample, '-ref', 'pricing-without-lock', '-allow', 'composer.json,app,tests') |
        Set-Content -LiteralPath $proposedFile -Encoding utf8
    Invoke-CLI $PreviousCLI @('capture', '-root', $sample, '-ref', 'pricing-another-maintainer') |
        Set-Content -LiteralPath $competingFile -Encoding utf8
    $browserSettings = @{
        UPGRADE_REHEARSAL_ORIGIN = $restored.Origin; UPGRADE_REHEARSAL_REVIEWER = $reviewer;
        UPGRADE_REHEARSAL_MAINTAINER = $maintainer; UPGRADE_REHEARSAL_PLAN = $created.sha256;
        UPGRADE_REHEARSAL_PROPOSED = $proposedFile; UPGRADE_REHEARSAL_COMPETING = $competingFile;
        UPGRADE_REHEARSAL_SALE = $sale.artifact; UPGRADE_REHEARSAL_CURRENCY = $currency.artifact;
        UPGRADE_REHEARSAL_REPORT = $validation.artifact; UPGRADE_REHEARSAL_BROWSER_RESULT = (Join-Path $EvidenceDirectory 'review-browser.json')
    }
    $previousSettings = @{}
    foreach ($name in $browserSettings.Keys) {
        $previousSettings[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        [Environment]::SetEnvironmentVariable($name, $browserSettings[$name], 'Process')
    }
    try {
        node (Join-Path $PSScriptRoot 'browser-review.cjs')
        if ($LASTEXITCODE -ne 0) { throw 'Browser batch rehearsal failed' }
    } finally {
        foreach ($name in $previousSettings.Keys) { [Environment]::SetEnvironmentVariable($name, $previousSettings[$name], 'Process') }
    }

    $result = [ordered]@{
        previous_version = $previousVersion; current_version = $currentVersion; mysql_version = $MySQLVersion
        before_schema = [int]$schemaBefore; after_schema = [int]$schemaAfter
        original_records = $before; records_preserved_through_upgrade = $true; records_preserved_through_restore = $true
        backup_sha256 = $dumpDigest; original_batch_after_restore_execution = [int]$stillPending; indexed_previous_target = $true
        restored_batch = $resumed; cross_tenant_statuses = @{review = 404; patch = 404; export = 404}
        browser_result = 'review-browser.json'
    }
    $result | ConvertTo-Json -Depth 30 | Set-Content -LiteralPath (Join-Path $EvidenceDirectory 'upgrade-rehearsal.json') -Encoding utf8
    Write-Output 'Previous release, isolated restore and browser review passed.'
} finally {
    foreach ($server in $servers) { Stop-Workspace $server }
    [Environment]::SetEnvironmentVariable('UPGRADE_MYSQL_DSN', $dsnEnv, 'Process')
}
