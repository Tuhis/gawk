<#
.SYNOPSIS
  R74 PP0 Windows Firewall row (docs/76 §3): what stands between another LAN
  machine and an unsigned gawk-relay.exe on this host.

.DESCRIPTION
  Reports the network profile of each connected interface, the firewall
  profiles, the gawk-relay.exe signature, every firewall rule that names the
  program (the ones the first-run prompt creates and the console's
  "gawk-relay" rule), and who owns the relay's UDP port.

  With -Watch N (elevated) it also turns on dropped-packet logging for the
  active profiles for N seconds, while you dial the relay from another LAN
  machine, and reports the UDP drops to the relay's port and the relay's
  accepted /echo sessions over the same window (its metrics on
  127.0.0.1:2112). Logging is put back as it was.

  It changes no rule. The last line is a PP0 result line for docs/76 §3.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File firewall.ps1
  powershell -ExecutionPolicy Bypass -File firewall.ps1 -Watch 60 -Label "after Cancel on the prompt"
#>
param(
  [string]$Exe = "",
  [int]$Port = 4433,
  [int]$Watch = 0,
  [string]$Metrics = "http://127.0.0.1:2112/metrics",
  [string]$Label = ""
)
$ErrorActionPreference = "Stop"

function Section($t) { Write-Host ""; Write-Host "== $t" }

function EchoAccepted {
  try {
    $m = (Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 $Metrics).Content
    $line = $m -split "`n" | Where-Object { $_ -match '^gawk_connections_total\{' -and $_ -match 'route="echo"' -and $_ -match 'outcome="accepted"' }
    if ($line) { return [double](($line -split '\s+')[-1]) }
    return 0
  } catch { return $null }
}

if (-not $Exe) {
  $p = Get-Process -Name gawk-relay -ErrorAction SilentlyContinue | Select-Object -First 1
  if ($p) { $Exe = $p.Path }
}
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
$os = Get-CimInstance Win32_OperatingSystem
$build = "$($os.Caption) $($os.Version) build $($os.BuildNumber)"

Section "Host"
Write-Host "OS:        $build"
Write-Host "Elevated:  $admin"
Write-Host "Program:   $(if ($Exe) { $Exe } else { '(gawk-relay not running; pass -Exe)' })"
$sig = "unknown"
if ($Exe -and (Test-Path $Exe)) {
  $s = Get-AuthenticodeSignature $Exe
  $sig = "$($s.Status)"
  Write-Host "Signature: $($s.Status) $($s.StatusMessage)"
  $zone = Get-Content -Path $Exe -Stream Zone.Identifier -ErrorAction SilentlyContinue
  Write-Host "Mark of the Web: $(if ($zone) { ($zone -join ' ') } else { 'none' })"
}

Section "Network profile per interface"
$profiles = Get-NetConnectionProfile
$profiles | Format-Table InterfaceAlias, Name, NetworkCategory, IPv4Connectivity -AutoSize | Out-String | Write-Host
$categories = ($profiles | ForEach-Object { "$($_.InterfaceAlias)=$($_.NetworkCategory)" }) -join ", "

Section "Firewall profiles"
$fw = Get-NetFirewallProfile
$fw | Format-Table Name, Enabled, DefaultInboundAction, AllowInboundRules, NotifyOnListen, LogBlocked, LogFileName -AutoSize | Out-String | Write-Host

Section "Rules naming the program, and rules named gawk-relay"
$rules = @()
if ($Exe) {
  $rules += Get-NetFirewallApplicationFilter -ErrorAction SilentlyContinue |
    Where-Object { $_.Program -and ($_.Program -ieq $Exe -or [Environment]::ExpandEnvironmentVariables($_.Program) -ieq $Exe) } |
    Get-NetFirewallRule
}
$rules += Get-NetFirewallRule -DisplayName "gawk-relay" -ErrorAction SilentlyContinue
$rules = $rules | Sort-Object -Property Name -Unique
$ruleText = @()
foreach ($r in $rules) {
  $pf = $r | Get-NetFirewallPortFilter
  $af = $r | Get-NetFirewallAddressFilter
  $t = "$($r.DisplayName) [$($r.Direction) $($r.Action) enabled=$($r.Enabled) profile=$($r.Profile) $($pf.Protocol) local=$($pf.LocalPort) remote=$($af.RemoteAddress -join ',')]"
  Write-Host "  $t"
  $ruleText += $t
}
if (-not $rules) { Write-Host "  (none)" }

Section "UDP $Port"
$ep = Get-NetUDPEndpoint -LocalPort $Port -ErrorAction SilentlyContinue
foreach ($e in $ep) {
  $owner = (Get-Process -Id $e.OwningProcess -ErrorAction SilentlyContinue).Path
  Write-Host "  $($e.LocalAddress):$($e.LocalPort) owned by pid $($e.OwningProcess) $owner"
}
if (-not $ep) { Write-Host "  nothing listens on UDP $Port" }

$result = "profiles [$categories]; program signature $sig; rules: $(if ($ruleText) { $ruleText -join '; ' } else { 'none' })"

if ($Watch -gt 0) {
  if (-not $admin) { throw "-Watch needs an elevated PowerShell (it toggles dropped-packet logging and reads the log)" }
  $active = $fw | Where-Object { $_.Enabled }
  $saved = @{}
  foreach ($f in $active) { $saved[$f.Name] = $f.LogBlocked }
  $before = EchoAccepted
  $start = Get-Date
  Section "Watching for $Watch s: dial https://<this host's LAN address>:$Port/echo from another LAN machine now"
  Write-Host "  e.g. gawk-echo -url https://<ip>:$Port/echo -cert-hash <hash> -count 3"
  try {
    foreach ($f in $active) { Set-NetFirewallProfile -Name $f.Name -LogBlocked True }
    Start-Sleep -Seconds $Watch
  } finally {
    foreach ($k in $saved.Keys) { Set-NetFirewallProfile -Name $k -LogBlocked $saved[$k] }
  }
  $after = EchoAccepted
  $drops = @()
  foreach ($file in ($active | ForEach-Object { [Environment]::ExpandEnvironmentVariables($_.LogFileName) } | Sort-Object -Unique)) {
    if (-not (Test-Path $file)) { continue }
    # Fields: date time action protocol src-ip dst-ip src-port dst-port ...
    $drops += Get-Content $file | Where-Object { $_ -match '^\d{4}-' } | ForEach-Object {
      $f = $_ -split ' '
      if ($f[2] -eq 'DROP' -and $f[3] -eq 'UDP' -and $f[7] -eq "$Port" -and ([datetime]"$($f[0]) $($f[1])") -ge $start.AddSeconds(-2)) { "$($f[4])->$($f[5])" }
    }
  }
  $dropSummary = if ($drops) { ($drops | Group-Object | ForEach-Object { "$($_.Name) x$($_.Count)" }) -join ', ' } else { 'none' }
  $sessions = if ($null -ne $before -and $null -ne $after) { "+$($after - $before)" } else { "unknown (metrics at $Metrics unreachable)" }
  Section "Watch result"
  Write-Host "  UDP drops to :$Port in the log: $dropSummary"
  Write-Host "  accepted /echo sessions at the relay: $sessions"
  $result += "; during a ${Watch}s watch: UDP drops to :$Port $dropSummary, relay /echo sessions accepted $sessions"
}

Section "Result line"
$lbl = if ($Label) { " ($Label)" } else { "" }
Write-Host "PP0 windows-firewall$lbl | firewall.ps1 on the relay host$(if ($Watch) { ", -Watch $Watch while another LAN machine dialed /echo" }) | $build, gawk-relay $(if ($Exe) { (Get-Item $Exe).VersionInfo.FileVersion }) | $result"
