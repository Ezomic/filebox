#!/usr/bin/env bash
# Runs deploy/filebox-backup and deploy/filebox-restore in a sandbox: the real shell logic, with restic, mount, unshare,
# ssh, stat and the ownership commands stubbed. restic itself is never run. Its stub keeps a "repository" of plain
# folders: a backup copies what the recorded mounts make visible and drops what the --exclude patterns match, read with
# this stub's own simple reading of them (a leading / anchors, * stays within one path part), and a restore copies one
# path back. So this tests what the helpers ask restic to do and what they make of its answers. Whether the real restic
# reads the patterns, locks, includes and exit codes the same way is not tested here.
# Run from anywhere: deploy/test/backup.sh (needs jq, like the box).
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEPLOY=$(dirname "$HERE")
T=$(mktemp -d)
trap '/bin/rm -rf "$T"' EXIT
BASE=$T/srv/sftp
SNAP=$BASE/.snapshots
SECRETS=$T/etc/filebox/backup
STATUS=$T/var/lib/filebox-backup/status
STATE=$T/state
TOOL=$T/usr/local/sbin/filebox-backup
RESTORE=$T/filebox-restore
mkdir -p "$T/stubs" "$STATE" "$BASE" "$T/var/lib/filebox" "$T/etc/filebox" "$T/run" "$T/usr/local/sbin" "$T/proc/1/ns" "$T/proc/self/ns"
: >"$T/calls.log"
: >"$T/restic.log"
: >"$STATE/mounts"
ln -s "mnt:[host]" "$T/proc/1/ns/mnt"

command -v jq >/dev/null || {
	echo "jq is needed"
	exit 1
}

# Every absolute path the helpers use is a readonly line at the top; those move into the sandbox. The 6 hour window
# becomes a few seconds.
sandbox() {
	sed -E -e "s#^export PATH=.*#export PATH=\"$T/stubs:\$PATH\"#" \
		-e "s#^(readonly [A-Z_]+=)(/[^ ]*)\$#\1$T\2#" \
		-e "s#^readonly WINDOW=.*#readonly WINDOW=8#" \
		-e "s#^readonly STOP_GRACE=.*#readonly STOP_GRACE=1#" \
		-e "s#^readonly CHECK_TIME=.*#readonly CHECK_TIME=2#" "$1" >"$2"
	chmod +x "$2"
}
sandbox "$DEPLOY/filebox-backup" "$TOOL"
sandbox "$DEPLOY/filebox-restore" "$RESTORE"
if grep -nhE '(^|[ "=])/(etc|srv|usr|var|run|proc)/' "$TOOL" "$RESTORE" | grep -v "$T" | grep -vE '^[0-9]+:\s*#'; then
	echo "UNSANDBOXED PATH ABOVE"
	exit 1
fi

stub() {
	printf '#!/bin/bash\n%s\n' "$2" | sed "s#@T@#$T#g; s#@STATE@#$STATE#g" >"$T/stubs/$1"
	chmod +x "$T/stubs/$1"
}
stub id '[ "$*" = -u ] && echo 0'
stub hostname 'echo box'
stub chown 'echo "chown $*" >>@T@/calls.log'
stub install '
echo "install $*" >>@T@/calls.log
args=()
while [ $# -gt 0 ]; do
	case $1 in -o | -g) shift 2 ;; *) args+=("$1"); shift ;; esac
done
exec /usr/bin/install "${args[@]}"'
stub unshare '
echo "unshare $*" >>@T@/calls.log
ln -sfn "mnt:[private]" @T@/proc/self/ns/mnt
while [ "$1" != -- ]; do shift; done
shift
exec "$@"'
# Records each mount for the restic stub, and fails any mount outside a private namespace. A plain bind becomes a
# symlink, so the helper can read through it; run_backup takes it away again, as the end of the namespace would.
stub mount '
echo "mount $*" >>@T@/calls.log
[ "$(readlink @T@/proc/self/ns/mnt)" = "mnt:[private]" ] || { echo "MOUNT IN THE HOST NAMESPACE: $*" >&2; exit 1; }
case "$*" in
"--make-rprivate /") echo "rprivate /" >>@STATE@/mounts ;;
"--bind -o ro "*) echo "bind-ro $4 $5" >>@STATE@/mounts ;;
"--bind "*)
	echo "bind $2 $3" >>@STATE@/mounts
	rmdir "$3" && ln -s "$2" "$3"
	;;
"-t tmpfs "*) echo "tmpfs ${*: -1}" >>@STATE@/mounts ;;
*) echo "unexpected mount $*" >&2; exit 1 ;;
esac'
stub filebox-user '[ ! -e @STATE@/list_fail ] && [ "$1" = list ] && cat @STATE@/users'
mv "$T/stubs/filebox-user" "$T/usr/local/sbin/filebox-user"
# The usb-disk folder is a disk of its own, until the flag file says it is not mounted.
stub stat '
if [ "$1 $2" = "-c %d" ] && [[ ${*: -1} == @T@/usb-disk* ]] && [ ! -e @STATE@/usb_unmounted ]; then
	echo 4242
	exit 0
fi
exec /usr/bin/stat "$@"'
stub ssh-keygen '
echo "ssh-keygen $*" >>@T@/calls.log
case $1 in
-q) echo PRIVATE >"${*: -1}"; echo "ssh-ed25519 AAAAC3testkey filebox-backup@box" >"${*: -1}.pub" ;;
-l) echo "256 SHA256:hostfingerprint [u123456.your-storagebox.de]:23 (ED25519)" ;;
*) exit 1 ;;
esac'
stub ssh-keyscan '
echo "ssh-keyscan $*" >>@T@/calls.log
[ -e @STATE@/keyscan_fail ] && exit 1
echo "[${*: -1}]:$2 ssh-ed25519 AAAAC3hostkey"'
stub ssh '
echo "ssh $*" >>@T@/calls.log
cat >@STATE@/ssh_stdin
[ ! -e @STATE@/ssh_fail ]'

# The restic stub. Flag files in the state folder make it fail, run past the window, ignore SIGINT or report files it
# could not read. Repository URLs that are not a local path map to a folder per URL.
cat >"$T/stubs/restic" <<'RESTIC'
#!/bin/bash
T=@T@
STATE=@STATE@
printf 'RESTIC_CACHE_DIR=%s\nTMPDIR=%s\nGOGC=%s\nAWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\n' "${RESTIC_CACHE_DIR:-}" \
	"${TMPDIR:-}" "${GOGC:-}" "${AWS_ACCESS_KEY_ID:-}" "${AWS_SECRET_ACCESS_KEY:-}" >"$T/restic.env"
while [ "${1:-}" = -o ]; do
	echo "$2" >>"$T/restic.options"
	shift 2
done
echo "restic $*" >>"$T/restic.log"
command=$1
shift
repo=$RESTIC_REPOSITORY
[[ $repo == /* ]] || repo=$T/remote/$(printf %s "$repo" | tr -c 'A-Za-z0-9' _)
fail() {
	echo "Fatal: $*" >&2
	exit 1
}
if [ "$command" != init ]; then
	[ -f "$repo/config" ] || fail "unable to open config file"
	cmp -s "$RESTIC_PASSWORD_FILE" "$repo/key" || fail "wrong password or no key found"
fi
resolve() {
	local path=$1 kind src dst
	while read -r kind src dst; do
		if [ "$kind" = bind ] && { [ "$path" = "$dst" ] || [[ $path == "$dst"/* ]]; }; then
			path=$src${path#"$dst"}
		fi
	done <"$STATE/mounts"
	echo "$path"
}
newest() { ls "$repo/snapshots" | sort | tail -n 1; }
case $command in
init)
	[ ! -e "$repo/config" ] || fail "config file already exists"
	[ ! -e "$STATE/init_fail" ] || fail "server refused"
	mkdir -p "$repo/snapshots" "$repo/locks"
	cp "$RESTIC_PASSWORD_FILE" "$repo/key"
	echo "$*" >"$repo/config"
	echo "created restic repository"
	;;
unlock)
	rm -f "$repo"/locks/*
	;;
backup)
	excludes=() targets=()
	while [ $# -gt 0 ]; do
		case $1 in
		--exclude) excludes+=("$2") && shift 2 ;;
		--host) [ "$2" = filebox ] && shift 2 || fail "host $2" ;;
		--quiet | --json) shift ;;
		-*) fail "unknown flag $1" ;;
		*) targets+=("$1") && shift ;;
		esac
	done
	[ ! -e "$STATE/backup_fail" ] || fail "connection lost"
	lock=$repo/locks/backup-$$
	touch "$lock"
	if [ -e "$STATE/hang" ]; then
		trap '' INT
		sleep 60
	elif [ -e "$STATE/slow" ]; then
		# restic installs its own SIGINT handler even when it starts with SIGINT ignored or blocked, as it is on some CI
		# runners. A bash trap cannot do that, so the waiting is done by python, exec'd so timeout signals it directly.
		exec python3 -c '
import os, signal, sys, time
lock, log = sys.argv[1], sys.argv[2]
def stop(*_):
    os.remove(lock)
    with open(log, "a") as f:
        f.write("interrupted\n")
    os._exit(130)
signal.pthread_sigmask(signal.SIG_UNBLOCK, {signal.SIGINT})
signal.signal(signal.SIGINT, stop)
time.sleep(60)
sys.exit(1)
' "$lock" "$T/restic.log"
	fi
	id=$(printf '%08x' $(($(ls "$repo/snapshots" | wc -l) + 1)))
	tree=$repo/snapshots/$id/tree
	mkdir -p "$tree"
	date --iso-8601=seconds >"$repo/snapshots/$id/time"
	for target in "${targets[@]}"; do
		if grep -qx "tmpfs $target" "$STATE/mounts"; then
			mkdir -p "$tree$target"
			while read -r kind src dst; do
				if [ "$kind" = bind-ro ] && [[ $dst == "$target"/* ]]; then
					mkdir -p "$(dirname "$tree$dst")"
					cp -a "$(resolve "$src")" "$tree$dst"
				fi
			done <"$STATE/mounts"
		else
			mkdir -p "$(dirname "$tree$target")"
			cp -a "$target" "$tree$target"
		fi
	done
	for pattern in "${excludes[@]}"; do
		regex=$(printf '%s' "$pattern" | sed -e 's/[].[^$+?(){}|]/\\&/g' -e 's/\*/[^\/]*/g')
		(cd "$tree" && find . -mindepth 1 | sed 's#^\.##') | grep -E "^$regex(/|\$)" | while IFS= read -r path; do
			rm -rf "$tree$path"
		done
	done
	rm -f "$lock"
	echo '{"message_type":"summary","data_added":'"$(du -sb "$tree" | cut -f1)"',"total_duration":0.5,"snapshot_id":"'"$id"'"}'
	[ ! -e "$STATE/incomplete" ] || exit 3
	;;
forget)
	[ -z "$(ls "$repo/locks")" ] || fail "unable to create lock in backend: repository is already locked"
	[ ! -e "$STATE/prune_fail" ] || fail "prune failed"
	;;
snapshots)
	if [ "${1:-}" = --json ]; then
		sep=""
		printf '['
		for id in $(ls "$repo/snapshots" | sort); do
			printf '%s{"time":"%s","id":"%s","short_id":"%s","hostname":"filebox"}' "$sep" "$(cat "$repo/snapshots/$id/time")" "$id" "$id"
			sep=,
		done
		printf ']\n'
	else
		for id in $(ls "$repo/snapshots" | sort); do echo "$id $(cat "$repo/snapshots/$id/time") filebox"; done
	fi
	;;
stats)
	echo '{"total_size":'"$(du -sb "$repo" | cut -f1)"',"snapshots_count":1}'
	;;
cat)
	[ "$1" = config ] || fail "unexpected restic cat $1"
	[ ! -e "$STATE/unreachable" ] || fail "unable to open repository: ssh: connect to host: no route to host"
	cat "$repo/config"
	;;
check)
	if [ -e "$STATE/check_damaged" ]; then
		echo "Pack ID does not match, want 1234abcd, got 9876fedc"
		fail "repository contains errors"
	fi
	if [ -e "$STATE/check_cut" ]; then
		echo "pack 1234abcd failed to download: ssh: connection lost"
		fail "repository contains errors"
	fi
	echo "no errors were found"
	;;
restore)
	id=$1
	shift
	includes=()
	while [ $# -gt 0 ]; do
		case $1 in
		--host) shift 2 ;;
		--target) target=$2 && shift 2 ;;
		--include) includes+=("$(printf '%s' "$2" | sed 's/\\\(.\)/\1/g')") && shift 2 ;;
		*) fail "unknown flag $1" ;;
		esac
	done
	printf '%s\n' "${includes[@]}" >"$T/restore.includes"
	if [ "$id" = latest ]; then id=$(newest); fi
	[ -d "$repo/snapshots/$id" ] || fail "no matching ID found for prefix $id"
	for include in "${includes[@]}"; do
		include=${include%/\*\*}
		if [ -e "$repo/snapshots/$id/tree$include" ] && [ ! -e "$target$include" ]; then
			mkdir -p "$target$(dirname "$include")"
			cp -a "$repo/snapshots/$id/tree$include" "$target$include"
		fi
	done
	;;
*) fail "unexpected restic $command" ;;
esac
RESTIC
sed -i "s#@T@#$T#; s#@STATE@#$STATE#" "$T/stubs/restic"
chmod +x "$T/stubs/restic"
export PATH="$T/stubs:$PATH"

fails=0
check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fails=$((fails + 1)); fi; }

run_backup() {
	ln -sfn "mnt:[host]" "$T/proc/self/ns/mnt"
	if [ -L "$T/run/filebox-backup/data" ]; then
		/bin/rm "$T/run/filebox-backup/data"
	fi
	: >"$STATE/mounts"
	: >"$T/restic.log"
	"$TOOL" run 2>&1
}
status_has() { grep -qE "$1" "$STATUS"; }
in_backup() { [ -e "$REPO/snapshots/$(ls "$REPO/snapshots" | sort | tail -n 1)/tree$1" ]; }
backed_up() { cat "$REPO/snapshots/$(ls "$REPO/snapshots" | sort | tail -n 1)/tree$1"; }
called() { grep -q -- "$1" "$T/restic.log"; }

TONIGHT=$(date -d '-30 minutes' +%Y-%m-%dT%H%M)
OLDER=$(date -d '-3 days' +%Y-%m-%dT%H%M)
printf 'amy\nbob\ncleo\n' >"$STATE/users"
mkdir -p "$BASE/amy/files/docs" "$BASE/amy/files/.trash" "$SNAP/amy/$OLDER/docs" "$SNAP/amy/$TONIGHT/docs"
echo "written after midnight" >"$BASE/amy/files/docs/new.txt"
echo older >"$SNAP/amy/$OLDER/docs/report.txt"
echo tonight >"$SNAP/amy/$TONIGHT/docs/report.txt"
for hidden in .trash .uploads .thumbs; do
	mkdir -p "$SNAP/amy/$TONIGHT/$hidden"
	echo private >"$SNAP/amy/$TONIGHT/$hidden/item"
done
mkdir -p "$BASE/bob/files/.trash" "$BASE/bob/files/projects/.trash"
echo bob >"$BASE/bob/files/notes.txt"
echo "a strange name" >"$BASE/bob/files/a*b[1].txt"
echo binned >"$BASE/bob/files/.trash/old.txt"
echo "his own folder" >"$BASE/bob/files/projects/.trash/keep.txt"
mkdir -p "$BASE/cleo/files" "$SNAP/cleo/$OLDER"
echo live >"$BASE/cleo/files/c.txt"
echo stale >"$SNAP/cleo/$OLDER/c.txt"
mkdir -p "$BASE/_family/photos" "$SNAP/_family/$TONIGHT/photos" "$SNAP/_family/$TONIGHT/.thumbs"
echo beach >"$SNAP/_family/$TONIGHT/photos/beach.jpg"
echo thumb >"$SNAP/_family/$TONIGHT/.thumbs/beach.t.jpg"
mkdir -p "$BASE/dave.deleted-20261101120000/files" "$SNAP/dave.deleted-20261101120000/$OLDER"
echo archived >"$BASE/dave.deleted-20261101120000/files/d.txt"
mkdir -p "$BASE/zed/files"
echo stray >"$BASE/zed/files/z.txt"
echo '{"amy":"admin"}' >"$T/var/lib/filebox/roles.json"
echo backup >"$T/etc/filebox/addons"

echo "--- before setup"
out=$(run_backup)
code=$?
check "the run ends quietly" '[ "$code" = 0 ] && [[ $out == *"not set up yet"* ]]'
check "the status says it is not set up" 'status_has "^state unconfigured$" && ! status_has "^destination"'
check "it ran in its own mount namespace and mounted nothing" 'grep -q "unshare --mount --propagation private" "$T/calls.log" && [ ! -s "$STATE/mounts" ]'
check "restic was not asked anything" '[ ! -s "$T/restic.log" ]'

echo "--- setup with another repository: S3 keys are asked for"
out=$(printf '2\ns3:https://s3.example.com/family-bucket\nAKIAEXAMPLE\nsecret-value\n' | "$TOOL" setup 2>&1)
check "it creates the repository" '[[ $out == *"The backup is set up"* ]] && grep -q "^RESTIC_REPOSITORY=s3:https://s3.example.com/family-bucket$" "$SECRETS/env"'
check "the keys are kept for the nightly runs and given to restic" 'grep -qx "AWS_ACCESS_KEY_ID=AKIAEXAMPLE" "$SECRETS/env" && grep -qx "AWS_SECRET_ACCESS_KEY=secret-value" "$SECRETS/env" && grep -qx "AWS_SECRET_ACCESS_KEY=secret-value" "$T/restic.env"'
check "the status names the host only" 'status_has "^destination s3 s3.example.com$" && ! grep -q "family-bucket\|AKIA\|secret" "$STATUS"'
check "the time of setup is recorded" 'status_has "^setup [0-9]+$"'
/bin/rm -f "$SECRETS/env" "$SECRETS/password"

echo "--- a password with a slash in a REST URL never reaches the panel"
out=$(printf '2\nrest:https://family:pa/ss@backup.example.com:8000/box\n' | "$TOOL" setup 2>&1)
check "only the host is shown" 'status_has "^destination rest backup.example.com$" && ! grep -q "pa/ss\|family:\|:pa\|ss@" "$STATUS"'
/bin/rm -f "$SECRETS/env" "$SECRETS/password"

echo "--- SFTP URLs without a user, or with a host that looks like an option, are refused"
for url in "sftp:backup.example.com:/filebox" "sftp://backup.example.com:23/filebox" "sftp://u1@-oProxyCommand=evil:23/x" "sftp://-V@host/x"; do
	out=$(printf '2\n%s\n' "$url" | "$TOOL" setup 2>&1) && echo "accepted: $url"
done >"$T/accepted"
check "none of them is set up" '[ ! -s "$T/accepted" ] && [ ! -e "$SECRETS/env" ] && ! grep -q -- "-oProxyCommand\|-V" "$T/calls.log"'

echo "--- a local folder on the system or data disk is refused"
out=$(printf '2\n%s\n' "$T/var/backups/restic" | "$TOOL" setup 2>&1)
check "it says why and creates nothing" '[[ $out == *"system or data disk"* ]] && [ ! -e "$SECRETS/env" ] && [ ! -e "$T/var/backups/restic" ]'

echo "--- setup that cannot create the repository"
touch "$STATE/init_fail"
out=$(printf '1\nu123456\n\n\nyes\n' | "$TOOL" setup 2>&1)
code=$?
/bin/rm "$STATE/init_fail"
check "it fails and says what to do" '[ "$code" != 0 ] && [[ $out == *"could not create the repository"* ]]'
check "nothing is left that looks set up" '[ ! -e "$SECRETS/env" ] && [ ! -e "$SECRETS/password" ]'

echo "--- setup with a Storage Box"
out=$(printf '1\nu123456\n\n\nyes\n' | "$TOOL" setup 2>&1)
code=$?
echo "$out" | sed 's/^/    /'
REPO=$T/remote/$(printf %s "sftp://u123456@u123456.your-storagebox.de:23/filebox" | tr -c 'A-Za-z0-9' _)
password=$(cat "$SECRETS/password")
check "it succeeds" '[ "$code" = 0 ] && [ -f "$REPO/config" ]'
check "the repository is a Storage Box folder over port 23" 'grep -qx "RESTIC_REPOSITORY=sftp://u123456@u123456.your-storagebox.de:23/filebox" "$SECRETS/env"'
check "the repository uses the format with compression" 'grep -q -- "--repository-version 2" "$REPO/config"'
check "the password is 48 random hex characters, printed exactly once" '[[ $password =~ ^[0-9a-f]{48}$ ]] && [ "$(grep -o "$password" <<<"$out" | wc -l)" = 1 ]'
check "secrets are root only" '[ "$(stat -c %a "$SECRETS")" = 700 ] && [ "$(stat -c %a "$SECRETS/env")" = 600 ] && [ "$(stat -c %a "$SECRETS/password")" = 600 ] && [ "$(stat -c %a "$SECRETS/known_hosts")" = 600 ]'
check "an SSH key is made and added with install-ssh-key" 'grep -q "ssh-keygen -q -t ed25519 -N  -C filebox-backup@box -f $SECRETS/ssh_key" "$T/calls.log" && grep -q "ssh -F none -p 23 .* -l u123456 u123456.your-storagebox.de install-ssh-key" "$T/calls.log" && grep -q AAAAC3testkey "$STATE/ssh_stdin"'
check "the host key is recorded and its fingerprint shown" 'grep -qx "\[u123456.your-storagebox.de\]:23 ssh-ed25519 AAAAC3hostkey" "$SECRETS/known_hosts" && [[ $out == *"SHA256:hostfingerprint"* ]]'
check "restic logs in with that key and only that host key" 'grep -qx "sftp.command=ssh -F none -i $SECRETS/ssh_key -o IdentitiesOnly=yes -o UserKnownHostsFile=$SECRETS/known_hosts -o StrictHostKeyChecking=yes -o BatchMode=yes -o ServerAliveInterval=60 -p 23 -l u123456 u123456.your-storagebox.de -s sftp" "$T/restic.options"'
check "the status names the host, not the user, path or password" 'status_has "^state on$" && status_has "^destination sftp u123456.your-storagebox.de$" && ! grep -q "u123456@\|filebox$\|$password" "$STATUS"'
out=$(printf '1\nu999\n\n\nyes\n' | "$TOOL" setup 2>&1)
check "a second setup is refused" '[[ $out == *"already set up"* ]] && grep -q u123456 "$SECRETS/env"'

echo "--- an unconfirmed host key stops setup"
mv "$SECRETS/env" "$T/env.saved"
out=$(printf '1\nu123456\n\n\nno\n' | "$TOOL" setup 2>&1)
check "it stops before anything is created" '[[ $out == *"host key was not confirmed"* ]] && [ ! -e "$SECRETS/env" ] && [ "$(cat "$SECRETS/password")" = "$password" ]'
mv "$T/env.saved" "$SECRETS/env"

echo "--- first night: the window ends before the first upload is done"
touch "$STATE/slow"
out=$(run_backup)
code=$?
/bin/rm "$STATE/slow"
echo "$out" | sed 's/^/    /'
check "the run ends without an error" '[ "$code" = 0 ]'
check "restic was stopped with SIGINT and removed its lock" 'called interrupted && [ -z "$(ls "$REPO/locks")" ]'
check "the status says stopped, with nothing known about the data added" 'status_has "^run [0-9]+ [0-9]+ stopped -$" && ! status_has "^latest"'
check "nothing was pruned or checked" '! called forget && ! called check'
check "stale locks are cleared before the backup" '[ "$(grep -n "restic unlock" "$T/restic.log" | cut -d: -f1)" -lt "$(grep -n "restic backup" "$T/restic.log" | cut -d: -f1)" ]'
check "mounts are made private first, then /srv/sftp becomes an empty tmpfs" '[ "$(head -n 3 "$STATE/mounts")" = "rprivate /
bind $BASE $T/run/filebox-backup/data
tmpfs $BASE" ]'
check "amy and the family folder are read from tonight's earlier versions" 'grep -qx "bind-ro $T/run/filebox-backup/data/.snapshots/amy/$TONIGHT $BASE/amy/files" "$STATE/mounts" && grep -qx "bind-ro $T/run/filebox-backup/data/.snapshots/_family/$TONIGHT $BASE/_family" "$STATE/mounts"'
check "bob has no earlier versions and cleo only a stale one, so their live folders are read" 'grep -qx "bind-ro $T/run/filebox-backup/data/bob/files $BASE/bob/files" "$STATE/mounts" && grep -qx "bind-ro $T/run/filebox-backup/data/cleo/files $BASE/cleo/files" "$STATE/mounts"'
check "deleted accounts and folders of no account are not mounted" '! grep -q "dave\|zed" "$STATE/mounts"'
check "the backup always names the same three paths" 'grep -q "restic backup --quiet --json --host filebox .* $T/etc/filebox $BASE $T/var/lib/filebox$" "$T/restic.log"'

echo "--- a run whose restic ignores the stop is killed, and the next night clears its lock"
touch "$STATE/hang"
out=$(run_backup)
/bin/rm "$STATE/hang"
check "it is reported as stopped" 'status_has "^run [0-9]+ [0-9]+ stopped -$"'
check "its lock is left in the repository" '[ -n "$(ls "$REPO/locks")" ]'
out=$(run_backup)
code=$?
echo "$out" | sed 's/^/    /'
check "the next night finishes" '[ "$code" = 0 ] && status_has "^run [0-9]+ [0-9]+ ok [0-9]+$"'
check "the stale lock was cleared, so pruning could lock the repository" 'called "forget --prune --host filebox --keep-daily 7 --keep-weekly 4 --keep-monthly 12"'
check "the newest backup, the repository size and the first check are recorded" 'status_has "^latest [0-9]+$" && status_has "^size [0-9]+ [0-9]+$" && status_has "^check [0-9]+ ok$" && called "check --read-data-subset 5%"'
check "restic keeps its cache and temporary files on disk, with a low GOGC" 'grep -qx "RESTIC_CACHE_DIR=$T/var/cache/filebox-backup" "$T/restic.env" && grep -qx "TMPDIR=$T/var/cache/filebox-backup/tmp" "$T/restic.env" && grep -qx "GOGC=20" "$T/restic.env"'
check "amy is backed up as she was tonight" '[ "$(backed_up "$BASE/amy/files/docs/report.txt")" = tonight ] && ! in_backup "$BASE/amy/files/docs/new.txt"'
check "without her Trash, unfinished uploads and thumbnails" '! in_backup "$BASE/amy/files/.trash" && ! in_backup "$BASE/amy/files/.uploads" && ! in_backup "$BASE/amy/files/.thumbs"'
check "bob and cleo come from their live folders" '[ "$(backed_up "$BASE/bob/files/notes.txt")" = bob ] && [ "$(backed_up "$BASE/cleo/files/c.txt")" = live ] && ! in_backup "$BASE/bob/files/.trash"'
check "a .trash folder a person made deeper down is kept" 'in_backup "$BASE/bob/files/projects/.trash/keep.txt"'
check "the family folder is in, without its thumbnails" '[ "$(backed_up "$BASE/_family/photos/beach.jpg")" = beach ] && ! in_backup "$BASE/_family/.thumbs"'
check "archived homes, earlier versions and stray folders are not" '! in_backup "$BASE/dave.deleted-20261101120000" && ! in_backup "$BASE/.snapshots" && ! in_backup "$BASE/zed"'
check "the panel data and /etc/filebox are in, the backup secrets are not" 'in_backup "$T/var/lib/filebox/roles.json" && in_backup "$T/etc/filebox/addons" && ! in_backup "$SECRETS"'
check "restic got the stable paths over SFTP with its own key" 'grep -q "sftp.command=ssh -F none -i $SECRETS/ssh_key" "$T/restic.options"'

month_old_check() { sed -i "s/^check [0-9]* \(ok\|failed\)$/check $(($(date +%s) - 31 * 86400)) \1/" "$STATUS"; }

echo "--- the next night neither checks nor measures again"
out=$(run_backup)
check "no check and no size a day after the last ones" 'status_has "^run [0-9]+ [0-9]+ ok [0-9]+$" && ! called check && ! called stats && status_has "^check [0-9]+ ok$"'

echo "--- the size is measured once a week"
sed -i "s/^size [0-9]* \([0-9]*\)$/size $(($(date +%s) - 8 * 86400)) \1/" "$STATUS"
measured=$(grep "^size " "$STATUS")
out=$(run_backup)
check "a week later it is measured again" 'called "stats --mode raw-data --json" && status_has "^size [0-9]+ [0-9]+$" && ! grep -qx "$measured" "$STATUS"'

echo "--- the monthly check finds damage"
month_old_check
touch "$STATE/check_damaged"
out=$(run_backup)
/bin/rm "$STATE/check_damaged"
check "a month later it runs, and the damage is recorded" 'called check && status_has "^check [0-9]+ failed$" && ! status_has "^checkerror" && status_has "^run [0-9]+ [0-9]+ ok [0-9]+$"'
out=$(run_backup)
check "after a failure it runs again the next night" 'called check && status_has "^check [0-9]+ ok$"'

echo "--- the monthly check cannot run to the end"
month_old_check
touch "$STATE/check_cut"
out=$(run_backup)
check "a pack that could not be downloaded is no damage" 'called check && status_has "^checkerror [0-9]+ 1$" && status_has "^check [0-9]+ ok$"'
since=$(sed -n "s/^checkerror \([0-9]*\) 1$/\1/p" "$STATUS")
out=$(run_backup)
out=$(run_backup)
check "it is tried every night and counted" 'called check && status_has "^checkerror $since 3$" && status_has "^check [0-9]+ ok$"'
/bin/rm "$STATE/check_cut"
touch "$STATE/check_damaged" "$STATE/unreachable"
out=$(run_backup)
/bin/rm "$STATE/check_damaged" "$STATE/unreachable"
check "errors from a repository that cannot be reached right after are no damage either" 'status_has "^checkerror $since 4$" && status_has "^check [0-9]+ ok$"'
out=$(run_backup)
check "a check that runs to the end clears the count" 'status_has "^check [0-9]+ ok$" && ! status_has "^checkerror"'

echo "--- files that could not be read, night after night"
touch "$STATE/incomplete"
out=$(run_backup)
code=$?
check "the run is incomplete, fails, still prunes, and counts the night" '[ "$code" != 0 ] && status_has "^run [0-9]+ [0-9]+ incomplete [0-9]+$" && called forget && status_has "^unreadable [0-9]+ 1$"'
since=$(sed -n "s/^unreadable \([0-9]*\) 1$/\1/p" "$STATUS")
out=$(run_backup)
/bin/rm "$STATE/incomplete"
check "the next such night counts on" 'status_has "^unreadable $since 2$"'

echo "--- the backup itself fails"
latest=$(grep "^latest " "$STATUS")
month_old_check
touch "$STATE/backup_fail"
out=$(run_backup)
code=$?
/bin/rm "$STATE/backup_fail"
check "it fails and is reported" '[ "$code" != 0 ] && status_has "^run [0-9]+ [0-9]+ backup_failed -$"'
check "nothing is pruned, the due check waits, and the newest backup is kept" '! called forget && ! called check && grep -qx "$latest" "$STATUS"'
check "a night without a backup leaves the count of unreadable nights alone" 'status_has "^unreadable $since 2$"'
out=$(run_backup)
check "a backup that reads every file ends the count" 'status_has "^run [0-9]+ [0-9]+ ok [0-9]+$" && ! status_has "^unreadable"'

echo "--- removing old backups fails"
touch "$STATE/prune_fail"
out=$(run_backup)
/bin/rm "$STATE/prune_fail"
check "it is reported" 'status_has "^run [0-9]+ [0-9]+ prune_failed [0-9]+$"'

echo "--- the accounts cannot be listed"
touch "$STATE/list_fail"
out=$(run_backup)
/bin/rm "$STATE/list_fail"
check "nothing is mounted or backed up" 'status_has "^run [0-9]+ [0-9]+ prepare_failed -$" && ! grep -q tmpfs "$STATE/mounts" && ! called backup'

echo "--- an empty account list"
mv "$STATE/users" "$STATE/users.saved"
: >"$STATE/users"
out=$(run_backup)
code=$?
check "fails the run instead of backing up the family folder alone" '[ "$code" != 0 ] && [[ $out == *"account list is empty"* ]] && status_has "^run [0-9]+ [0-9]+ prepare_failed -$" && ! called backup'

echo "--- an account with neither an earlier version nor a files folder"
printf 'amy\nbob\ncleo\neve\n' >"$STATE/users"
out=$(run_backup)
code=$?
mv "$STATE/users.saved" "$STATE/users"
check "fails the run and names the account" '[ "$code" != 0 ] && [[ $out == *"eve has neither"* ]] && status_has "^run [0-9]+ [0-9]+ prepare_failed -$" && ! called backup'

echo "--- a run while a restore holds the lock"
before=$(cat "$STATUS")
exec 8>"$T/run/filebox-backup.lock"
flock -n 8
out=$(run_backup)
code=$?
exec 8>&-
check "it is skipped and changes nothing" '[ "$code" = 0 ] && [[ $out == *"skipped"* ]] && [ "$(cat "$STATUS")" = "$before" ]'

echo "--- restore"
out=$("$RESTORE" list 2>&1)
check "list shows the backups" '[[ $out == *"00000001 "* ]] && grep -q "snapshots --host filebox --compact" "$T/restic.log"'
out=$("$RESTORE" user amy 2>&1)
code=$?
echo "$out" | sed 's/^/    /'
dest=$(sed -n 's/^Restored .* to //p' <<<"$out")
check "a person's files go to a new folder that only root can open" '[ "$code" = 0 ] && [[ $dest == "$BASE/.restored/"*-amy ]] && [ "$(stat -c %a "$BASE/.restored")" = 700 ]'
check "with what they had tonight, and nothing else" '[ "$(cat "$dest/docs/report.txt")" = tonight ] && [ ! -e "$dest/.trash" ] && [ ! -e "$dest.partial" ]'
check "restic is asked for the folder and for everything below it" '[ "$(cat "$T/restore.includes")" = "$BASE/amy/files
$BASE/amy/files/**" ]'
check "the live files are untouched" '[ "$(cat "$BASE/amy/files/docs/new.txt")" = "written after midnight" ]'
out=$("$RESTORE" family 00000003 --to "$T/out-family" 2>&1)
check "the family folder from a chosen backup, to a chosen folder" '[ "$(cat "$T/out-family/photos/beach.jpg")" = beach ]'
out=$("$RESTORE" path "$BASE/bob/files/a*b[1].txt" --to "$T/out-one" 2>&1)
check "one file, whose name has wildcard characters, keeps its name" '[ "$(cat "$T/out-one/a*b[1].txt")" = "a strange name" ] && [ "$(ls "$T/out-one")" = "a*b[1].txt" ]'
out=$("$RESTORE" path "$T/var/lib/filebox" --to "$T/out-panel" 2>&1)
check "the panel data" '[ -f "$T/out-panel/filebox/roles.json" ]'
out=$("$RESTORE" family --to "$T/out-family" 2>&1)
check "an existing folder is never restored into" '[[ $out == *"already exists"* ]] && [ ! -e "$T/out-family.partial" ]'
out=$("$RESTORE" user zed --to "$T/out-zed" 2>&1)
check "an account that is not in the backup is reported, and leaves nothing behind" '[[ $out == *"is not in backup latest"* ]] && [ ! -e "$T/out-zed" ] && [ ! -e "$T/out-zed.partial" ]'
for bad in "path $T/etc/passwd" "path $BASE/amy/files/../../cleo/files" "path $BASE/amy/files/" "path $BASE/dave.deleted-20261101120000/files" \
	"user ../amy" "user amy nightly" "family --to relative/folder"; do
	# shellcheck disable=SC2086 # the words of each case are separate arguments
	out=$("$RESTORE" $bad 2>&1) && echo "accepted: $bad"
done >"$T/accepted"
check "paths outside the backup, bad names, IDs and folders are refused" '[ ! -s "$T/accepted" ]'

echo "--- starting over with a folder on a disk of its own"
/bin/rm "$SECRETS/env"
mkdir -p "$T/usb-disk"
out=$(printf '2\n%s\n' "$T/usb-disk/backup" | "$TOOL" setup 2>&1)
check "the new repository starts without the old one's results" '[ -f "$T/usb-disk/backup/config" ] && status_has "^destination local -$" && status_has "^setup [0-9]+$" && ! status_has "^(run|latest|size|check|checkerror|unreadable) "'
touch "$STATE/usb_unmounted"
out=$(run_backup)
/bin/rm "$STATE/usb_unmounted"
check "a night when that disk is not mounted fails before anything is written" '[[ $out == *"system or data disk"* ]] && status_has "^run [0-9]+ [0-9]+ prepare_failed -$" && ! called backup && [ ! -s "$STATE/mounts" ]'

echo "--- arguments"
check "an unknown command is refused" '! "$TOOL" prune >/dev/null 2>&1 && ! "$RESTORE" everything >/dev/null 2>&1'
check "extra arguments are refused" '! "$TOOL" run now >/dev/null 2>&1'

echo
[ "$fails" = 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit "$fails"
