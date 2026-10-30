#Requires -Version 5.1
<#
.SYNOPSIS
  Wizard that turns an old Windows PC into a filebox server by booting an unattended Debian install.

.DESCRIPTION
  Run in an elevated PowerShell on the old PC. It looks at your disks, asks which partition Debian
  replaces and which one holds your data, downloads and checks the Debian network installer, stages it
  on a small new partition, adds a one-time boot entry and (after you type a confirmation) reboots.

  -DryRun    shows every step and changes nothing on the disks or in the boot configuration.
  -Rollback  removes the boot entry and the staging partition again (before the install has run).
  -Answers   pre-filled answers for the prompts, for testing.

  Needs UEFI. Legacy BIOS machines should use the USB method in docs/index.html.
  The partition you pick is ERASED by the install. Back up first.
#>
param(
    [switch]$DryRun,
    [switch]$Rollback,
    [string]$WorkDir = (Join-Path $env:ProgramData 'filebox-installer'),
    [string[]]$Answers = @()
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'
Import-Module (Join-Path $PSScriptRoot 'FileboxInstaller.psm1') -Force

$NetbootBase = 'https://deb.debian.org/debian/dists/trixie/main/installer-amd64/current/images'
$ReleaseBase = 'https://github.com/Ezomic/filebox/releases'
$EspGuid = '{c12a7328-f81f-11d2-ba4b-00a0c93ec93b}'
$StagingMB = 300
$StatePath = Join-Path $WorkDir 'state.json'
$script:queue = New-Object System.Collections.Generic.Queue[string]
foreach ($a in $Answers) { $script:queue.Enqueue($a) }

function Say([string]$text) { Write-Host $text }
function Step([string]$text) { Write-Host ''; Write-Host "== $text" -ForegroundColor Cyan }
function Stop-Wizard([string]$text) { Write-Host ''; Write-Host "Stopped: $text" -ForegroundColor Red; exit 1 }

function Ask {
    param([string]$Prompt, [string]$Default = '')
    if ($script:queue.Count -gt 0) {
        $value = $script:queue.Dequeue()
        Write-Host "$Prompt $value (preset)"
    } else {
        $suffix = ''
        if ($Default) { $suffix = " [$Default]" }
        $value = Read-Host "$Prompt$suffix"
    }
    if ([string]::IsNullOrWhiteSpace($value)) { return $Default }

    return $value.Trim()
}

function Ask-Secret {
    param([string]$Prompt)
    if ($script:queue.Count -gt 0) { return $script:queue.Dequeue() }
    $secure = Read-Host $Prompt -AsSecureString
    $ptr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
    try { return [Runtime.InteropServices.Marshal]::PtrToStringBSTR($ptr) } finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($ptr) }
}

function Test-Administrator {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    return (New-Object Security.Principal.WindowsPrincipal($id)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Get-PartUuid($disk, $partition) {
    if ($disk.PartitionStyle -eq 'GPT') { return $partition.Guid.Trim('{}').ToLower() }

    return ('{0:x8}-{1:d2}' -f [uint32]$disk.Signature, $partition.PartitionNumber)
}

function Get-Layout {
    $rows = @()
    foreach ($disk in Get-Disk) {
        if ($disk.BusType -eq 'USB') { continue }
        foreach ($p in Get-Partition -DiskNumber $disk.Number) {
            $fs = ''
            $label = ''
            $volume = $p | Get-Volume -ErrorAction SilentlyContinue
            if ($volume) { $fs = [string]$volume.FileSystem; $label = [string]$volume.FileSystemLabel }
            $kind = 'other'
            if ($disk.PartitionStyle -eq 'GPT') {
                if ($p.GptType -eq $EspGuid) { $kind = 'esp' }
                elseif ($p.GptType -eq '{e3c9e316-0b5c-4db8-817d-f92df00215ae}') { $kind = 'msr' }
                elseif ($p.GptType -eq '{de94bba4-06d1-4d40-a16a-bfd50179d6ac}') { $kind = 'recovery' }
                elseif ($p.GptType -eq '{0fc63daf-8483-4772-8e79-3d69d8477de4}') { $kind = 'linux' }
            } elseif ($p.MbrType -eq 131) { $kind = 'linux' }
            $rows += [pscustomobject]@{
                Disk = $disk.Number; Number = $p.PartitionNumber; SizeGB = [math]::Round($p.Size / 1GB, 1); Size = $p.Size
                FileSystem = $fs; Label = $label; Kind = $kind; Letter = [string]$p.DriveLetter
                PartUuid = (Get-PartUuid $disk $p); IsSystem = [bool]($p.IsSystem -or $p.IsBoot)
                Style = [string]$disk.PartitionStyle
            }
        }
    }

    return $rows
}

function Show-Layout($rows) {
    $rows | Format-Table -AutoSize @{n = 'Disk'; e = { $_.Disk } }, @{n = 'Part'; e = { $_.Number } }, @{n = 'GB'; e = { $_.SizeGB } },
        @{n = 'Filesystem'; e = { $_.FileSystem } }, @{n = 'Label'; e = { $_.Label } }, @{n = 'Drive'; e = { $_.Letter } }, @{n = 'Type'; e = { $_.Kind } } | Out-String | Write-Host
}

function Get-Download {
    param([string]$Url, [string]$Path)
    if (Test-Path $Path) { return }
    Say "Downloading $Url"
    $client = New-Object System.Net.WebClient
    try { $client.DownloadFile($Url, $Path) } finally { $client.Dispose() }
}

function Get-Sha256([string]$Path) { return (Get-FileHash -Algorithm SHA256 -Path $Path).Hash.ToLower() }

function Save-Boot {
    param($State)
    New-Item -ItemType Directory -Force $WorkDir | Out-Null
    ($State | ConvertTo-Json) | Set-Content -Path $StatePath -Encoding UTF8
}

function Invoke-Rollback {
    Step 'Rollback'
    if (-not (Test-Path $StatePath)) { Stop-Wizard "Nothing to roll back: $StatePath does not exist." }
    $state = Get-Content $StatePath -Raw | ConvertFrom-Json
    if ($state.BootEntry) {
        Say "Removing boot entry $($state.BootEntry)"
        if (-not $DryRun) { & bcdedit /delete $state.BootEntry | Out-Null }
    }
    if ($state.StagingDisk -ne $null -and $state.StagingPartition -ne $null) {
        Say "Removing staging partition $($state.StagingPartition) on disk $($state.StagingDisk)"
        if (-not $DryRun) { Remove-Partition -DiskNumber $state.StagingDisk -PartitionNumber $state.StagingPartition -Confirm:$false }
    }
    if ($state.TargetDisk -ne $null -and $state.TargetPartition -ne $null) {
        Say 'Growing the Windows partition back to full size'
        if (-not $DryRun) {
            $max = (Get-PartitionSupportedSize -DiskNumber $state.TargetDisk -PartitionNumber $state.TargetPartition).SizeMax
            Resize-Partition -DiskNumber $state.TargetDisk -PartitionNumber $state.TargetPartition -Size $max
        }
    }
    if (-not $DryRun) { Remove-Item $StatePath }
    Say 'Rolled back. Windows is as it was.'
    exit 0
}

# ---------------------------------------------------------------------------------------------------

Say 'filebox install wizard'
if ($DryRun) { Say '(dry run: nothing on the disks or in the boot configuration will be changed)' }
if (-not (Test-Administrator)) { Stop-Wizard 'Run this in PowerShell as Administrator.' }
if ($Rollback) { Invoke-Rollback }
if (Test-Path $StatePath) { Stop-Wizard "A previous run left $StatePath. Run with -Rollback first." }

Step '1. Checking this PC'
$bootLoader = (& bcdedit /enum '{current}') -join ' '
if ($bootLoader -notmatch 'winload\.efi') { Stop-Wizard 'This PC boots in legacy BIOS mode. The wizard needs UEFI; use the USB method in docs/index.html.' }
Say 'UEFI boot: yes'
try { Say "Secure Boot: $(Confirm-SecureBootUEFI)" } catch { Say 'Secure Boot: unknown' }
$wired = Get-NetAdapter -Physical | Where-Object { $_.Status -eq 'Up' -and $_.PhysicalMediaType -match '802.3' }
if (-not $wired) { Stop-Wizard 'No connected wired (Ethernet) adapter found. The installer downloads Debian over the network and cannot use Wi-Fi. Plug in a cable.' }
Say 'Wired network: yes'

Step '2. Your disks'
$layout = Get-Layout
Show-Layout $layout

$candidates = @($layout | Where-Object { $_.FileSystem -eq 'NTFS' -and $_.Kind -eq 'other' })
if (-not $candidates) { Stop-Wizard 'Found no NTFS partition that Debian could replace.' }
$targetNo = Ask 'Number (Part) of the partition Debian will REPLACE (this erases it, usually the Windows C: partition)'
$disks = @($layout | Where-Object { $_.Number -eq [int]$targetNo -and $_.FileSystem -eq 'NTFS' -and $_.Kind -eq 'other' })
if ($disks.Count -ne 1) {
    $diskNo = Ask 'That number exists on several disks, which disk number'
    $disks = @($layout | Where-Object { $_.Disk -eq [int]$diskNo -and $_.Number -eq [int]$targetNo -and $_.FileSystem -eq 'NTFS' -and $_.Kind -eq 'other' })
}
if ($disks.Count -ne 1) { Stop-Wizard 'That is not an NTFS data or Windows partition.' }
$target = $disks[0]
if ($target.Letter) {
    $bitlocker = Get-BitLockerVolume -MountPoint "$($target.Letter):" -ErrorAction SilentlyContinue
    if ($bitlocker -and $bitlocker.ProtectionStatus -eq 'On') { Stop-Wizard 'BitLocker is on for that partition. Turn it off (and wait for decryption) first, because shrinking an encrypted partition is unreliable.' }
}

$dataAction = 'none'
$data = $null
$dataChoice = Ask 'Number (Part) of the partition that holds your DATA, or none' 'none'
if ($dataChoice -ne 'none') {
    $match = @($layout | Where-Object { $_.Disk -eq $target.Disk -and $_.Number -eq [int]$dataChoice })
    if ($match.Count -ne 1) { Stop-Wizard 'The data partition must be on the same disk as the one being replaced.' }
    $data = $match[0]
    if ($data.PartUuid -eq $target.PartUuid) { Stop-Wizard 'The data partition cannot be the partition that gets replaced.' }
    if ($data.Kind -in @('esp', 'msr', 'recovery')) { Stop-Wizard 'That is a system partition, not a data partition.' }
    if ($data.Kind -eq 'linux') {
        $dataAction = 'keep'
        Say 'Data partition looks like a Linux partition: it will be kept as it is and mounted at /srv/sftp (if it is ext4, xfs or btrfs).'
    } else {
        Say "The data partition is $($data.FileSystem). filebox cannot use that: it needs ext4 for permissions."
        Say 'Copy the data to another drive first. The install can then FORMAT this partition as ext4 (erasing it).'
        $dataAction = 'format'
    }
}

Step '3. Your server'
$hostname = Ask 'Server name' 'filebox'
$defaultUser = ($env:USERNAME.ToLower() -replace '[^a-z0-9_-]', '')
if ($defaultUser -notmatch '^[a-z]') { $defaultUser = 'admin' }
if ($defaultUser -in @('root', 'filebox')) { $defaultUser = 'admin' }
$user = Ask 'Login name for the server' $defaultUser
$password = Ask-Secret 'Password for that login (at least 12 characters, used at the console and for sudo)'
if ($password.Length -lt 12) { Stop-Wizard 'Password is shorter than 12 characters.' }
if ((Ask-Secret 'Type the password again') -ne $password) { Stop-Wizard 'The passwords differ.' }
$keys = @(Get-ChildItem (Join-Path $env:USERPROFILE '.ssh') -Filter '*.pub' -ErrorAction SilentlyContinue)
$sshKey = ''
if ($keys) {
    Say 'SSH public keys on this PC:'
    for ($i = 0; $i -lt $keys.Count; $i++) { Say "  [$i] $($keys[$i].Name)" }
    $pick = Ask 'Number of the key to use, or paste a public key line' '0'
    if ($pick -match '^\d+$' -and [int]$pick -lt $keys.Count) { $sshKey = (Get-Content $keys[[int]$pick].FullName -Raw).Trim() } else { $sshKey = $pick }
} else {
    $sshKey = Ask 'Paste your SSH public key (one line, starts with ssh-ed25519 or ssh-rsa). Make one with: ssh-keygen -t ed25519'
}
$timezone = Ask 'Time zone' 'Europe/Amsterdam'
$keymap = Ask 'Keyboard layout (us, gb, nl ...)' 'us'

$settings = @{
    Hostname = $hostname; User = $user; Timezone = $timezone; Keymap = $keymap
    TargetPartUuid = $target.PartUuid; TargetFsType = 'ntfs'
    DataAction = $dataAction; SshKey = $sshKey
    PasswordHash = (New-Sha512Crypt -Password $password)
}
$password = $null
if ($data) { $settings.DataPartUuid = $data.PartUuid }
try { Assert-InstallSettings $settings } catch { Stop-Wizard $_.Exception.Message }

Step '4. Review'
Say "Debian will REPLACE : disk $($target.Disk) partition $($target.Number) ($($target.SizeGB) GB, label '$($target.Label)', drive $($target.Letter):)"
if ($data) { Say "Data partition      : partition $($data.Number) ($($data.SizeGB) GB) action: $dataAction" } else { Say 'Data partition      : none (files will live on the new system partition)' }
Say "Server             : $hostname, login $user, time zone $timezone, keyboard $keymap"
Say "Windows           : will be gone afterwards. Everything on partition $($target.Number) is erased."
if ($dataAction -eq 'format') { Say "The data partition $($data.Number) will ALSO be erased and formatted." }
if ((Ask 'Have you copied everything you want to keep somewhere else? (yes/no)' 'no') -ne 'yes') { Stop-Wizard 'Back up first, then run the wizard again.' }

Step '5. Downloading the installer'
New-Item -ItemType Directory -Force $WorkDir | Out-Null
$netboot = Join-Path $WorkDir 'netboot.tar.gz'
Get-Download "$NetbootBase/netboot/netboot.tar.gz" $netboot
Get-Download "$NetbootBase/SHA256SUMS" (Join-Path $WorkDir 'SHA256SUMS')
$expected = ((Get-Content (Join-Path $WorkDir 'SHA256SUMS')) | Where-Object { $_ -match 'netboot/netboot\.tar\.gz$' } | ForEach-Object { ($_ -split '\s+')[0] } | Select-Object -First 1)
if (-not $expected -or (Get-Sha256 $netboot) -ne $expected.ToLower()) { Remove-Item $netboot -ErrorAction SilentlyContinue; Stop-Wizard 'The Debian installer download does not match its checksum.' }
Say 'Debian installer checksum OK'
$resolve = [System.Net.HttpWebRequest]::Create("$ReleaseBase/latest")
$resolve.AllowAutoRedirect = $false
$response = $resolve.GetResponse()
$tag = ($response.Headers['Location'] -split '/')[-1]
$response.Close()
if ($tag -notmatch '^v\d+\.\d+\.\d+$') { Stop-Wizard "Unexpected filebox release name: $tag" }
$bundle = Join-Path $WorkDir 'deploy.tar.gz'
Get-Download "$ReleaseBase/download/$tag/deploy.tar.gz" $bundle
Get-Download "$ReleaseBase/download/$tag/deploy.tar.gz.sha256" "$bundle.sha256"
if ((Get-Sha256 $bundle) -ne ((Get-Content "$bundle.sha256" -Raw) -split '\s+')[0].ToLower()) { Remove-Item $bundle -ErrorAction SilentlyContinue; Stop-Wizard 'The filebox bundle does not match its checksum.' }
Say "filebox $tag checksum OK"
$x = Join-Path $WorkDir 'x'
Remove-Item -Recurse -Force $x -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force "$x\netboot", "$x\deploy" | Out-Null
& tar.exe -xzf $netboot -C "$x\netboot"
& tar.exe -xzf $bundle -C "$x\deploy"
$net = "$x\netboot\debian-installer\amd64"
if (-not (Test-Path "$net\initrd.gz")) { Stop-Wizard 'The Debian installer archive has an unexpected layout.' }

Step '6. Building the installer files'
$settingsForBuild = $settings.Clone()
$files = New-InstallerFileSet -Settings $settingsForBuild -InstallerDir $PSScriptRoot -UpdateScriptPath "$x\deploy\deploy\filebox-update"
New-PayloadInitrd -InitrdPath "$net\initrd.gz" -OutPath (Join-Path $WorkDir 'initrd.gz') -Files $files
Say 'Installer image built'

Step '7. Staging and boot entry'
$plan = @(
    "shrink Windows partition $($target.Number) by $StagingMB MB",
    "create a $StagingMB MB FAT32 staging partition (type EFI System) in the freed space and copy the installer to it",
    'add a one-time boot entry "filebox installer"',
    'reboot into the unattended install'
)
$plan | ForEach-Object { Say "  - $_" }
if ($DryRun) { Say ''; Say "Dry run finished. Nothing was changed. The built installer image is in $WorkDir"; exit 0 }

$typed = Ask "Type ERASE $($target.Number) to start (this will erase partition $($target.Number) on the next reboot)"
if ($typed -ne "ERASE $($target.Number)") { Stop-Wizard 'Confirmation not given, nothing was changed.' }

$state = @{ TargetDisk = $target.Disk; TargetPartition = $target.Number }
Save-Boot $state
$size = (Get-PartitionSupportedSize -DiskNumber $target.Disk -PartitionNumber $target.Number)
$newSize = [int64](Get-Partition -DiskNumber $target.Disk -PartitionNumber $target.Number).Size - ([int64]$StagingMB * 1MB)
if ($newSize -lt $size.SizeMin) { Stop-Wizard "Windows cannot be shrunk by $StagingMB MB (its used space is too large or fragmented). Free some space or defragment, then run again. Nothing was changed." }
Resize-Partition -DiskNumber $target.Disk -PartitionNumber $target.Number -Size $newSize
$stage = New-Partition -DiskNumber $target.Disk -Size ([int64]$StagingMB * 1MB) -GptType $EspGuid -AssignDriveLetter
$state.StagingDisk = $target.Disk
$state.StagingPartition = $stage.PartitionNumber
Save-Boot $state
Format-Volume -Partition $stage -FileSystem FAT32 -NewFileSystemLabel 'FBINSTALL' -Confirm:$false | Out-Null
$root = "$($stage.DriveLetter):"
New-Item -ItemType Directory -Force "$root\debian-installer\amd64\grub", "$root\EFI\BOOT" | Out-Null
Copy-Item "$net\bootnetx64.efi" "$root\EFI\BOOT\BOOTX64.EFI"
Copy-Item "$net\grubx64.efi" "$root\EFI\BOOT\grubx64.efi"
Copy-Item "$net\linux" "$root\debian-installer\amd64\linux"
Copy-Item (Join-Path $WorkDir 'initrd.gz') "$root\debian-installer\amd64\initrd.gz"
[System.IO.File]::WriteAllText("$root\debian-installer\amd64\grub\grub.cfg", (New-GrubConfig -KernelPath '/debian-installer/amd64/linux' -InitrdPath '/debian-installer/amd64/initrd.gz' -Hostname $hostname))

# The staging partition's PARTUUID is only known now; the install removes it afterwards.
$stageUuid = (Get-PartUuid (Get-Disk -Number $target.Disk) (Get-Partition -DiskNumber $target.Disk -PartitionNumber $stage.PartitionNumber))
$settings.CleanupPartUuid = $stageUuid
$files = New-InstallerFileSet -Settings $settings -InstallerDir $PSScriptRoot -UpdateScriptPath "$x\deploy\deploy\filebox-update"
New-PayloadInitrd -InitrdPath "$net\initrd.gz" -OutPath "$root\debian-installer\amd64\initrd.gz" -Files $files

$copy = (& bcdedit /copy '{bootmgr}' /d 'filebox installer') -join ' '
if ($copy -notmatch '\{[0-9a-fA-F-]{36}\}') { Stop-Wizard "Could not create the boot entry: $copy" }
$entry = $Matches[0]
$state.BootEntry = $entry
Save-Boot $state
& bcdedit /set $entry device "partition=$root" | Out-Null
& bcdedit /set $entry path '\EFI\BOOT\BOOTX64.EFI' | Out-Null
& bcdedit /set '{fwbootmgr}' displayorder $entry /addfirst | Out-Null
& bcdedit /set '{fwbootmgr}' bootsequence $entry | Out-Null
Say "Boot entry $entry added (used once, on the next boot only)."

Step '8. Reboot'
Say 'The PC will restart in 60 seconds and install Debian by itself (about 10 minutes, then it reboots into the new server).'
Say 'To cancel: shutdown /a   then run this wizard with -Rollback.'
shutdown.exe /r /t 60 /c 'filebox installer: the next boot erases the chosen partition'
