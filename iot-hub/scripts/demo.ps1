<#
.SYNOPSIS
  Runs a demo IoT Hub with simulated sensors and devices that answer commands.

.DESCRIPTION
  The Windows counterpart of scripts/demo.sh: it needs only Go, no bash and no
  Python. Builds the hub and the simulators, starts a multi-tenant hub, seeds a
  demo tenant, and prints a sign-in token for each role.

  Data goes to .\demo-data, which is deleted on each start. Ctrl+C stops
  everything, or with -Detach the hub keeps running after the script returns.

.EXAMPLE
  .\scripts\demo.ps1
  .\scripts\demo.ps1 -HttpPort 9000 -MqttPort 1884
  .\scripts\demo.ps1 -Detach      # leave it running; stop with .\scripts\demo-stop.ps1
#>
[CmdletBinding()]
param(
  [int]$HttpPort = 8080,
  [int]$MqttPort = 1883,
  [switch]$SkipBuild,
  [switch]$Detach
)

$ErrorActionPreference = 'Stop'
Set-Location (Split-Path $PSScriptRoot -Parent)

$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) {
  $local = "$env:LOCALAPPDATA\Programs\Go\bin\go.exe"
  if (Test-Path $local) { $go = Get-Item $local } else { throw "Go is not installed. Get it from https://go.dev/dl/ or run: winget install GoLang.Go" }
}

$data = 'demo-data'
$bin = Join-Path $data 'bin'
$base = "http://127.0.0.1:$HttpPort"
$super = 'platform-demo-token'
$DemoEmail = 'ana@plant.co'
$DemoPassword = 'demo plant password'
$procs = @()

function Invoke-Api {
  param([string]$Method, [string]$Path, $Body, [string]$Token = $super, [string]$Tenant = 'acme')
  $headers = @{ Authorization = "Bearer $Token" }
  if ($Tenant -and -not $Path.StartsWith('/api/admin')) { $headers['X-Tenant'] = $Tenant }
  $req = @{ Method = $Method; Uri = "$base$Path"; Headers = $headers; TimeoutSec = 30 }
  if ($null -ne $Body) {
    # UTF-8 bytes: sensor units such as °C must survive the round trip.
    $req['Body'] = [Text.Encoding]::UTF8.GetBytes(($Body | ConvertTo-Json -Depth 12 -Compress))
    $req['ContentType'] = 'application/json'
  }
  try { Invoke-RestMethod @req } catch { throw "$Method $Path failed: $($_.Exception.Message)" }
}

try {
  if (-not $SkipBuild) {
    if (Test-Path $data) { Remove-Item -Recurse -Force $data }
    New-Item -ItemType Directory -Force $bin | Out-Null
    foreach ($c in 'iothub', 'simulate', 'devicesim') {
      Write-Host "building $c..."
      & $go.Source build -o "$bin\$c.exe" "./cmd/$c"
      if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/$c failed" }
    }
  }

  # -auth-file too: it defaults to .\data\devices.json, so without it the demo
  # leaves credentials outside demo-data and a re-run fails with 409 Conflict.
  $hubArgs = @('-tenancy', 'multi', '-token', $super, '-data', "$data\iothub.json", '-db', "sqlite:$data\readings.db",
    '-auth-file', "$data\devices.json", '-backup-dir', "$data\backups",
    '-http', "127.0.0.1:$HttpPort", '-mqtt', "127.0.0.1:$MqttPort")
  $procs += Start-Process -FilePath "$bin\iothub.exe" -ArgumentList $hubArgs -PassThru -NoNewWindow `
    -RedirectStandardOutput "$data\hub.log" -RedirectStandardError "$data\hub.err"

  $up = $false
  foreach ($i in 1..60) {
    try { Invoke-RestMethod "$base/api/health" -TimeoutSec 2 | Out-Null; $up = $true; break } catch { Start-Sleep -Milliseconds 300 }
  }
  if (-not $up) { throw "the hub did not start; see $data\hub.err" }

  # ---- seed: tenants, people, devices, sensors, commands, dashboard ----
  $cred = @{}
  # deviceSelfService: the tenant's own admins may add devices and users.
  # Without it only the platform operator can, and their Add buttons are hidden.
  Invoke-Api POST '/api/admin/tenants' @{ id = 'acme'; name = 'Acme Mining'; deviceSelfService = $true
    quota = @{ messagesPerDay = 500000; maxSensors = 200; maxDevices = 100 } } | Out-Null
  Invoke-Api POST '/api/admin/tenants' @{ id = 'globex'; name = 'Globex Foods' } | Out-Null

  foreach ($p in @(@('ops', 'admin', 'Plant manager'), @('shift', 'operator', 'Shift supervisor'), @('screen', 'viewer', 'Control-room screen'))) {
    $cred[$p[0]] = (Invoke-Api POST '/api/devices' @{ id = $p[0]; role = $p[1]; auth = 'token'; note = $p[2] }).token
  }
  $cred['gateway-1'] = (Invoke-Api POST '/api/devices' @{ id = 'gateway-1'; role = 'service'; sensors = @('*'); auth = 'token'; note = 'Edge gateway (simulator)' }).token
  foreach ($d in @(@('pump-1', 'Borehole pump A'), @('pump-2', 'Borehole pump B'), @('pump-3', 'Spare pump (offline)'), @('valve-1', 'Main supply valve'))) {
    $cred[$d[0]] = (Invoke-Api POST '/api/devices' @{ id = $d[0]; role = 'device'; sensors = @($d[0]); auth = 'keys'; note = $d[1] }).connectionString
  }
  $cred['meter-9'] = (Invoke-Api POST '/api/devices' @{ id = 'meter-9'; role = 'device'; sensors = @('meter-9'); auth = 'token'; note = 'Energy meter, substation' }).token
  # A person who signs in with an email and password, to try that path.
  Invoke-Api POST '/api/devices' @{ id = 'ana'; role = 'admin'; auth = 'password'; email = $DemoEmail
    password = $DemoPassword; note = 'Plant manager (email sign-in)' } | Out-Null

  # [char]0xB0 for the degree sign: Windows PowerShell 5.1 reads a .ps1 without
  # a BOM as the system codepage, which would mangle a literal non-ASCII byte.
  $degC = "$([char]0xB0)C"
  Invoke-Api PUT '/api/sensors/env-1' @{ name = 'Boiler room'; kind = 'environment'; location = 'Plant A'; fields = @{
      temperature = @{ unit = $degC; min = 0; max = 50; detect = @{ high = 24 } }; humidity = @{ unit = '%'; min = 0; max = 100 } } } | Out-Null
  Invoke-Api PUT '/api/sensors/tank-1' @{ name = 'Raw water tank'; kind = 'tank'; location = 'Plant A'; fields = @{
      level = @{ unit = '%'; min = 0; max = 100; detect = @{ low = 10; high = 95 } } } } | Out-Null
  Invoke-Api PUT '/api/sensors/power-1' @{ name = 'Main feeder'; kind = 'meter'; fields = @{
      voltage = @{ unit = 'V' }; current = @{ unit = 'A' }; power = @{ unit = 'W' }
      kw = @{ label = 'Power (kW)'; unit = 'kW'; calc = @{ formula = 'voltage * current / 1000' } } } } | Out-Null

  Invoke-Api PUT '/api/commands/reboot' @{ label = 'Reboot'; devices = @('pump-*'); kind = 'method'; timeout = '5s'; confirm = $true
    params = @(@{ name = 'delay'; label = 'Delay (s)'; type = 'number'; min = 0; max = 60; default = 5 })
    retry  = @{ attempts = 3; every = '15s'; backoff = 2; jitter = 0.2 } } | Out-Null
  Invoke-Api PUT '/api/commands/set-valve' @{ label = 'Set valve'; devices = @('valve-*', 'pump-*'); kind = 'message'; payload = @{ cmd = 'valve' }
    params = @(@{ name = 'position'; label = 'Position'; type = 'choice'; choices = @('open', 'closed'); required = $true }) } | Out-Null
  Invoke-Api PUT '/api/devices/meter-9/expected-interval' @{ interval = '1m' } | Out-Null

  Invoke-Api PUT '/api/dashboard' @{ tiles = @(
      @{ id = 't1'; type = 'stat'; sensor = 'tank-1'; field = 'level'; w = 1; h = 1; options = @{ warn = 80; crit = 95 } },
      @{ id = 't2'; type = 'meter'; sensor = 'env-1'; field = 'temperature'; w = 1; h = 1; options = @{ min = 0; max = 50; warn = 22; crit = 24 } },
      @{ id = 't3'; type = 'line'; sensor = 'power-1'; fields = @('kw'); w = 2; h = 2; options = @{} },
      @{ id = 't4'; type = 'anomalies'; sensor = ''; w = 2; h = 2; options = @{} },
      @{ id = 't5'; type = 'state'; sensor = 'door-1'; field = 'door'; w = 1; h = 1; options = @{ ok = 'closed' } },
      @{ id = 't6'; type = 'stat'; sensor = 'pump-1'; field = 'vibration'; w = 1; h = 1; options = @{} }) } | Out-Null

  # No BOM: Set-Content -Encoding utf8 adds one in PowerShell 5.1, and a BOM
  # breaks JSON.parse, jq and json.load for anything else reading this file.
  [IO.File]::WriteAllText((Join-Path (Get-Location) "$data\creds.json"),
    ($cred | ConvertTo-Json), (New-Object Text.UTF8Encoding $false))
  Write-Host "seeded: $($cred.Keys -join ', ')"

  # ---- simulators: six sensors, three devices that answer commands ----
  $procs += Start-Process -FilePath "$bin\simulate.exe" -PassThru -NoNewWindow `
    -ArgumentList @('-mqtt=', '-http', $base, '-token', $cred['gateway-1'], '-every', '1s') `
    -RedirectStandardOutput "$data\sim.log" -RedirectStandardError "$data\sim.err"
  foreach ($d in 'pump-1', 'pump-2', 'valve-1') {
    $procs += Start-Process -FilePath "$bin\devicesim.exe" -PassThru -NoNewWindow `
      -ArgumentList @('-cs', $cred[$d], '-mqtt-port', "$MqttPort") `
      -RedirectStandardOutput "$data\$d.log" -RedirectStandardError "$data\$d.err"
  }
  Invoke-Api POST '/api/sensors/meter-9/data' @{ kwh = 1204.5 } -Token $cred['meter-9'] | Out-Null

  Write-Host ""
  Write-Host "  IoT Hub demo is running: $base" -ForegroundColor Green
  Write-Host ""
  Write-Host "  Sign in with an email and password:"
  Write-Host "    Admin (Acme)        $DemoEmail / $DemoPassword"
  Write-Host ""
  Write-Host "  or with one of these tokens:"
  Write-Host "    Platform operator   $super"
  Write-Host "    Admin (Acme)        $($cred['ops'])"
  Write-Host "    Operator (Acme)     $($cred['shift'])"
  Write-Host "    Viewer (Acme)       $($cred['screen'])"
  Write-Host ""
  Write-Host "  pump-1, pump-2 and valve-1 answer commands; pump-3 stays offline (watch retries);"
  Write-Host "  meter-9 goes overdue after ~2 minutes."
  if ($Detach) {
    Write-Host "  Running in the background. Stop it with .\scripts\demo-stop.ps1"
    Write-Host ""
  } else {
    Write-Host "  Ctrl+C stops everything."
    Write-Host ""
    while ($true) { Start-Sleep -Seconds 1 }
  }
} finally {
  if (-not $Detach) {
    foreach ($p in $procs) {
      if ($p -and -not $p.HasExited) { try { Stop-Process -Id $p.Id -Force -ErrorAction Stop } catch {} }
    }
  }
}
