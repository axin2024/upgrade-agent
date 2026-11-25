param(
    [Parameter(Mandatory)][string]$MySQLBin,
    [Parameter(Mandatory)][string]$Php,
    [Parameter(Mandatory)][string]$Composer
)

$ErrorActionPreference = 'Stop'
$project = Split-Path $PSScriptRoot -Parent
$MySQLBin = (Resolve-Path -LiteralPath $MySQLBin).Path
$mysql = Join-Path $MySQLBin 'mysql.exe'
$mysqld = Join-Path $MySQLBin 'mysqld.exe'
$mysqladmin = Join-Path $MySQLBin 'mysqladmin.exe'
$root = (New-Item -ItemType Directory -Force -Path (Join-Path $project '.local')).FullName
$name = 'mysql-test-' + [guid]::NewGuid().ToString('N')
$fixture = (New-Item -ItemType Directory -Path (Join-Path $root $name)).FullName
$data = (New-Item -ItemType Directory -Path (Join-Path $fixture 'data')).FullName
$init = Join-Path $fixture 'init.sql'
$password = [guid]::NewGuid().ToString('N')
$rootPassword = [guid]::NewGuid().ToString('N')
$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
$listener.Start()
try { $port = $listener.LocalEndpoint.Port } finally { $listener.Stop() }
$argsForMySQL = @('--no-defaults', '--mysqlx=OFF', '--innodb-buffer-pool-size=32M', '--max-connections=24',
    ('--basedir="{0}"' -f (Split-Path $MySQLBin -Parent)), ('--datadir="{0}"' -f $data),
    ('--log-error="{0}/mysql.log"' -f $fixture))
$client = @('--no-defaults', '--protocol=TCP', '--host=127.0.0.1', "--port=$port")
$variables = @{
    MYSQL_PWD = $password
    UPGRADE_TEST_MYSQL_DSN = "test_upgrade:$password@tcp(127.0.0.1:$port)/upgrade_test"
    UPGRADE_TEST_PHP = (Resolve-Path -LiteralPath $Php).Path
    UPGRADE_TEST_COMPOSER = (Resolve-Path -LiteralPath $Composer).Path
}
$original = @{}
$process = $null
$locationChanged = $false
foreach ($item in $variables.Keys) { $original[$item] = [Environment]::GetEnvironmentVariable($item, 'Process') }
try {
    $initial = Start-Process -FilePath $mysqld -ArgumentList ($argsForMySQL + '--initialize-insecure') -PassThru -Wait -WindowStyle Hidden
    if ($initial.ExitCode -ne 0) { throw 'Unable to initialize disposable MySQL' }
    @"
ALTER USER 'root'@'localhost' IDENTIFIED BY '$rootPassword';
CREATE DATABASE upgrade_test CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin;
CREATE USER 'test_upgrade'@'127.0.0.1' IDENTIFIED BY '$password';
GRANT ALL ON upgrade_test.* TO 'test_upgrade'@'127.0.0.1';
"@ | Set-Content -LiteralPath $init -Encoding ascii
    $process = Start-Process -FilePath $mysqld -ArgumentList ($argsForMySQL + @('--bind-address=127.0.0.1', "--port=$port", ('--init-file="{0}"' -f $init))) -PassThru -WindowStyle Hidden
    foreach ($item in $variables.Keys) { [Environment]::SetEnvironmentVariable($item, $variables[$item], 'Process') }
    $version = $null
    for ($attempt = 0; $attempt -lt 40; $attempt++) {
        if ($process.HasExited) { throw 'Disposable MySQL exited before startup' }
        $version = & $mysql @client --user=test_upgrade --database=upgrade_test --batch --skip-column-names '--execute=SELECT VERSION()' 2>$null
        if ($LASTEXITCODE -eq 0 -and $version) { break }
        Start-Sleep -Milliseconds 250
    }
    if ($version -notmatch '^8\.4\.') { throw 'Expected MySQL 8.4' }
    Remove-Item -LiteralPath $init
    Write-Output "Disposable MySQL $version"
    Push-Location -LiteralPath $project
    $locationChanged = $true
    go test -count=1 -timeout 4m -v ./...
    if ($LASTEXITCODE -ne 0) { throw 'Workspace integration failed' }
} finally {
    if ($locationChanged) { Pop-Location }
    if (Test-Path -LiteralPath $init) { Remove-Item -LiteralPath $init }
    if ($process -and !$process.HasExited) {
        $env:MYSQL_PWD = $rootPassword
        & $mysqladmin @client --user=root --connect-timeout=2 shutdown 2>$null
        if (!$process.WaitForExit(5000)) { Stop-Process -InputObject $process }
    }
    foreach ($item in $original.Keys) { [Environment]::SetEnvironmentVariable($item, $original[$item], 'Process') }
    if (Test-Path -LiteralPath $fixture) {
        $absolute = (Resolve-Path -LiteralPath $fixture).Path
        if ((Split-Path -Parent $absolute) -ne $root -or (Split-Path -Leaf $absolute) -ne $name) {
            throw 'Disposable MySQL cleanup refused the resolved path'
        }
        Remove-Item -LiteralPath $absolute -Force -Recurse
    }
}
