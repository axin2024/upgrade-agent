param(
    [Parameter(Mandatory)][string]$CLI,
    [Parameter(Mandatory)][string]$OutputDirectory
)

$ErrorActionPreference = 'Stop'
$CLI = (Resolve-Path -LiteralPath $CLI).Path
if (Test-Path -LiteralPath $OutputDirectory) { throw 'Choose a new fixture directory' }
$root = (New-Item -ItemType Directory -Path $OutputDirectory).FullName
$index = [ordered]@{}

function Save-ContractFixture([string]$Name, [hashtable]$Files, [string]$Scope) {
    $repo = (New-Item -ItemType Directory -Path (Join-Path $root $Name)).FullName
    foreach ($relative in $Files.Keys) {
        $target = Join-Path $repo $relative
        $null = New-Item -ItemType Directory -Path (Split-Path $target) -Force
        [IO.File]::WriteAllText($target, $Files[$relative].Replace("`r`n", "`n") + "`n")
    }
    $json = & $CLI capture -root $repo -allow $Scope -ref $Name
    if ($LASTEXITCODE -ne 0) { throw "Failed to capture $Name" }
    $json | Set-Content -Encoding utf8NoBOM -LiteralPath (Join-Path $root "$Name.json")
    $script:index[$Name] = ($json -join "`n" | ConvertFrom-Json).snapshot.sha256
}

function New-HTTPContract([string]$Kind, [string]$Route, [string]$Field, [string]$Callsite = '', [int]$Line = 4) {
    $contract = [ordered]@{kind = $Kind; method = 'GET'; path = $Route; required_response_fields = @($Field); response_example = @{$Field = 1200}}
    if ($Callsite) { $contract.callsite = $Callsite; $contract.line = $Line }
    $contract | ConvertTo-Json -Compress
}

foreach ($version in 'old','compat','final') {
    $routes = @()
    if ($version -ne 'final') { $routes += "'/v1/quote' => ['minor' => 1200]" }
    if ($version -ne 'old') { $routes += "'/v2/quote' => ['amount_minor' => 1200]" }
    $router = "<?php`n" + 'header("Content-Type: application/json");' + "`n" +
        '$out = match ($_SERVER["REQUEST_URI"]) { ' + ($routes -join ', ') + ', default => null };' + "`n" +
        'if ($out === null) http_response_code(404); echo json_encode($out);'
    $files = @{
        'src/router.php' = $router
        'src/test.php' = "<?php`n" + '$_SERVER["REQUEST_URI"] = "' + $(if ($version -eq 'final') { '/v2/quote' } else { '/v1/quote' }) +
            '"; ob_start(); require __DIR__."/router.php"; $quote = json_decode(ob_get_clean(), true); if (array_sum($quote) !== 1200) exit(1);'
    }
    if ($version -ne 'final') { $files['contracts/quote-v1.http.json'] = New-HTTPContract producer '/v1/quote' minor }
    if ($version -ne 'old') { $files['contracts/quote-v2.http.json'] = New-HTTPContract producer '/v2/quote' amount_minor }
    Save-ContractFixture "provider-$version" $files 'src,contracts'
}

foreach ($version in 'old','new') {
    $route,$field = if ($version -eq 'old') { '/v1/quote','minor' } else { '/v2/quote','amount_minor' }
    $php = @'
<?php
function checkout(): array {
    $provider = getenv('UPGRADE_PROVIDER_URL');
    return json_decode(file_get_contents($provider . 'ROUTE'), true, 512, JSON_THROW_ON_ERROR);
}
'@
    $phpTest = '<?php require __DIR__."/checkout.php"; if (checkout() !== ["FIELD" => 1200]) exit(1);'
    Save-ContractFixture "php-$version" @{
        'src/checkout.php' = $php.Replace('ROUTE', $route)
        'src/test.php' = $phpTest.Replace('FIELD', $field)
        'contracts/checkout.http.json' = New-HTTPContract consumer $route $field 'src/checkout.php'
    } 'src,contracts'

    $go = @'
package quote

import ("encoding/json"; "net/http"; "os")
func checkout() (map[string]int, error) {
    response, err := http.Get(os.Getenv("UPGRADE_PROVIDER_URL") + "ROUTE")
    if err != nil { return nil, err }; defer response.Body.Close()
    var result map[string]int
    err = json.NewDecoder(response.Body).Decode(&result)
    return result, err
}
'@
    $goTest = @'
package quote

import "testing"
func TestCheckout(t *testing.T) {
    quote, err := checkout()
    if err != nil || quote["FIELD"] != 1200 { t.Fatalf("quote = %v, error = %v", quote, err) }
}
'@
    Save-ContractFixture "go-$version" @{
        'go.mod' = "module example.invalid/contract-estate/quote`n`ngo 1.26.0"
        'src/checkout.go' = $go.Replace('ROUTE', $route)
        'src/checkout_test.go' = $goTest.Replace('FIELD', $field)
        'contracts/checkout.http.json' = New-HTTPContract consumer $route $field 'src/checkout.go' 5
    } 'src,contracts,go.mod'
}

$index | ConvertTo-Json | Set-Content -Encoding utf8NoBOM -LiteralPath (Join-Path $root 'snapshots.json')
$index | ConvertTo-Json
