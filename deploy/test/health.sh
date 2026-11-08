#!/usr/bin/env bash
# Runs deploy/filebox-health in a sandbox: the real shell logic, with smartctl, btrfs, stat, findmnt and the ownership
# commands stubbed, and /sys/fs/btrfs a plain folder. Files in the state folder decide what the stubs answer.
# Run from anywhere: deploy/test/health.sh (needs jq, like the box).
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEPLOY=$(dirname "$HERE")
T=$(mktemp -d)
trap '/bin/rm -rf "$T"' EXIT
BASE=$T/srv/sftp
SYSFS=$T/sys/fs/btrfs
STATUS_DIR=$T/var/lib/filebox-health
STATUS=$STATUS_DIR/status
STATE=$T/state
UUID=0f1e2d3c-4b5a-4968-8776-a5b4c3d2e1f0
DEVINFO=$SYSFS/$UUID/devinfo
CHECKED=1794000000
mkdir -p "$T/stubs" "$T/bin" "$STATE" "$BASE" "$T/var/lib"
: >"$T/calls.log"

command -v jq >/dev/null || {
	echo "jq is needed"
	exit 1
}
# Only the stubs and the tools the helper needs are on PATH, so a missing stub means a missing command.
for tool in awk sed head grep cat date jq mktemp chmod mv rm; do
	ln -s "$(command -v "$tool")" "$T/bin/$tool"
done
sed -e "s#^export PATH=.*#export PATH=\"$T/stubs:$T/bin\"#" \
	-e "s#/srv/sftp#$BASE#g" \
	-e "s#/sys/fs/btrfs#$SYSFS#g" \
	-e "s#/var/lib/filebox-health#$STATUS_DIR#g" "$DEPLOY/filebox-health" >"$T/filebox-health"
chmod +x "$T/filebox-health"

cat >"$T/stubs/stat" <<EOF
#!/bin/bash
[ "\$*" = "-f -c %T -- $BASE" ] && cat "$STATE/fstype"
EOF
# The panel's sandbox bind-mounts /srv/sftp onto itself, so mountpoint always says yes there. The helper must not ask.
cat >"$T/stubs/mountpoint" <<'EOF'
#!/bin/bash
exit 0
EOF
cat >"$T/stubs/findmnt" <<EOF
#!/bin/bash
case "\$*" in
"--fstab -n -o FSTYPE $BASE") [ -e "$STATE/fstab" ] && cat "$STATE/fstab" ;;
"-n -o UUID --target $BASE") [ -e "$STATE/uuid" ] && cat "$STATE/uuid" ;;
*) exit 1 ;;
esac
EOF
cat >"$T/stubs/btrfs" <<EOF
#!/bin/bash
[ "\$*" = "scrub status $BASE" ] || { echo "unexpected: btrfs \$*" >&2; exit 1; }
[ -e "$STATE/scrub_fail" ] && exit 1
cat "$STATE/scrub"
EOF
cat >"$T/stubs/smartctl" <<EOF
#!/bin/bash
echo "smartctl \$*" >>"$T/calls.log"
case "\$*" in
"--scan -j") cat "$STATE/scan" ;;
"-j -H -A -n standby "*)
	disk=$STATE/smart\${*: -1}
	cat "\$disk" 2>/dev/null
	exit "\$(cat "\$disk.exit" 2>/dev/null || echo 0)"
	;;
*) exit 1 ;;
esac
EOF
cat >"$T/stubs/date" <<EOF
#!/bin/bash
[ "\$*" = +%s ] && echo $CHECKED && exit 0
exec "$T/bin/date" "\$@"
EOF
cat >"$T/stubs/id" <<'EOF'
#!/bin/bash
echo 0
EOF
# install and chown record the owner and group they were asked for, then do without them.
cat >"$T/stubs/install" <<EOF
#!/bin/bash
echo "install \$*" >>"$T/calls.log"
args=()
while [ \$# -gt 0 ]; do
	case \$1 in -o | -g) shift 2 ;; *) args+=("\$1"); shift ;; esac
done
exec /usr/bin/install "\${args[@]}"
EOF
cat >"$T/stubs/chown" <<EOF
#!/bin/bash
echo "chown \$*" >>"$T/calls.log"
[ ! -e "$STATE/chown_fail" ]
EOF
chmod +x "$T"/stubs/*

fails=0
check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fails=$((fails + 1)); fi; }
health() { "$T/filebox-health" "$@" 2>&1; }
expect() { [ "$(cat "$STATUS")" = "$(printf '%s\n' "$@")" ]; }

disk() { # disk <devid> <missing 0|1> <write> <read> <flush>
	mkdir -p "$DEVINFO/$1"
	echo "$2" >"$DEVINFO/$1/missing"
	printf 'write_errs %s\nread_errs %s\nflush_errs %s\ncorruption_errs 9\ngeneration_errs 9\n' "$3" "$4" "$5" >"$DEVINFO/$1/error_stats"
}
scrubbed() { # scrubbed <status> <error summary> [uncorrectable]
	{
		echo "UUID:             $UUID"
		echo "Scrub started:    Sun Nov  1 03:00:01 2026"
		echo "Status:           $1"
		echo "Duration:         0:42:13"
		echo "Total to scrub:   812.53GiB"
		echo "Rate:             328.42MiB/s"
		echo "Error summary:    $2"
		if [ $# -eq 3 ]; then
			echo "  Corrected:      0"
			echo "  Uncorrectable:  $3"
			echo "  Unverified:     0"
		fi
	} >"$STATE/scrub"
}
STARTED=$(date -d "Sun Nov  1 03:00:01 2026" +%s)

mkdir -p "$STATE/smart/dev"
cat >"$STATE/scan" <<'EOF'
{
  "devices": [
    { "name": "/dev/sda", "info_name": "/dev/sda [SAT]", "type": "sat", "protocol": "ATA" },
    { "name": "/dev/sdb", "info_name": "/dev/sdb [SAT]", "type": "sat", "protocol": "ATA" },
    { "name": "/dev/bus/0", "info_name": "/dev/bus/0 [megaraid_disk_00]", "type": "megaraid,0", "protocol": "SCSI" }
  ]
}
EOF
cat >"$STATE/smart/dev/sda" <<'EOF'
{
  "device": { "name": "/dev/sda", "type": "sat" },
  "model_name": "WDC WD5000AAKX-00ERMA0",
  "serial_number": "WD-WCC2EJ123456",
  "smart_status": { "passed": true },
  "ata_smart_attributes": { "table": [ { "id": 5, "name": "Reallocated_Sector_Ct", "raw": { "value": 0, "string": "0" } } ] },
  "temperature": { "current": 34 }
}
EOF
cat >"$STATE/smart/dev/sdb" <<'EOF'
{
  "device": { "name": "/dev/sdb", "type": "sat" },
  "model_name": "ST1000DM003-1CH162",
  "serial_number": "Z1D2ABCD",
  "smart_status": { "passed": false }
}
EOF
echo 8 >"$STATE/smart/dev/sdb.exit"
SDA=$(jq -c . "$STATE/smart/dev/sda")
SDB=$(jq -c . "$STATE/smart/dev/sdb")

echo btrfs >"$STATE/fstype"
echo btrfs >"$STATE/fstab"
echo "$UUID" >"$STATE/uuid"
disk 1 0 0 0 0
disk 2 0 1 2 0
scrubbed finished "no errors found"
on() { expect "filebox-health 1" "checked $CHECKED" "smart $SDA" "smart $SDB" "state on" "$@"; }
clean_scrub="scrub finished $STARTED 0 0"

echo "--- a healthy mirror"
out=$(health)
check "writes the disks, the mirror and a clean scrub" 'on "device 1 ok 0" "device 2 ok 3" "$clean_scrub"'
check "a failing disk is reported although smartctl exits with an error bit" 'grep -q "Z1D2ABCD" "$STATUS"'
check "only real disks are asked, never woken" '! grep -q "/dev/bus/0" "$T/calls.log" && grep -q "smartctl -j -H -A -n standby /dev/sda" "$T/calls.log"'
check "the folder belongs to root and the panel group may read it" 'grep -q "install -d -o root -g filebox -m 0750 $STATUS_DIR" "$T/calls.log" && [ "$(/usr/bin/stat -c %a "$STATUS_DIR")" = 750 ]'
check "the file is root:filebox 0640" 'grep -q "chown root:filebox $STATUS." "$T/calls.log" && [ "$(/usr/bin/stat -c %a "$STATUS")" = 640 ]'
check "no temporary file is left" '[ "$(ls "$STATUS_DIR")" = status ]'

echo "--- disks"
echo 2 >"$STATE/smart/dev/sda.exit"
printf '{\n  "device": { "name": "/dev/sda" },\n  "power_mode": "STANDBY"\n}\n' >"$STATE/smart/dev/sda"
out=$(health)
check "a sleeping disk is listed with what little it says" 'grep -qx "smart {\"device\":{\"name\":\"/dev/sda\"},\"power_mode\":\"STANDBY\"}" "$STATUS"'
mv "$T/stubs/smartctl" "$T/smartctl.aside"
out=$(health)
check "without smartctl the panel hears so" 'expect "filebox-health 1" "checked $CHECKED" "smart unavailable" "state on" "device 1 ok 0" "device 2 ok 3" "$clean_scrub"'
mv "$T/smartctl.aside" "$T/stubs/smartctl"
printf '%s\n' "$SDA" | jq . >"$STATE/smart/dev/sda"
echo 0 >"$STATE/smart/dev/sda.exit"

echo "--- a mirror running without one of its disks"
disk 2 1 0 0 0
out=$(health)
check "only sysfs decides" 'on "device 1 ok 0" "device 2 missing 0" "$clean_scrub"'
disk 2 0 1 2 0

echo "--- disks that cannot be read"
echo "?" >"$DEVINFO/2/missing"
out=$(health)
check "a strange missing flag makes the whole list unknown" 'on "devices unknown" "$clean_scrub"'
echo 0 >"$DEVINFO/2/missing"
/bin/rm "$DEVINFO/1/error_stats"
out=$(health)
check "a kernel without error counts says so" 'on "device 1 ok -" "device 2 ok 3" "$clean_scrub"'
disk 1 0 0 0 0
/bin/rm "$STATE/uuid"
out=$(health)
check "no uuid from findmnt is unknown" 'on "devices unknown" "$clean_scrub"'
echo "$UUID" >"$STATE/uuid"
mv "$DEVINFO" "$T/devinfo.aside"
out=$(health)
check "no devinfo in sysfs is unknown" 'on "devices unknown" "$clean_scrub"'
mv "$T/devinfo.aside" "$DEVINFO"

echo "--- what the scrub found"
scrubbed finished "read=1 csum=3" 1
out=$(health)
check "errors are added up, with the ones that could not be repaired" 'on "device 1 ok 0" "device 2 ok 3" "scrub finished $STARTED 4 1"'
scrubbed aborted "csum=12" 0
out=$(health)
check "an aborted scrub still reports what it found" 'on "device 1 ok 0" "device 2 ok 3" "scrub aborted $STARTED 12 0"'
scrubbed running "no errors found"
sed -i 's/^Scrub started:/Scrub resumed:/' "$STATE/scrub"
out=$(health)
check "a resumed scrub has a start too" 'on "device 1 ok 0" "device 2 ok 3" "scrub running $STARTED 0 0"'
printf 'UUID:             %s\n\tno stats available\n' "$UUID" >"$STATE/scrub"
out=$(health)
check "no scrub yet" 'on "device 1 ok 0" "device 2 ok 3" "scrub none - 0 0"'
printf 'UUID:             %s\nStatus:           confused\n' "$UUID" >"$STATE/scrub"
out=$(health)
check "a status it does not know is unknown" 'on "device 1 ok 0" "device 2 ok 3" "scrub unknown"'
touch "$STATE/scrub_fail"
out=$(health)
check "a scrub status that fails is unknown" 'on "device 1 ok 0" "device 2 ok 3" "scrub unknown"'
/bin/rm "$STATE/scrub_fail"
scrubbed finished "no errors found"

echo "--- not mounted, or not applicable"
echo ext4 >"$STATE/fstype"
out=$(health)
check "listed as btrfs but not on btrfs is not mounted, though mountpoint would say yes" 'on_unmounted=$(printf "%s\n" "filebox-health 1" "checked $CHECKED" "smart $SDA" "smart $SDB" "state unmounted"); [ "$(cat "$STATUS")" = "$on_unmounted" ]'
echo ext4 >"$STATE/fstab"
out=$(health)
check "an ext4 data disk is not applicable" 'grep -qx "state off not_btrfs" "$STATUS" && ! grep -q "^device\|^scrub" "$STATUS"'
/bin/rm "$STATE/fstab"
out=$(health)
check "files on the system disk are not applicable" 'grep -qx "state off not_btrfs" "$STATUS"'
echo btrfs >"$STATE/fstab"
echo btrfs >"$STATE/fstype"
mv "$T/stubs/btrfs" "$T/btrfs.aside"
out=$(health)
check "btrfs without btrfs-progs says so" 'grep -qx "state off no_btrfs_progs" "$STATUS"'
mv "$T/btrfs.aside" "$T/stubs/btrfs"

echo "--- writing"
out=$(health)
cp "$STATUS" "$T/good"
touch "$STATE/chown_fail"
out=$(health)
check "a status that cannot be finished fails loudly" '[[ $out == *"could not write"* ]]'
check "and leaves the last good one in place, with nothing beside it" 'cmp -s "$STATUS" "$T/good" && [ "$(ls "$STATUS_DIR")" = status ]'
/bin/rm "$STATE/chown_fail"
check "any argument is refused" '! health now >/dev/null'

echo
[ "$fails" = 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit "$fails"
