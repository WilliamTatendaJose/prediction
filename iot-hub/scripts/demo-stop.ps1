<#
.SYNOPSIS
  Stops a demo started with scripts\demo.ps1 -Detach.

.DESCRIPTION
  Only stops processes running from this repository's demo-data\bin, so a
  hub or simulator you started from somewhere else is left alone.
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
Set-Location (Split-Path $PSScriptRoot -Parent)
$bin = (Join-Path (Get-Location) 'demo-data\bin')

$stopped = 0
foreach ($name in 'iothub', 'simulate', 'devicesim') {
  foreach ($p in @(Get-Process -Name $name -ErrorAction SilentlyContinue)) {
    $path = $null
    try { $path = $p.Path } catch { }   # Access denied on a process we don't own
    if ($path -and $path.StartsWith($bin, [StringComparison]::OrdinalIgnoreCase)) {
      try { Stop-Process -Id $p.Id -Force -ErrorAction Stop; $stopped++ } catch { Write-Warning "could not stop $name ($($p.Id)): $($_.Exception.Message)" }
    }
  }
}
if ($stopped) { Write-Host "stopped $stopped demo process(es)" } else { Write-Host "no demo processes were running" }
