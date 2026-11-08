#!/usr/bin/env bash
# Runs deploy/filebox-snapshot, the family folder conversion in deploy/setup.sh and filebox-user create and delete in a
# sandbox: the real shell logic, with btrfs, stat, date and the account tools stubbed. A "subvolume" is a plain folder
# whose inode is in a registry (so it stays one when moved, like on btrfs), and how full the disk is comes from a file
# that every snapshot delete lowers. Flag files in the state folder make chosen commands fail.
# Run from anywhere: deploy/test/snapshots.sh
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEPLOY=$(dirname "$HERE")
T=$(mktemp -d)
trap '/bin/rm -rf "$T"' EXIT
BASE=$T/srv/sftp
SNAP=$BASE/.snapshots
STATUS=$T/var/lib/filebox-snapshot/status
STATE=$T/state
mkdir -p "$T/stubs" "$STATE" "$BASE" "$T/var/lib" "$T/run"
: >"$STATE/subvols"
: >"$STATE/users"
: >"$T/calls.log"
echo btrfs >"$STATE/fstype"
echo "1000 500" >"$STATE/blocks"
echo 0 >"$STATE/freed"
echo 20261107120000 >"$STATE/stamp"
touch "$STATE/quotas" "$STATE/panel_active"

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
sed -n '/^# The family folder conversion builds/,/^configure_sshd() {/p' "$DEPLOY/setup.sh" | sed '$d' | sed "s#/srv/sftp#$BASE#g" >"$T/convert.sh"
grep -q '^convert_family()' "$T/convert.sh" || {
	echo "convert_family not found in setup.sh"
	exit 1
}

cat >"$T/stubs/stat" <<EOF
#!/bin/bash
path=\${*: -1}
if [ "\$1" = -f ]; then
	case \$3 in
	%T) cat "$STATE/fstype" ;;
	'%b %f') cat "$STATE/blocks" ;;
	*) exec /usr/bin/stat "\$@" ;;
	esac
	exit 0
fi
if [ "\$1" = -c ] && [ "\$2" = %i ]; then
	inode=\$(/usr/bin/stat -c %i -- "\$path") || exit 1
	if grep -qx -- "\$inode" "$STATE/subvols"; then echo 256; elif [ "\$inode" = 256 ]; then echo 257; else echo "\$inode"; fi
	exit 0
fi
exec /usr/bin/stat "\$@"
EOF
cat >"$T/stubs/btrfs" <<EOF
#!/bin/bash
echo "btrfs \$*" >>"$T/calls.log"
reg=$STATE/subvols
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
	/bin/rm -rf -- "\$path"
	grep -vx -- "\$id" "\$reg" >"\$reg.new"
	mv "\$reg.new" "\$reg"
	read -r total free <"$STATE/blocks"
	echo "\$total \$((free + \$(cat "$STATE/freed")))" >"$STATE/blocks"
	;;
"subvolume sync" | "qgroup clear-stale") ;;
"qgroup show")
	[ -e "$STATE/quotas" ] || exit 1
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
"+%Y-%m-%dT%H%M") cat "$STATE/now" ;;
"+%Y%m%d%H%M%S") cat "$STATE/stamp" ;;
*) exec /usr/bin/date "\$@" ;;
esac
EOF
cat >"$T/stubs/id" <<'EOF'
#!/bin/bash
if [ "$*" = -u ]; then echo 0; else echo 1001; fi
EOF
# install records the owner and group it was asked for, then runs without them (the sandbox has no such users).
cat >"$T/stubs/install" <<EOF
#!/bin/bash
echo "install \$*" >>"$T/calls.log"
if [ -e "$STATE/install_fail" ] && [ "\${*: -1}" = "$SNAP" ]; then exit 1; fi
args=()
while [ \$# -gt 0 ]; do
	case \$1 in -o | -g) shift 2 ;; *) args+=("\$1"); shift ;; esac
done
exec /usr/bin/install "\${args[@]}"
EOF
cat >"$T/stubs/cp" <<EOF
#!/bin/bash
args=()
for arg; do [ "\$arg" = --reflink=always ] || args+=("\$arg"); done
/bin/cp "\${args[@]}" || exit
if [ -e "$STATE/cp_fail" ]; then exit 1; fi
if [ -e "$STATE/cp_extra" ]; then touch "\${*: -1}/extra.txt"; fi
EOF
cat >"$T/stubs/rm" <<EOF
#!/bin/bash
if [ -e "$STATE/rm_fail_old" ] && [[ \$* == *_family.pre-snapshots* ]]; then exit 1; fi
exec /bin/rm "\$@"
EOF
cat >"$T/stubs/systemctl" <<EOF
#!/bin/bash
echo "systemctl \$*" >>"$T/calls.log"
if [ "\$1" = is-active ]; then [ -e "$STATE/panel_active" ]; fi
EOF
cat >"$T/stubs/filebox-user" <<EOF
#!/bin/bash
[ ! -e "$STATE/list_fail" ] && [ "\$1" = list ] && cat "$STATE/users"
EOF
cat >"$T/stubs/getent" <<EOF
#!/bin/bash
name=\${*: -1}
case \$1 in
group) [ "\$name" = sftpusers ] && echo "sftpusers:x:1001:" ;;
passwd) grep -qx -- "\$name" "$STATE/users" && echo "\$name:x:1001:1001:filebox SFTP user:$BASE/\$name:/usr/sbin/nologin" ;;
*) false ;;
esac
EOF
cat >"$T/stubs/useradd" <<EOF
#!/bin/bash
echo "\${*: -1}" >>"$STATE/users"
EOF
cat >"$T/stubs/userdel" <<EOF
#!/bin/bash
grep -vx -- "\${*: -1}" "$STATE/users" >"$STATE/users.new"
mv "$STATE/users.new" "$STATE/users"
EOF
for c in chown chpasswd pkill; do
	printf '#!/bin/sh\necho "%s $*" >>"%s/calls.log"\n' "$c" "$T" >"$T/stubs/$c"
done
chmod +x "$T"/stubs/*
export PATH="$T/stubs:$PATH"

fails=0
check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fails=$((fails + 1)); fi; }

inode_of() { /usr/bin/stat -c %i -- "$1"; }
subvol() {
	mkdir -p "$1"
	inode_of "$1" >>"$STATE/subvols"
}
is_subvol() { [ -e "$1" ] && grep -qx -- "$(inode_of "$1")" "$STATE/subvols"; }
# drop removes a folder for good, and forgets it as a subvolume so a new folder that reuses the inode is not one.
drop() {
	local path
	for path; do
		if [ -e "$path" ]; then
			grep -vx -- "$(inode_of "$path")" "$STATE/subvols" >"$STATE/subvols.new"
			mv "$STATE/subvols.new" "$STATE/subvols"
			/bin/rm -rf -- "$path"
		fi
	done
}
plain_family() {
	drop "$BASE/_family" "$BASE/_family.new" "$BASE/_family.pre-snapshots"
	mkdir -p "$BASE/_family/photos"
	echo soup >"$BASE/_family/recipe.txt"
	echo beach >"$BASE/_family/photos/beach.jpg"
}
family_intact() { [ "$(cat "$BASE/_family/recipe.txt")" = soup ] && [ "$(cat "$BASE/_family/photos/beach.jpg")" = beach ]; }
nothing_aside() { [ ! -e "$BASE/_family.new" ] && [ ! -e "$BASE/_family.pre-snapshots" ]; }
panel_restarted() {
	[ "$(grep -c "systemctl stop filebox-panel.service" "$T/calls.log")" = "$(grep -c "systemctl start --no-block filebox-panel.service" "$T/calls.log")" ]
}
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

echo "--- a new install gets the family folder as a subvolume"
out=$(convert)
check "it is created as one" 'is_subvol "$BASE/_family" && [[ $out == *"convert_family returned 0"* ]]'

echo "--- family conversion is skipped on a 96% full disk"
plain_family
echo "1000 40" >"$STATE/blocks"
out=$(convert)
check "warns about the full disk and carries on" '[[ $out == *"95% full"* && $out == *"convert_family returned 0"* ]]'
check "the family folder stays a plain folder with its files" '! is_subvol "$BASE/_family" && family_intact && nothing_aside'
echo "1000 500" >"$STATE/blocks"

echo "--- a copy that fails leaves the live family folder alone"
touch "$STATE/cp_fail"
out=$(convert)
/bin/rm "$STATE/cp_fail"
check "warns and carries on" '[[ $out == *"could not make a matching copy"* && $out == *"convert_family returned 0"* ]]'
check "the family folder is untouched and nothing is left beside it" '! is_subvol "$BASE/_family" && family_intact && nothing_aside'
check "the panel was stopped and started again" 'grep -q "systemctl stop filebox-panel.service" "$T/calls.log" && panel_restarted'

echo "--- a copy that does not match is thrown away"
touch "$STATE/cp_extra"
out=$(convert)
/bin/rm "$STATE/cp_extra"
check "warns and carries on" '[[ $out == *"could not make a matching copy"* && $out == *"convert_family returned 0"* ]]'
check "the family folder is untouched" '! is_subvol "$BASE/_family" && family_intact && [ ! -e "$BASE/_family/extra.txt" ] && nothing_aside && panel_restarted'

echo "--- a leftover old copy beside a live family folder is never touched"
mkdir "$BASE/_family.pre-snapshots"
echo "only here" >"$BASE/_family.pre-snapshots/note.txt"
out=$(convert)
check "warns loudly and changes nothing" '[[ $out == *"may hold files"* ]] && ! is_subvol "$BASE/_family" && family_intact && [ -f "$BASE/_family.pre-snapshots/note.txt" ]'
drop "$BASE/_family.pre-snapshots"

echo "--- a family folder found moved aside is put back"
/bin/mv -T "$BASE/_family" "$BASE/_family.pre-snapshots"
out=$(convert)
check "it is back where it was" '[[ $out == *"put back"* ]] && family_intact && nothing_aside'

echo "--- a run that stopped between the two moves of the swap is finished"
subvol "$BASE/_family.new"
/bin/cp -a "$BASE/_family/." "$BASE/_family.new/"
/bin/mv -T "$BASE/_family" "$BASE/_family.pre-snapshots"
out=$(convert)
check "the checked copy is swapped in" '[[ $out == *"Finishing the interrupted conversion"* ]] && is_subvol "$BASE/_family" && family_intact && nothing_aside'

echo "--- setup stopping on an error right after the swap still starts the panel"
plain_family
touch "$STATE/rm_fail_old"
out=$(convert)
/bin/rm "$STATE/rm_fail_old"
check "setup stopped" '[[ $out != *"convert_family returned"* ]]'
check "the converted folder is live and the old one kept" 'is_subvol "$BASE/_family" && family_intact && [ -d "$BASE/_family.pre-snapshots" ]'
check "the panel was started again" 'panel_restarted && [ "$(tail -n 1 "$T/calls.log")" = "systemctl start --no-block filebox-panel.service" ]'
out=$(convert)
check "the next run warns about the old copy" '[[ $out == *"may hold files"* ]]'

echo "--- a leftover new copy is thrown away before converting"
plain_family
subvol "$BASE/_family.new"
echo junk >"$BASE/_family.new/junk.txt"
out=$(convert)
check "converts and carries on" '[[ $out == *"Converting the family folder"* && $out == *"convert_family returned 0"* ]]'
check "the family folder is a subvolume with its own files only" 'is_subvol "$BASE/_family" && family_intact && [ ! -e "$BASE/_family/junk.txt" ] && nothing_aside'
check "the new folder got the family owner" 'grep -q "chown filebox:filebox $BASE/_family.new" "$T/calls.log"'
check "the panel is running again" 'panel_restarted'
creates=$(grep -c "subvolume create" "$T/calls.log")
out=$(convert)
check "a second run leaves the subvolume alone" '[[ $out != *"Converting"* ]] && [ "$(grep -c "subvolume create" "$T/calls.log")" = "$creates" ]'

echo "--- first night"
printf 'amy\nbob\n' >"$STATE/users"
subvol "$BASE/amy/files"
echo hello >"$BASE/amy/files/a.txt"
mkdir -p "$BASE/bob/files"
echo 2026-11-07T0030 >"$STATE/now"
out=$(snap daily)
echo "$out" | sed 's/^/    /'
check "amy's files are snapshotted" '[ "$(cat "$SNAP/amy/2026-11-07T0030/a.txt")" = hello ] && is_subvol "$SNAP/amy/2026-11-07T0030"'
check "the family folder is snapshotted" '[ "$(cat "$SNAP/_family/2026-11-07T0030/recipe.txt")" = soup ]'
check "snapshots are read-only" 'grep -q "subvolume snapshot -r $BASE/amy/files" "$T/calls.log"'
check "the snapshot folders are asked for root:filebox 0710 and 0750" 'grep -q "install -d -o root -g filebox -m 0710 $SNAP$" "$T/calls.log" && grep -q "install -d -o root -g filebox -m 0750 $SNAP/amy$" "$T/calls.log"'
check "bob's plain folder is not covered" '[ ! -e "$SNAP/bob" ]'
check "the status says it ran" 'grep -q "^state on$" "$STATUS" && grep -qE "^daily [0-9]+ ok$" "$STATUS"'
check "the status marks bob as not protected" 'grep -qx "scope bob no 0 - - 0 -" "$STATUS"'
check "the status shows amy and the family folder" 'grep -qx "scope amy yes 1 2026-11-07T0030 2026-11-07T0030 1000 -" "$STATUS" && grep -q "^scope _family yes 1 " "$STATUS"'
echo 2026-11-07T0930 >"$STATE/now"
snap daily >/dev/null
check "a second run on the same day takes nothing new" '[ "$(count amy)" = 1 ] && [ "$(count _family)" = 1 ]'

echo "--- a nightly run that stops halfway still reports it failed"
touch "$STATE/install_fail"
snap daily >/dev/null
code=$?
/bin/rm "$STATE/install_fail"
check "it fails" '[ "$code" != 0 ]'
check "the status says the night failed" 'grep -qE "^daily [0-9]+ failed$" "$STATUS" && grep -q "^state on$" "$STATUS"'

echo "--- when the accounts cannot be listed nobody is reported gone"
touch "$STATE/list_fail"
snap check >/dev/null
/bin/rm "$STATE/list_fail"
check "amy is unchecked, not gone" 'grep -q "^scope amy unchecked 1 " "$STATUS" && ! grep -q " gone " "$STATUS"'
check "the family folder is still checked" 'grep -q "^scope _family yes 1 " "$STATUS"'

echo "--- retention: 14 days, then the last of each week for 8 weeks and of each month for 6 months"
history amy 2026-04-01 2026-11-06
history _family 2026-10-01 2026-11-06
echo 2026-11-07T0030 >"$STATE/now"
out=$(snap daily)
want="2026-06-30T0030 2026-07-31T0030 2026-08-31T0030 2026-09-20T0030 2026-09-27T0030 2026-09-30T0030 2026-10-04T0030 2026-10-11T0030 2026-10-18T0030"
for day in 25 26 27 28 29 30 31; do want+=" 2026-10-${day}T0030"; done
for day in 01 02 03 04 05 06 07; do want+=" 2026-11-${day}T0030"; done
check "amy keeps exactly the retained days" '[ "$(stamps amy)" = "$want " ]'
check "the family folder keeps 14 days and its weeks" '[ "$(stamps _family | cut -d" " -f1-4)" = "2026-10-04T0030 2026-10-11T0030 2026-10-18T0030 2026-10-25T0030" ] && [ "$(count _family)" = 17 ]'
check "pruned snapshots were deleted as subvolumes" 'grep -q "subvolume delete $SNAP/amy/2026-04-01T0030" "$T/calls.log"'
check "a good night reports ok again" 'grep -qE "^daily [0-9]+ ok$" "$STATUS"'

echo "--- a 97% full disk loses its oldest snapshots, whichever folder they are in"
echo "1000 30" >"$STATE/blocks"
echo 10 >"$STATE/freed"
out=$(snap check)
echo "$out" | sed 's/^/    /'
check "the three oldest are gone" '[ "$(stamps amy | cut -d" " -f1)" = 2026-09-20T0030 ] && [ "$(count amy)" = 20 ] && [ "$(count _family)" = 17 ]'
check "it stops once the disk is below 95%" '[ "$(cat "$STATE/blocks")" = "1000 60" ]'
check "the status records it" 'grep -qE "^lowspace [0-9]+ 3 no$" "$STATUS"'
check "it is logged" '[[ $out == *"deleted the 3 oldest"* ]]'

echo "--- a disk that stays full keeps the newest 7 of every folder"
echo "1000 0" >"$STATE/blocks"
echo 0 >"$STATE/freed"
out=$(snap check)
check "amy keeps her newest 7" '[ "$(stamps amy)" = "2026-11-01T0030 2026-11-02T0030 2026-11-03T0030 2026-11-04T0030 2026-11-05T0030 2026-11-06T0030 2026-11-07T0030 " ]'
check "the family folder keeps its newest 7" '[ "$(count _family)" = 7 ]'
check "the status says it is still full" 'grep -qE "^lowspace [0-9]+ 23 yes$" "$STATUS"'
check "the daily result is carried over" 'grep -qE "^daily [0-9]+ ok$" "$STATUS"'
check "held space counts each snapshot's own bytes" 'grep -qx "scope amy yes 7 2026-11-01T0030 2026-11-07T0030 7000 -" "$STATUS"'
echo "1000 500" >"$STATE/blocks"

echo "--- without btrfs quotas the held space is unknown"
/bin/rm "$STATE/quotas"
snap check >/dev/null
check "held shows -" 'grep -qx "scope amy yes 7 2026-11-01T0030 2026-11-07T0030 - -" "$STATUS"'
touch "$STATE/quotas"

echo "--- deleting amy moves her snapshots aside"
out=$("$T/filebox-user" delete amy 2>&1)
check "delete succeeds" '[ -z "$out" ]'
check "the home is archived" '[ -d "$BASE/amy.deleted-20261107120000" ] && [ ! -e "$BASE/amy" ]'
check "the snapshots moved with it" '[ ! -e "$SNAP/amy" ] && [ "$(count amy.deleted-20261107120000)" = 7 ]'
check "and get chown root:root and mode 0700" '[ "$(stat -c %a "$SNAP/amy.deleted-20261107120000")" = 700 ] && grep -q "chown root:root -- $SNAP/amy.deleted-20261107120000" "$T/calls.log"'
snap check >/dev/null
check "the status shows them as gone" 'grep -q "^scope amy.deleted-20261107120000 gone 7 " "$STATUS" && ! grep -q "^scope amy " "$STATUS"'

echo "--- creating an account moves aside earlier versions left under its name"
subvol "$SNAP/dave/2026-11-07T0030"
echo 20261108090000 >"$STATE/stamp"
out=$(printf 'a long enough password\n' | "$T/filebox-user" create dave 2>&1)
check "create succeeds" '[ -z "$out" ] && grep -qx dave "$STATE/users" && is_subvol "$BASE/dave/files"'
check "the leftover versions are moved aside" '[ ! -e "$SNAP/dave" ] && [ "$(count dave.deleted-20261108090000)" = 1 ]'
check "and get chown root:root and mode 0700" '[ "$(stat -c %a "$SNAP/dave.deleted-20261108090000")" = 700 ] && grep -q "chown root:root -- $SNAP/dave.deleted-20261108090000" "$T/calls.log"'
snap check >/dev/null
check "the new dave has no earlier versions" 'grep -qx "scope dave yes 0 - - 0 -" "$STATUS" && grep -q "^scope dave.deleted-20261108090000 gone 1 " "$STATUS"'
echo 20261107120000 >"$STATE/stamp"

echo "--- a new amy starts with no earlier versions"
echo amy >>"$STATE/users"
subvol "$BASE/amy/files"
echo new >"$BASE/amy/files/b.txt"
echo 2026-11-08T0030 >"$STATE/now"
snap daily >/dev/null
check "only tonight's snapshot" '[ "$(stamps amy)" = "2026-11-08T0030 " ] && [ ! -e "$SNAP/amy/2026-11-08T0030/a.txt" ]'
check "the old ones are still kept aside" '[ "$(count amy.deleted-20261107120000)" = 7 ]'
echo 2027-06-01T0030 >"$STATE/now"
snap daily >/dev/null
check "they expire on the normal schedule and the folder goes" '[ ! -e "$SNAP/amy.deleted-20261107120000" ]'

echo "--- a disk that is not btrfs switches it off"
echo ext4 >"$STATE/fstype"
snap check >/dev/null
check "the status says why" 'grep -qx "state off not_btrfs" "$STATUS" && ! grep -q "^scope " "$STATUS"'
echo btrfs >"$STATE/fstype"

echo "--- arguments"
check "an unknown command is refused" '! snap prune >/dev/null'
check "extra arguments are refused" '! snap daily now >/dev/null'

echo
[ "$fails" = 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit "$fails"
