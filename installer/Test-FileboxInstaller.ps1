Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'FileboxInstaller.psm1') -Force

$script:failures = 0
function Check {
    param([string]$Name, [scriptblock]$Test)
    try {
        if (& $Test) { Write-Host "ok   $Name" } else { Write-Host "FAIL $Name"; $script:failures++ }
    } catch {
        Write-Host "FAIL $Name : $($_.Exception.Message)"
        $script:failures++
    }
}
function Throws {
    param([scriptblock]$Test)
    try { & $Test | Out-Null; return $false } catch { return $true }
}

$good = @{
    Hostname = 'filebox'; User = 'robbin'; Timezone = 'Europe/Amsterdam'; Keymap = 'us'
    TargetPartUuid = '2a9a1c4e-0000-4000-8000-000000000001'; TargetFsType = 'ntfs'
    DataPartUuid = '2a9a1c4e-0000-4000-8000-000000000002'; DataAction = 'keep'
    SshKey = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleExampleExampleExampleExampleExample me@pc'
    PasswordHash = New-Sha512Crypt -Password 'a long password' -Salt 'abcdefgh'
}

Check 'sha512-crypt matches the published test vector' {
    (New-Sha512Crypt -Password 'Hello world!' -Salt 'saltstring') -eq '$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1'
}
Check 'random salt gives different hashes of the right shape' {
    $a = New-Sha512Crypt -Password 'x y z'; $b = New-Sha512Crypt -Password 'x y z'
    ($a -ne $b) -and ($a -match '^\$6\$[./0-9A-Za-z]{12}\$[./0-9A-Za-z]{86}$')
}
Check 'good settings validate' { Assert-InstallSettings $good; $true }
Check 'data partition equal to target is refused' {
    $bad = $good.Clone(); $bad.DataPartUuid = $bad.TargetPartUuid
    Throws { Assert-InstallSettings $bad }
}
Check 'staging partition equal to data is refused' {
    $bad = $good.Clone(); $bad.CleanupPartUuid = $bad.DataPartUuid
    Throws { Assert-InstallSettings $bad }
}
foreach ($case in @(
    @('Hostname', 'bad host'), @('Hostname', 'a;rm -rf /'), @('User', 'Root'), @('User', 'x$(id)'), @('User', 'filebox'),
    @('Timezone', 'Europe/../etc'), @('Keymap', "us`nfoo"), @('TargetPartUuid', "x'; reboot #"),
    @('DataAction', 'wipe'), @('SshKey', "ssh-ed25519 AAAA`nroot::0:0"), @('SshKey', 'not a key'),
    @('PasswordHash', 'plaintext')
)) {
    Check "rejects $($case[0]) = $($case[1] -replace "`n", '\n')" {
        $bad = $good.Clone(); $bad[$case[0]] = $case[1]
        Throws { Assert-InstallSettings $bad }
    }
}
Check 'preseed has no unfilled placeholders and carries the values' {
    $text = New-PreseedText $good (Join-Path $PSScriptRoot 'preseed.cfg.tmpl')
    ($text -notmatch '@@') -and $text.Contains('passwd/username string robbin') -and $text.Contains($good.PasswordHash) -and ($text -notmatch "`r")
}
Check 'conf file quotes every value' {
    $conf = New-ConfText $good
    $conf.Contains("TARGET_PARTUUID='2a9a1c4e-0000-4000-8000-000000000001'") -and $conf.Contains("DATA_ACTION='keep'") -and ($conf -notmatch "`r")
}
Check 'shell literal escapes single quotes' { (ConvertTo-ShellLiteral "a'b") -eq "'a'\''b'" }
Check 'cpio archive is well formed' {
    $files = [ordered]@{ 'a.txt' = @{ Mode = 0x81A4; Data = [byte[]](65, 66, 67) } }
    $bytes = New-CpioArchive $files
    $text = [System.Text.Encoding]::ASCII.GetString($bytes)
    ($bytes.Length % 4 -eq 0) -and $text.StartsWith('070701') -and $text.Contains('a.txt') -and $text.Contains('ABC') -and $text.Contains('TRAILER!!!')
}
Check 'payload initrd keeps the original bytes then adds a gzip member' {
    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString())
    New-Item -ItemType Directory $tmp | Out-Null
    try {
        [System.IO.File]::WriteAllBytes("$tmp\in", [byte[]](1, 2, 3, 4))
        New-PayloadInitrd -InitrdPath "$tmp\in" -OutPath "$tmp\out" -Files ([ordered]@{ 'x' = @{ Mode = 0x81A4; Data = [byte[]](1) } })
        $out = [System.IO.File]::ReadAllBytes("$tmp\out")
        ($out[0] -eq 1) -and ($out[3] -eq 4) -and ($out[4] -eq 0x1f) -and ($out[5] -eq 0x8b)
    } finally { Remove-Item -Recurse -Force $tmp }
}
Check 'grub config points at kernel and initrd' {
    $cfg = New-GrubConfig -KernelPath '/linux' -InitrdPath '/initrd.gz' -Hostname 'filebox'
    $cfg.Contains('linux /linux auto=true') -and $cfg.Contains('initrd /initrd.gz')
}

Check 'wifi settings fill the preseed' {
    $wifi = $good.Clone(); $wifi.WifiSsid = 'Home Net'; $wifi.WifiPsk = 'correct horse'; $wifi.FirmwarePackages = @('firmware-iwlwifi')
    $text = New-PreseedText $wifi (Join-Path $PSScriptRoot 'preseed.cfg.tmpl')
    $text.Contains('netcfg/wireless_essid string Home Net') -and $text.Contains('netcfg/wireless_wpa string correct horse') -and
        $text.Contains('load_firmware boolean true') -and $text.Contains('sudo network-manager wpasupplicant firmware-iwlwifi') -and ($text -notmatch '@@')
}
Check 'wired preseed has no wifi lines' {
    $text = New-PreseedText $good (Join-Path $PSScriptRoot 'preseed.cfg.tmpl')
    $text.Contains('load_firmware boolean false') -and -not $text.Contains('wireless') -and -not $text.Contains('wpasupplicant')
}
Check 'wifi settings reach the conf file quoted' {
    $wifi = $good.Clone(); $wifi.WifiSsid = "Mom's Net"; $wifi.WifiPsk = 'pass $word 123'
    $conf = New-ConfText $wifi
    $conf.Contains("WIFI_SSID='Mom'\''s Net'") -and $conf.Contains("WIFI_PSK='pass `$word 123'")
}
Check 'wired conf has empty wifi values' { (New-ConfText $good).Contains("WIFI_SSID=''") }
foreach ($case in @(
    @('WifiSsid', "bad`nssid"), @('WifiPsk', 'short'), @('WifiPsk', "has`nnewline-and-long"), @('FirmwarePackages', @('firmware-ok', 'evil;rm'))
)) {
    Check "rejects wifi $($case[0])" {
        $bad = $good.Clone(); $bad.WifiSsid = 'Home'; $bad.WifiPsk = 'long enough key'; $bad[$case[0]] = $case[1]
        Throws { Assert-InstallSettings $bad }
    }
}
Check 'firmware package follows the chip vendor' {
    (Get-WifiFirmwarePackage 'PCI\VEN_8086&DEV_7A70&SUBSYS_00948086') -eq 'firmware-iwlwifi' -and
        (Get-WifiFirmwarePackage 'USB\VID_0BDA&PID_8179') -eq 'firmware-realtek' -and
        $null -eq (Get-WifiFirmwarePackage 'PCI\VEN_ABCD&DEV_0001')
}
if ($script:failures) { Write-Host "$script:failures failed"; exit 1 }
Write-Host 'all passed'
