#!/usr/bin/env bash
# Runs deploy/filebox-snapshot, the family folder conversion in deploy/setup.sh and filebox-user delete in a sandbox:
# the real shell logic, with btrfs, stat, date and the account tools stubbed. A "subvolume" is a plain folder whose
# inode is in a registry (so it stays one when moved, like on btrfs), and how full the disk is comes from a file that
# every snapshot delete lowers.
# Run from anywhere: deploy/test/snapshots.sh
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEPLOY=$(dirname "$HERE")
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
BASE=$T/srv/sftp
SNAP=$BASE/.snapshots
STATUS=$T/var/lib/filebox-snapshot/status
mkdir -p "$T/stubs" "$T/state" "$BASE" "$T/var/lib" "$T/run"
: >"$T/state/subvols"
: >"$T/calls.log"
echo btrfs >"$T/state/fstype"
echo "1000 500" >"$T/state/blocks"
echo 0 >"$T/state/freed"
touch "$T/state/quotas"

# Absolute paths moved into the sandbox and the stubs first on PATH.
sandbox() {
	sed -e "s#^export PATH=.*#export PATH=\"$T/stubs:\$PATH\"#" \
		-e "s#/srv/sftp#$BASE#g" \
		-e "s#/var/lib/filebox-snapshot#$T/var/lib/filebox-snapshot#g" \
		-e "s#/run/filebox-snapshot.lock#$T/run/filebox-snapshot.lock#g" \
		-e "s#/usr/local/sbin/filebox-user#$T/stubs/filebox-user#g" "$1" >"$2"
	chmod +x "$2"
}
sandbox "$DEPLOY/filebox-snapshot" "$T/filebox-snapshot"
sandbox "$DEPLOY/filebox-user" "$T/filebox-user"
sed -n '/^# used\*100 >= total\*percent/,/^configure_sshd() {/p' "$DEPLOY/setup.sh" | sed '$d' | sed "s#/srv/sftp#$BASE#g" >"$T/convert.sh"
grep -q '^convert_family()' "$T/convert.sh" || {
	echo "convert_family not found in setup.sh"
	exit 1
}

cat >"$T/stubs/stat" <<EOF
#!/bin/bash
path=\${*: -1}
if [ "\$1" = -f ]; then
	case \$3 in
	%T) cat "$T/state/fstype" ;;
	'%b %f') cat "$T/state/blocks" ;;
	*) exec /usr/bin/stat "\$@" ;;
	esac
	exit 0
fi
if [ "\$1" = -c ] && [ "\$2" = %i ]; then
	inode=\$(/usr/bin/stat -c %i -- "\$path") || exit 1
	if grep -qx -- "\$inode" "$T/state/subvols"; then echo 256; elif [ "\$inode" = 256 ]; then echo 257; else echo "\$inode"; fi
	exit 0
fi
exec /usr/bin/stat "\$@"
EOF
cat >"$T/stubs/btrfs" <<EOF
#!/bin/bash
echo "btrfs \$*" >>"$T/calls.log"
reg=$T/state/subvols
inode() { /usr/bin/stat -c %i -- "\$1"; }
case "\$1 \$2" in
"subvolume create")
	mkdir -- "\${*: -1}" && inode "\${*: -1}" >>"\$reg"
	;;
"subvolume snapshot")
	src=\${*: -2:1}
	dst=\${*: -1}
	[ ! -e "\$dst" ] && cp -a -- "\$src" "\$dst" && inode "\$dst" >>"\$reg"
	;;
"subvolume delete")
	path=\${*: -1}
	id=\$(inode "\$path") && grep -qx -- "\$id" "\$reg" || { echo "not a subvolume: \$path" >&2; exit 1; }
	rm -rf -- "\$path"
	grep -vx -- "\$id" "\$reg" >"\$reg.new"
	mv "\$reg.new" "\$reg"
	read -r total free <"$T/state/blocks"
	echo "\$total \$((free + \$(cat "$T/state/freed")))" >"$T/state/blocks"
	;;
"subvolume sync" | "qgroup clear-stale") ;;
"qgroup show")
	[ -e "$T/state/quotas" ] || exit 1
	echo "Qgroupid Referenced Exclusive Path"
	while read -r id; do
		echo "0/\$id 5000 1000 x"
	done <"\$reg"
	;;
"inspect-internal rootid")
	id=\$(inode "\$3") && grep -qx -- "\$id" "\$reg" && echo "\$id"
	;;
*)
	echo "unexpected: btrfs \$*" >&2
	exit 1
	;;
esac
EOF
cat >"$T/stubs/date" <<EOF
#!/bin/bash
case "\$*" in
"+%Y-%m-%dT%H%M") cat "$T/state/now" ;;
"+%Y%m%d%H%M%S") echo 20261107120000 ;;
*) exec /usr/bin/date "\$@" ;;
esac
EOF
cat >"$T/stubs/id" <<'EOF'
#!/bin/bash
if [ "$*" = -u ]; then echo 0; else echo 1001; fi
EOF
cat >"$T/stubs/install" <<'EOF'
#!/bin/bash
args=()
while [ $# -gt 0 ]; do
	case $1 in -o | -g) shift 2 ;; *) args+=("$1"); shift ;; esac
done
exec /usr/bin/install "${args[@]}"
EOF
cat >"$T/stubs/cp" <<'EOF'
#!/bin/bash
args=()
for arg; do [ "$arg" = --reflink=always ] || args+=("$arg"); done
exec /bin/cp "${args[@]}"
EOF
cat >"$T/stubs/filebox-user" <<EOF
#!/bin/bash
[ "\$1" = list ] && cat "$T/state/users"
EOF
cat >"$T/stubs/getent" <<EOF
#!/bin/bash
name=\${*: -1}
case \$1 in
group) [ "\$name" = sftpusers ] && echo "sftpusers:x:1001:" ;;
passwd) grep -qx -- "\$name" "$T/state/users" && echo "\$name:x:1001:1001:filebox SFTP user:$BASE/\$name:/usr/sbin/nologin" ;;
*) false ;;
esac
EOF
cat >"$T/stubs/userdel" <<EOF
#!/bin/bash
grep -vx -- "\${*: -1}" "$T/state/users" >"$T/state/users.new"
mv "$T/state/users.new" "$T/state/users"
EOF
for c in chown systemctl pkill; do
	printf '#!/bin/sh\necho "%s $*" >>"%s/calls.log"\n' "$c" "$T" >"$T/stubs/$c"
done
chmod +x "$T"/stubs/*
export PATH="$T/stubs:$PATH"

fails=0
check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fails=$((fails + 1)); fi; }

subvol() {
	mkdir -p "$1"
	/usr/bin/stat -c %i -- "$1" >>"$T/state/subvols"
}
is_subvol() { grep -qx -- "$(/usr/bin/stat -c %i -- "$1")" "$T/state/subvols"; }
stamps() { find "$SNAP/$1" -mindepth 1 -maxdepth 1 -printf '%f\n' 2>/dev/null | sort | tr '\n' ' '; }
count() { find "$SNAP/$1" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l; }
history() { # history <scope> <first day> <last day>: one snapshot every night in between
	local day=$2
	while [[ $day < $3 || $day == "$3" ]]; do
		subvol "$SNAP/$1/${day}T0030"
		day=$(/usr/bin/date -u -d "$day +1 day" +%F)
	done
}
snap() { "$T/filebox-snapshot" "$@" 2>&1; }
convert() {
	(
		set -euo pipefail
		log() { echo "==> $*"; }
		# shellcheck disable=SC1091
		. "$T/convert.sh"
		convert_family
		echo "convert_family returned $?"
	) 2>&1
}

echo "--- family conversion is skipped on a 96% full disk"
mkdir -p "$BASE/_family/photos"
echo "soup" >"$BASE/_family/recipe.txt"
echo "beach" >"$BASE/_family/photos/beach.jpg"
echo "1000 40" >"$T/state/blocks"
out=$(convert)
check "warns about the full disk and carries on" '[[ $out == *"95% full"* && $out == *"convert_family returned 0"* ]]'
check "the family folder stays a plain folder with its files" '! is_subvol "$BASE/_family" && [ "$(cat "$BASE/_family/recipe.txt")" = soup ]'
check "nothing was moved aside" '[ ! -e "$BASE/_family.pre-snapshots" ]'

echo "--- family conversion on a disk with room"
echo "1000 500" >"$T/state/blocks"
out=$(convert)
check "converts and carries on" '[[ $out == *"Converting the family folder"* && $out == *"convert_family returned 0"* ]]'
check "the family folder is a subvolume now" 'is_subvol "$BASE/_family"'
check "the files came along" '[ "$(cat "$BASE/_family/recipe.txt")" = soup ] && [ "$(cat "$BASE/_family/photos/beach.jpg")" = beach ]'
check "the old copy is gone once the copy checked out" '[ ! -e "$BASE/_family.pre-snapshots" ]'
check "the panel was stopped for it" 'grep -q "systemctl stop filebox-panel.service" "$T/calls.log"'
out=$(convert)
check "a second run leaves the subvolume alone" '[[ $out != *"Converting"* ]] && [ "$(grep -c "subvolume create" "$T/calls.log")" = 1 ]'

echo "--- first night"
printf 'amy\nbob\n' >"$T/state/users"
subvol "$BASE/amy/files"
echo hello >"$BASE/amy/files/a.txt"
mkdir -p "$BASE/bob/files"
echo 2026-11-07T0030 >"$T/state/now"
out=$(snap daily)
echo "$out" | sed 's/^/    /'
check "amy's files are snapshotted" '[ "$(cat "$SNAP/amy/2026-11-07T0030/a.txt")" = hello ] && is_subvol "$SNAP/amy/2026-11-07T0030"'
check "the family folder is snapshotted" '[ "$(cat "$SNAP/_family/2026-11-07T0030/recipe.txt")" = soup ]'
check "snapshots are read-only" 'grep -q "subvolume snapshot -r $BASE/amy/files" "$T/calls.log"'
check "bob's plain folder is not covered" '[ ! -e "$SNAP/bob" ]'
check "the status says it ran" 'grep -q "^state on$" "$STATUS" && grep -qE "^daily [0-9]+ ok$" "$STATUS"'
check "the status marks bob as not protected" 'grep -qx "scope bob no 0 - - 0 -" "$STATUS"'
check "the status shows amy and the family folder" 'grep -qx "scope amy yes 1 2026-11-07T0030 2026-11-07T0030 1000 -" "$STATUS" && grep -q "^scope _family yes 1 " "$STATUS"'
echo 2026-11-07T0930 >"$T/state/now"
snap daily >/dev/null
check "a second run on the same day takes nothing new" '[ "$(count amy)" = 1 ] && [ "$(count _family)" = 1 ]'

echo "--- retention: 14 days, then the last of each week for 8 weeks and of each month for 6 months"
history amy 2026-04-01 2026-11-06
history _family 2026-10-01 2026-11-06
echo 2026-11-07T0030 >"$T/state/now"
out=$(snap daily)
want="2026-06-30T0030 2026-07-31T0030 2026-08-31T0030 2026-09-20T0030 2026-09-27T0030 2026-09-30T0030 2026-10-04T0030 2026-10-11T0030 2026-10-18T0030"
for day in 25 26 27 28 29 30 31; do want+=" 2026-10-${day}T0030"; done
for day in 01 02 03 04 05 06 07; do want+=" 2026-11-${day}T0030"; done
check "amy keeps exactly the retained days" '[ "$(stamps amy)" = "$want " ]'
check "the family folder keeps 14 days and its weeks" '[ "$(stamps _family | cut -d" " -f1-4)" = "2026-10-04T0030 2026-10-11T0030 2026-10-18T0030 2026-10-25T0030" ] && [ "$(count _family)" = 17 ]'
check "pruned snapshots were deleted as subvolumes" 'grep -q "subvolume delete $SNAP/amy/2026-04-01T0030" "$T/calls.log"'

echo "--- a 97% full disk loses its oldest snapshots, whichever folder they are in"
echo "1000 30" >"$T/state/blocks"
echo 10 >"$T/state/freed"
out=$(snap check)
echo "$out" | sed 's/^/    /'
check "the three oldest are gone" '[ "$(stamps amy | cut -d" " -f1)" = 2026-09-20T0030 ] && [ "$(count amy)" = 20 ] && [ "$(count _family)" = 17 ]'
check "it stops once the disk is below 95%" '[ "$(cat "$T/state/blocks")" = "1000 60" ]'
check "the status records it" 'grep -qE "^lowspace [0-9]+ 3 no$" "$STATUS"'
check "it is logged" '[[ $out == *"deleted the 3 oldest"* ]]'

echo "--- a disk that stays full keeps the newest 7 of every folder"
echo "1000 0" >"$T/state/blocks"
echo 0 >"$T/state/freed"
out=$(snap check)
check "amy keeps her newest 7" '[ "$(stamps amy)" = "2026-11-01T0030 2026-11-02T0030 2026-11-03T0030 2026-11-04T0030 2026-11-05T0030 2026-11-06T0030 2026-11-07T0030 " ]'
check "the family folder keeps its newest 7" '[ "$(count _family)" = 7 ]'
check "the status says it is still full" 'grep -qE "^lowspace [0-9]+ 23 yes$" "$STATUS"'
check "the daily result is carried over" 'grep -qE "^daily [0-9]+ ok$" "$STATUS"'
check "held space counts each snapshot's own bytes" 'grep -qx "scope amy yes 7 2026-11-01T0030 2026-11-07T0030 7000 -" "$STATUS"'
echo "1000 500" >"$T/state/blocks"

echo "--- without btrfs quotas the held space is unknown"
rm "$T/state/quotas"
snap check >/dev/null
check "held shows -" 'grep -qx "scope amy yes 7 2026-11-01T0030 2026-11-07T0030 - -" "$STATUS"'
touch "$T/state/quotas"

echo "--- deleting amy moves her snapshots aside"
out=$("$T/filebox-user" delete amy 2>&1)
check "delete succeeds" '[ -z "$out" ]'
check "the home is archived" '[ -d "$BASE/amy.deleted-20261107120000" ] && [ ! -e "$BASE/amy" ]'
check "the snapshots moved with it" '[ ! -e "$SNAP/amy" ] && [ "$(count amy.deleted-20261107120000)" = 7 ]'
check "and are locked to root" '[ "$(stat -c %a "$SNAP/amy.deleted-20261107120000")" = 700 ] && grep -q "chown root:root -- $SNAP/amy.deleted-20261107120000" "$T/calls.log"'
snap check >/dev/null
check "the status shows them as gone" 'grep -q "^scope amy.deleted-20261107120000 gone 7 " "$STATUS" && ! grep -q "^scope amy " "$STATUS"'

echo "--- a new amy starts with no earlier versions"
echo amy >>"$T/state/users"
subvol "$BASE/amy/files"
echo new >"$BASE/amy/files/b.txt"
echo 2026-11-08T0030 >"$T/state/now"
snap daily >/dev/null
check "only tonight's snapshot" '[ "$(stamps amy)" = "2026-11-08T0030 " ] && [ ! -e "$SNAP/amy/2026-11-08T0030/a.txt" ]'
check "the old ones are still kept aside" '[ "$(count amy.deleted-20261107120000)" = 7 ]'
echo 2027-06-01T0030 >"$T/state/now"
snap daily >/dev/null
check "they expire on the normal schedule and the folder goes" '[ ! -e "$SNAP/amy.deleted-20261107120000" ]'

echo "--- a disk that is not btrfs switches it off"
echo ext4 >"$T/state/fstype"
snap check >/dev/null
check "the status says why" 'grep -qx "state off not_btrfs" "$STATUS" && ! grep -q "^scope " "$STATUS"'
echo btrfs >"$T/state/fstype"

echo "--- arguments"
check "an unknown command is refused" '! snap prune >/dev/null'
check "extra arguments are refused" '! snap daily now >/dev/null'

echo
[ "$fails" = 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit "$fails"
