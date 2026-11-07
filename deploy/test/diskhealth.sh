#!/usr/bin/env bash
# Runs deploy/filebox-disk-health in a sandbox: the real shell logic, with btrfs, stat, mountpoint and findmnt stubbed
# and /sys/fs/btrfs a plain folder. Files in the state folder decide what the stubs answer.
# Run from anywhere: deploy/test/diskhealth.sh
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEPLOY=$(dirname "$HERE")
T=$(mktemp -d)
trap '/bin/rm -rf "$T"' EXIT
BASE=$T/srv/sftp
SYSFS=$T/sys/fs/btrfs
STATE=$T/state
UUID=0f1e2d3c-4b5a-4968-8776-a5b4c3d2e1f0
DEVINFO=$SYSFS/$UUID/devinfo
mkdir -p "$T/stubs" "$T/bin" "$STATE" "$BASE"

# Only the stubs and the tools the helper needs are on PATH, so a missing btrfs stub means no btrfs-progs.
for tool in awk sed head grep cat date; do
	ln -s "$(command -v "$tool")" "$T/bin/$tool"
done
sed -e "s#^export PATH=.*#export PATH=\"$T/stubs:$T/bin\"#" \
	-e "s#/srv/sftp#$BASE#g" \
	-e "s#/sys/fs/btrfs#$SYSFS#g" "$DEPLOY/filebox-disk-health" >"$T/filebox-disk-health"
chmod +x "$T/filebox-disk-health"

cat >"$T/stubs/stat" <<EOF
#!/bin/bash
[ "\$*" = "-f -c %T -- $BASE" ] && cat "$STATE/fstype"
EOF
cat >"$T/stubs/mountpoint" <<EOF
#!/bin/bash
[ -e "$STATE/mounted" ]
EOF
cat >"$T/stubs/findmnt" <<EOF
#!/bin/bash
[ "\$*" = "--fstab -n -o FSTYPE $BASE" ] && [ -e "$STATE/fstab" ] && cat "$STATE/fstab"
EOF
cat >"$T/stubs/btrfs" <<EOF
#!/bin/bash
case "\$*" in
"filesystem show $BASE")
	printf "Label: 'filebox-data'  uuid: $UUID\n\tTotal devices 2 FS bytes used 812.53GiB\n\t*** Some devices missing\n"
	;;
"scrub status $BASE")
	[ -e "$STATE/scrub_fail" ] && exit 1
	cat "$STATE/scrub"
	;;
*)
	echo "unexpected: btrfs \$*" >&2
	exit 1
	;;
esac
EOF
chmod +x "$T"/stubs/*

fails=0
check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fails=$((fails + 1)); fi; }
health() { "$T/filebox-disk-health" "$@" 2>&1; }
expect() { [ "$out" = "$(printf '%s\n' "$@")" ]; }

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

echo btrfs >"$STATE/fstype"
touch "$STATE/mounted"
echo btrfs >"$STATE/fstab"
disk 1 0 0 0 0
disk 2 0 1 2 0
scrubbed finished "no errors found"
on() { expect "filebox-disk-health 1" "state on" "$@"; }
clean_scrub="scrub finished $STARTED 0 0"

echo "--- a healthy mirror"
out=$(health)
check "reports both disks and a clean scrub" 'on "device 1 ok 0" "device 2 ok 3" "$clean_scrub"'

echo "--- a mirror running without one of its disks"
disk 2 1 0 0 0
out=$(health)
check "only sysfs decides, not btrfs filesystem show, which calls them all missing in the sandbox" 'on "device 1 ok 0" "device 2 missing 0" "$clean_scrub"'
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
printf 'UUID:             %s
	no stats available
' "$UUID" >"$STATE/scrub"
out=$(health)
check "no scrub yet" 'on "device 1 ok 0" "device 2 ok 3" "scrub none - 0 0"'
printf 'UUID:             %s
Status:           confused
' "$UUID" >"$STATE/scrub"
out=$(health)
check "a status it does not know is unknown" 'on "device 1 ok 0" "device 2 ok 3" "scrub unknown"'
touch "$STATE/scrub_fail"
out=$(health)
check "a scrub status that fails is unknown" 'on "device 1 ok 0" "device 2 ok 3" "scrub unknown"'
/bin/rm "$STATE/scrub_fail"
scrubbed finished "no errors found"

echo "--- not applicable"
/bin/rm "$STATE/mounted"
echo ext4 >"$STATE/fstype"
out=$(health)
check "a btrfs data disk that is not mounted says so" 'expect "filebox-disk-health 1" "state unmounted"'
echo ext4 >"$STATE/fstab"
out=$(health)
check "an ext4 data disk is not applicable" 'expect "filebox-disk-health 1" "state off not_btrfs"'
/bin/rm "$STATE/fstab"
out=$(health)
check "files on the system disk are not applicable" 'expect "filebox-disk-health 1" "state off not_btrfs"'
touch "$STATE/mounted"
echo btrfs >"$STATE/fstype"
mv "$T/stubs/btrfs" "$T/btrfs.aside"
out=$(health)
check "btrfs without btrfs-progs says so" 'expect "filebox-disk-health 1" "state off no_btrfs_progs"'
mv "$T/btrfs.aside" "$T/stubs/btrfs"

echo "--- arguments"
check "any argument is refused" '! "$T/filebox-disk-health" now >/dev/null 2>&1'
check "and nothing is reported" '[ -z "$("$T/filebox-disk-health" now 2>/dev/null)" ]'

echo
[ "$fails" = 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit "$fails"
