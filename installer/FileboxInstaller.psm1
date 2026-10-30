Set-StrictMode -Version 2.0

# Building blocks for the unattended filebox install. Works on Windows PowerShell 5.1 and later.

Add-Type -TypeDefinition @'
using System;
using System.Security.Cryptography;
using System.Text;

public static class FileboxSha512Crypt
{
    const string Alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";
    static readonly int[][] Order = {
        new[]{0,21,42}, new[]{22,43,1}, new[]{44,2,23}, new[]{3,24,45}, new[]{25,46,4}, new[]{47,5,26},
        new[]{6,27,48}, new[]{28,49,7}, new[]{50,8,29}, new[]{9,30,51}, new[]{31,52,10}, new[]{53,11,32},
        new[]{12,33,54}, new[]{34,55,13}, new[]{56,14,35}, new[]{15,36,57}, new[]{37,58,16}, new[]{59,17,38},
        new[]{18,39,60}, new[]{40,61,19}, new[]{62,20,41}
    };

    static byte[] Concat(params byte[][] parts)
    {
        int length = 0;
        foreach (var p in parts) length += p.Length;
        var all = new byte[length];
        int at = 0;
        foreach (var p in parts) { Buffer.BlockCopy(p, 0, all, at, p.Length); at += p.Length; }
        return all;
    }

    static byte[] Repeat(byte[] source, int length)
    {
        var result = new byte[length];
        for (int i = 0; i < length; i++) result[i] = source[i % source.Length];
        return result;
    }

    public static string Hash(string password, string salt, int rounds)
    {
        var p = Encoding.UTF8.GetBytes(password);
        var s = Encoding.UTF8.GetBytes(salt);
        byte[] digest;
        using (var sha = SHA512.Create())
        {
            var b = sha.ComputeHash(Concat(p, s, p));
            var a = Concat(p, s, Repeat(b, p.Length));
            for (int i = p.Length; i > 0; i >>= 1) a = Concat(a, (i & 1) != 0 ? b : p);
            digest = sha.ComputeHash(a);

            var dp = sha.ComputeHash(Repeat(p, p.Length * p.Length));
            var pp = Repeat(dp, p.Length);
            var ds = sha.ComputeHash(Repeat(s, s.Length * (16 + digest[0])));
            var ss = Repeat(ds, s.Length);

            for (int i = 0; i < rounds; i++)
            {
                var c = (i & 1) != 0 ? pp : digest;
                if (i % 3 != 0) c = Concat(c, ss);
                if (i % 7 != 0) c = Concat(c, pp);
                c = Concat(c, (i & 1) != 0 ? digest : pp);
                digest = sha.ComputeHash(c);
            }
        }

        var text = new StringBuilder();
        foreach (var group in Order)
        {
            int value = (digest[group[0]] << 16) | (digest[group[1]] << 8) | digest[group[2]];
            for (int i = 0; i < 4; i++) { text.Append(Alphabet[value & 0x3f]); value >>= 6; }
        }
        int last = digest[63];
        text.Append(Alphabet[last & 0x3f]);
        text.Append(Alphabet[last >> 6]);

        return "$6$" + salt + "$" + text.ToString();
    }
}
'@

function New-Sha512Crypt {
    param(
        [Parameter(Mandatory)][string]$Password,
        [string]$Salt
    )

    if (-not $Salt) {
        $bytes = New-Object byte[] 12
        $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
        $rng.GetBytes($bytes)
        $rng.Dispose()
        $alphabet = './0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz'
        $Salt = -join ($bytes | ForEach-Object { $alphabet[$_ % 64] })
    }
    if ($Salt -notmatch '^[./0-9A-Za-z]{1,16}$') { throw 'Salt must be 1 to 16 characters from ./0-9A-Za-z' }

    return [FileboxSha512Crypt]::Hash($Password, $Salt, 5000)
}

# Values end up inside a POSIX sh file, so only a strict set of characters passes unquoted.
function ConvertTo-ShellLiteral {
    param([AllowEmptyString()][string]$Value)

    return "'" + $Value.Replace("'", "'\''") + "'"
}

function Assert-InstallSettings {
    param([Parameter(Mandatory)][hashtable]$Settings)

    $rules = [ordered]@{
        Hostname      = '^[a-z][a-z0-9-]{0,30}$'
        User          = '^[a-z][a-z0-9_-]{0,30}$'
        Timezone      = '^[A-Za-z]+(/[A-Za-z0-9_+-]+)+$|^UTC$'
        Keymap        = '^[a-z]{2,3}(-[a-z0-9]+)?$'
        TargetPartUuid = '^[0-9a-fA-F-]{8,40}$'
        TargetFsType  = '^[a-z0-9]+$'
        DataAction    = '^(keep|format|none)$'
    }
    foreach ($key in $rules.Keys) {
        if (-not $Settings.ContainsKey($key) -or [string]$Settings[$key] -cnotmatch $rules[$key]) {
            throw "Invalid or missing setting: $key"
        }
    }
    if ($Settings.ContainsKey('WifiSsid') -and $Settings.WifiSsid) {
        if ([string]$Settings.WifiSsid -cnotmatch '^[ -~]{1,32}$') { throw 'Invalid setting: WifiSsid (1 to 32 plain ASCII characters)' }
        if (-not $Settings.ContainsKey('WifiPsk') -or [string]$Settings.WifiPsk -cnotmatch '^[ -~]{8,63}$') { throw 'Invalid or missing setting: WifiPsk (8 to 63 plain ASCII characters)' }
    }
    if ($Settings.ContainsKey('FirmwarePackages')) {
        foreach ($package in @($Settings.FirmwarePackages)) {
            if ([string]$package -cnotmatch '^firmware-[a-z0-9-]+$') { throw "Invalid firmware package name: $package" }
        }
    }
    if ($Settings.User -in @('root', 'filebox')) { throw 'The login name cannot be root or filebox.' }
    if ($Settings.DataAction -ne 'none') {
        if (-not $Settings.ContainsKey('DataPartUuid') -or [string]$Settings.DataPartUuid -cnotmatch '^[0-9a-fA-F-]{8,40}$') {
            throw 'Invalid or missing setting: DataPartUuid'
        }
        if ($Settings.DataPartUuid -eq $Settings.TargetPartUuid) { throw 'The data partition cannot also be the partition Debian replaces.' }
    }
    if ($Settings.ContainsKey('CleanupPartUuid') -and $Settings.CleanupPartUuid) {
        if ([string]$Settings.CleanupPartUuid -cnotmatch '^[0-9a-fA-F-]{8,40}$') { throw 'Invalid setting: CleanupPartUuid' }
        if ($Settings.ContainsKey('DataPartUuid') -and $Settings.CleanupPartUuid -eq $Settings.DataPartUuid) { throw 'The staging partition cannot be the data partition.' }
    }
    if (-not $Settings.ContainsKey('SshKey') -or [string]$Settings.SshKey -cnotmatch '^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com) [A-Za-z0-9+/=]+( [^\r\n]*)?$') {
        throw 'Invalid or missing setting: SshKey (paste one public key line)'
    }
    if (-not $Settings.ContainsKey('PasswordHash') -or [string]$Settings.PasswordHash -cnotmatch '^\$6\$[./0-9A-Za-z]{1,16}\$[./0-9A-Za-z]{86}$') {
        throw 'Invalid or missing setting: PasswordHash'
    }
}

function New-PreseedText {
    param(
        [Parameter(Mandatory)][hashtable]$Settings,
        [Parameter(Mandatory)][string]$TemplatePath
    )

    Assert-InstallSettings $Settings
    $text = [System.IO.File]::ReadAllText($TemplatePath)
    $map = @{
        '@@KEYMAP@@'        = $Settings.Keymap
        '@@HOSTNAME@@'      = $Settings.Hostname
        '@@USER@@'          = $Settings.User
        '@@PASSWORD_HASH@@' = $Settings.PasswordHash
        '@@TIMEZONE@@'      = $Settings.Timezone
    }
    $wifi = ''
    $loadFirmware = 'false'
    if ($Settings.ContainsKey('WifiSsid') -and $Settings.WifiSsid) {
        $loadFirmware = 'true'
        $wifi = (@(
            "d-i netcfg/wireless_essid string $($Settings.WifiSsid)"
            "d-i netcfg/wireless_essid_again string $($Settings.WifiSsid)"
            'd-i netcfg/wireless_security_type select wpa'
            "d-i netcfg/wireless_wpa string $($Settings.WifiPsk)"
        ) -join "`n")
    }
    $packages = ''
    if ($Settings.ContainsKey('FirmwarePackages') -and $Settings.FirmwarePackages) { $packages = ' network-manager wpasupplicant ' + (@($Settings.FirmwarePackages) -join ' ') }
    $map['@@WIFI_LINES@@'] = $wifi
    $map['@@LOAD_FIRMWARE@@'] = $loadFirmware
    $map['@@EXTRA_PACKAGES@@'] = $packages
    foreach ($key in $map.Keys) { $text = $text.Replace($key, [string]$map[$key]) }
    if ($text -match '@@[A-Z_]+@@') { throw "Unfilled placeholder in preseed: $($Matches[0])" }

    return ($text -replace "`r`n", "`n")
}

function New-ConfText {
    param([Parameter(Mandatory)][hashtable]$Settings)

    Assert-InstallSettings $Settings
    $data = ''
    if ($Settings.ContainsKey('DataPartUuid') -and $Settings.DataPartUuid) { $data = [string]$Settings.DataPartUuid }
    $cleanup = ''
    $wifiSsid = ''
    $wifiPsk = ''
    if ($Settings.ContainsKey('WifiSsid') -and $Settings.WifiSsid) { $wifiSsid = [string]$Settings.WifiSsid; $wifiPsk = [string]$Settings.WifiPsk }
    if ($Settings.ContainsKey('CleanupPartUuid') -and $Settings.CleanupPartUuid) { $cleanup = [string]$Settings.CleanupPartUuid }

    $lines = @(
        'TARGET_PARTUUID=' + (ConvertTo-ShellLiteral $Settings.TargetPartUuid)
        'TARGET_FSTYPE=' + (ConvertTo-ShellLiteral $Settings.TargetFsType)
        'DATA_PARTUUID=' + (ConvertTo-ShellLiteral $data)
        'DATA_ACTION=' + (ConvertTo-ShellLiteral $Settings.DataAction)
        'CLEANUP_PARTUUID=' + (ConvertTo-ShellLiteral $cleanup)
        'FILEBOX_USER=' + (ConvertTo-ShellLiteral $Settings.User)
        'SSH_KEY=' + (ConvertTo-ShellLiteral $Settings.SshKey)
        'WIFI_SSID=' + (ConvertTo-ShellLiteral $wifiSsid)
        'WIFI_PSK=' + (ConvertTo-ShellLiteral $wifiPsk)
    )

    return (($lines -join "`n") + "`n")
}

# A cpio "newc" archive, the format the Linux initramfs loader reads.
function New-CpioArchive {
    param([Parameter(Mandatory)][System.Collections.Specialized.OrderedDictionary]$Files)

    $stream = New-Object System.IO.MemoryStream
    $ascii = [System.Text.Encoding]::ASCII
    $inode = 1

    $writeEntry = {
        param([string]$Name, [byte[]]$Data, [int]$Mode)
        $nameBytes = $ascii.GetBytes($Name + [char]0)
        $fields = @($inode, $Mode, 0, 0, 1, 0, $Data.Length, 0, 0, 0, 0, $nameBytes.Length, 0)
        $header = '070701' + (($fields | ForEach-Object { '{0:X8}' -f $_ }) -join '')
        $headerBytes = $ascii.GetBytes($header)
        $stream.Write($headerBytes, 0, $headerBytes.Length)
        $stream.Write($nameBytes, 0, $nameBytes.Length)
        $pad = (4 - (($headerBytes.Length + $nameBytes.Length) % 4)) % 4
        if ($pad) { $stream.Write((New-Object byte[] $pad), 0, $pad) }
        if ($Data.Length) { $stream.Write($Data, 0, $Data.Length) }
        $pad = (4 - ($Data.Length % 4)) % 4
        if ($pad) { $stream.Write((New-Object byte[] $pad), 0, $pad) }
    }

    foreach ($name in $Files.Keys) {
        $entry = $Files[$name]
        & $writeEntry $name ([byte[]]$entry.Data) ([int]$entry.Mode)
        $inode++
    }
    & $writeEntry 'TRAILER!!!' ([byte[]]@()) 0

    $bytes = $stream.ToArray()
    $stream.Dispose()

    return , $bytes
}

function Compress-Gzip {
    param([Parameter(Mandatory)][byte[]]$Bytes)

    $out = New-Object System.IO.MemoryStream
    $gz = New-Object System.IO.Compression.GZipStream($out, [System.IO.Compression.CompressionMode]::Compress, $true)
    $gz.Write($Bytes, 0, $Bytes.Length)
    $gz.Dispose()
    $result = $out.ToArray()
    $out.Dispose()

    return , $result
}

# Debian's initrd.gz followed by a second gzip member holding our files: the kernel unpacks both.
function New-PayloadInitrd {
    param(
        [Parameter(Mandatory)][string]$InitrdPath,
        [Parameter(Mandatory)][string]$OutPath,
        [Parameter(Mandatory)][System.Collections.Specialized.OrderedDictionary]$Files
    )

    $payload = Compress-Gzip (New-CpioArchive $Files)
    $original = [System.IO.File]::ReadAllBytes($InitrdPath)
    $stream = [System.IO.File]::Open($OutPath, [System.IO.FileMode]::Create, [System.IO.FileAccess]::Write)
    try {
        $stream.Write($original, 0, $original.Length)
        $stream.Write($payload, 0, $payload.Length)
    } finally {
        $stream.Dispose()
    }
}

function New-InstallerFileSet {
    param(
        [Parameter(Mandatory)][hashtable]$Settings,
        [Parameter(Mandatory)][string]$InstallerDir,
        [Parameter(Mandatory)][string]$UpdateScriptPath
    )

    $utf8 = New-Object System.Text.UTF8Encoding($false)
    $readScript = { param($path) $utf8.GetBytes(([System.IO.File]::ReadAllText($path) -replace "`r`n", "`n")) }
    $files = [ordered]@{}
    $files['preseed.cfg'] = @{ Mode = 0x81A4; Data = $utf8.GetBytes((New-PreseedText $Settings (Join-Path $InstallerDir 'preseed.cfg.tmpl'))) }
    $files['filebox.conf'] = @{ Mode = 0x8180; Data = $utf8.GetBytes((New-ConfText $Settings)) }
    $files['filebox-early.sh'] = @{ Mode = 0x81ED; Data = (& $readScript (Join-Path $InstallerDir 'early.sh')) }
    $files['filebox-late.sh'] = @{ Mode = 0x81ED; Data = (& $readScript (Join-Path $InstallerDir 'late.sh')) }
    $files['filebox-firstboot.service'] = @{ Mode = 0x81A4; Data = (& $readScript (Join-Path $InstallerDir 'filebox-firstboot.service')) }
    $files['filebox-update'] = @{ Mode = 0x81ED; Data = (& $readScript $UpdateScriptPath) }

    return $files
}

function Get-WifiFirmwarePackage {
    param([Parameter(Mandatory)][string]$PnpDeviceId)

    $pci = @{ '8086' = 'firmware-iwlwifi'; '10EC' = 'firmware-realtek'; '168C' = 'firmware-atheros'; '14E4' = 'firmware-brcm80211'; '14C3' = 'firmware-mediatek'; '1814' = 'firmware-misc-nonfree' }
    $usb = @{ '8087' = 'firmware-iwlwifi'; '0BDA' = 'firmware-realtek'; '0CF3' = 'firmware-atheros'; '0E8D' = 'firmware-mediatek'; '148F' = 'firmware-misc-nonfree' }
    if ($PnpDeviceId -match '^PCI\\VEN_([0-9A-Fa-f]{4})') { return $pci[$Matches[1].ToUpper()] }
    if ($PnpDeviceId -match '^USB\\VID_([0-9A-Fa-f]{4})') { return $usb[$Matches[1].ToUpper()] }

    return $null
}

$script:TarExe = Join-Path $env:SystemRoot 'System32\tar.exe'

function Save-DebDataArchive {
    param([Parameter(Mandatory)][string]$DebPath, [Parameter(Mandatory)][string]$OutDir)

    $bytes = [System.IO.File]::ReadAllBytes($DebPath)
    $ascii = [System.Text.Encoding]::ASCII
    if ($ascii.GetString($bytes, 0, 8) -ne "!<arch>`n") { throw "$DebPath is not a .deb archive" }
    $offset = 8
    while ($offset + 60 -le $bytes.Length) {
        $name = $ascii.GetString($bytes, $offset, 16).Trim()
        $size = [int64]$ascii.GetString($bytes, $offset + 48, 10).Trim()
        $offset += 60
        if ($name -like 'data.tar*') {
            $target = Join-Path $OutDir ($name.TrimEnd('/'))
            $slice = New-Object byte[] $size
            [Array]::Copy($bytes, $offset, $slice, 0, $size)
            [System.IO.File]::WriteAllBytes($target, $slice)

            return $target
        }
        $offset += $size + ($size % 2)
    }

    throw "No data archive inside $DebPath"
}

function Get-FirmwareFileSet {
    param(
        [Parameter(Mandatory)][string[]]$Packages,
        [Parameter(Mandatory)][string]$WorkDir,
        [string]$Mirror = 'https://deb.debian.org/debian',
        [string]$Suite = 'trixie'
    )

    New-Item -ItemType Directory -Force $WorkDir | Out-Null
    $client = New-Object System.Net.WebClient
    try {
        $indexGz = Join-Path $WorkDir 'Packages.gz'
        $client.DownloadFile("$Mirror/dists/$Suite/non-free-firmware/binary-amd64/Packages.gz", $indexGz)
        $in = [System.IO.File]::OpenRead($indexGz)
        $gz = New-Object System.IO.Compression.GZipStream($in, [System.IO.Compression.CompressionMode]::Decompress)
        $reader = New-Object System.IO.StreamReader($gz)
        try { $index = $reader.ReadToEnd() } finally { $reader.Dispose(); $in.Dispose() }

        $files = [ordered]@{}
        foreach ($package in $Packages) {
            $stanza = ($index -split "`n`n") | Where-Object { $_ -match "(?m)^Package: $([regex]::Escape($package))$" } | Select-Object -First 1
            if (-not $stanza -or $stanza -notmatch '(?m)^Filename: (\S+)$') { throw "Firmware package $package not found in Debian $Suite" }
            $filename = $Matches[1]
            if ($stanza -notmatch '(?m)^SHA256: ([0-9a-f]{64})$') { throw "No checksum for $package" }
            $expected = $Matches[1]
            $deb = Join-Path $WorkDir ([System.IO.Path]::GetFileName($filename))
            $client.DownloadFile("$Mirror/$filename", $deb)
            if ((Get-FileHash -Algorithm SHA256 $deb).Hash.ToLower() -ne $expected) { throw "Checksum mismatch for $package" }

            $dir = Join-Path $WorkDir $package
            Remove-Item -Recurse -Force $dir -ErrorAction SilentlyContinue
            New-Item -ItemType Directory -Force $dir | Out-Null
            $data = Save-DebDataArchive -DebPath $deb -OutDir $dir
            & $script:TarExe -xf $data -C $dir
            $fw = Join-Path $dir 'usr\lib\firmware'
            if (-not (Test-Path $fw)) { $fw = Join-Path $dir 'lib\firmware' }
            if (-not (Test-Path $fw)) { throw "No firmware files in $package" }
            foreach ($file in Get-ChildItem $fw -Recurse -File) {
                $relative = $file.FullName.Substring($fw.Length + 1).Replace('\', '/')
                $files["lib/firmware/$relative"] = @{ Mode = 0x81A4; Data = [System.IO.File]::ReadAllBytes($file.FullName) }
            }
        }
    } finally { $client.Dispose() }

    return $files
}

function New-GrubConfig {
    param(
        [Parameter(Mandatory)][string]$KernelPath,
        [Parameter(Mandatory)][string]$InitrdPath,
        [Parameter(Mandatory)][string]$Hostname
    )

    return @"
set timeout=5
set default=0
menuentry "filebox unattended install (erases the chosen partition)" {
    linux $KernelPath auto=true priority=critical preseed/file=/preseed.cfg hostname=$Hostname domain= --- quiet
    initrd $InitrdPath
}
"@ -replace "`r`n", "`n"
}

Export-ModuleMember -Function New-Sha512Crypt, Assert-InstallSettings, New-PreseedText, New-ConfText, New-CpioArchive, New-PayloadInitrd, New-InstallerFileSet, New-GrubConfig, ConvertTo-ShellLiteral, Get-WifiFirmwarePackage, Get-FirmwareFileSet
