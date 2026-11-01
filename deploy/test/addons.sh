#!/usr/bin/env bash
# Runs the add-on functions of deploy/setup.sh in a sandbox: the real shell logic, with the system
# commands (apt-get, systemctl, useradd, ...) stubbed and a fake release served over local HTTP.
# Run from anywhere: deploy/test/addons.sh
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEPLOY=$(dirname "$HERE")
SETUP=$DEPLOY/setup.sh
T=$(mktemp -d)
PIDS=()
trap 'for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; done; rm -rf "$T"' EXIT
mkdir -p "$T/stubs" "$T/etc/filebox" "$T/bin" "$T/deploy/systemd" "$T/srv"
cp "$DEPLOY/systemd/streambox.service" "$T/deploy/systemd/"
cp "$DEPLOY/streambox-version" "$T/deploy/"

# A release the script can download: a real executable plus its checksum.
RELDIR=$T/rel
mkdir -p "$RELDIR/good/download/v0.1.0" "$RELDIR/bad/download/v0.1.0"
(cd "$DEPLOY/.." && go build -o "$RELDIR/good/download/v0.1.0/streambox" ./deploy/test/fakestreambox) || exit 1
(cd "$RELDIR/good/download/v0.1.0" && sha256sum streambox >streambox.sha256)
cp "$RELDIR/good/download/v0.1.0/streambox" "$RELDIR/bad/download/v0.1.0/streambox"
echo "0000000000000000000000000000000000000000000000000000000000000000  streambox" >"$RELDIR/bad/download/v0.1.0/streambox.sha256"
serve() { # serve <dir> -> prints the port
	local port
	port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
	(cd "$1" && exec python3 -m http.server "$port" --bind 127.0.0.1 >"$1/http.log" 2>&1) &
	PIDS+=($!)
	for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$port/" && break; sleep 0.1; done
	echo "$port"
}
GOOD=http://127.0.0.1:$(serve "$RELDIR/good")
BAD=http://127.0.0.1:$(serve "$RELDIR/bad")

# Functions from setup.sh with every absolute path moved into the sandbox.
sed -n '/^# The add-ons asked for/,/^print_next_steps() {/p' "$SETUP" | sed '$d' |
	sed "s#/etc/systemd/system/#@@UNITDIR@@/#g; s#/etc/#$T/etc/#g; s#/srv/#$T/srv/#g; s#/usr/local/bin/#$T/bin/#g; s#/var/lib/#$T/var/lib/#g; s#@@UNITDIR@@#$T/etc/systemd-system#g" >"$T/funcs.sh"
# Sanity: nothing outside the sandbox may be referenced.
if grep -nE '(^|[ "=])/(etc|srv|usr|var)/' "$T/funcs.sh" | grep -v "$T" | grep -vE '^\s*[0-9]+:\s*#' | grep -vE 'systemd/system|/dev/|/usr/sbin/nologin|/etc/systemd'; then
	echo "UNSANDBOXED PATH ABOVE"
fi
mkdir -p "$T/etc/systemd-system"

stub() { printf '#!/bin/sh\necho "%s $*" >>"%s/calls.log"\n%s\n' "$1" "$T" "${2:-exit 0}" >"$T/stubs/$1"; chmod +x "$T/stubs/$1"; }
for c in apt-get systemctl usermod; do stub "$c"; done
mkdir -p "$T/state"
# getent, groupadd and useradd remember what exists and refuse what the real tools refuse, so a
# wrong flag combination fails here the way it does on a real box.
cat >"$T/stubs/getent" <<GETENT
#!/bin/sh
[ -e "$T/state/\$1-\$2" ] && exit 0
exit 2
GETENT
cat >"$T/stubs/groupadd" <<GROUPADD
#!/bin/sh
echo "groupadd \$*" >>"$T/calls.log"
for last; do :; done
if [ -e "$T/state/group-\$last" ]; then echo "groupadd: group '\$last' already exists" >&2; exit 9; fi
touch "$T/state/group-\$last"
GROUPADD
cat >"$T/stubs/useradd" <<USERADD
#!/bin/bash
echo "useradd \$*" >>"$T/calls.log"
user=\${!#}; ugroup=no; gid=""
args=("\$@")
for ((i = 0; i < \${#args[@]}; i++)); do
	case \${args[i]} in
	--user-group | -U) ugroup=yes ;;
	--gid | -g) gid=\${args[i + 1]} ;;
	esac
done
if [ -e "$T/state/passwd-\$user" ]; then echo "useradd: user '\$user' already exists" >&2; exit 9; fi
if [ "\$ugroup" = yes ] && [ -e "$T/state/group-\$user" ]; then
	echo "useradd: group \$user exists - if you want to add this user to that group, use -g." >&2; exit 9
fi
if [ -n "\$gid" ] && [ ! -e "$T/state/group-\$gid" ]; then echo "useradd: group '\$gid' does not exist" >&2; exit 6; fi
[ "\$ugroup" = yes ] && touch "$T/state/group-\$user"
echo "\${gid:-\$user}" >"$T/state/passwd-\$user"
USERADD
chmod +x "$T/stubs/getent" "$T/stubs/groupadd" "$T/stubs/useradd"
# install without ownership flags (the sandbox has no streambox group)
cat >"$T/stubs/install" <<EOF
#!/bin/bash
args=()
while [ \$# -gt 0 ]; do
	case \$1 in -o|-g) shift 2 ;; *) args+=("\$1"); shift ;; esac
done
exec /usr/bin/install "\${args[@]}"
EOF
chmod +x "$T/stubs/install"
export PATH="$T/stubs:$PATH"

fails=0
check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fails=$((fails + 1)); fi; }

run() { # run <extra env...> -- calls install_addons in a fresh shell, prints its stderr/stdout
	(
		set -euo pipefail
		SRC="$T/deploy"
		STREAMBOX_PORT=8090
		STREAMBOX_RELEASES=${REL:-$GOOD}
		KNOWN_ADDONS="streambox"
		die() { echo "die: $*" >&2; exit 1; }
		log() { echo "==> $*"; }
		# shellcheck disable=SC1090
		. "$T/funcs.sh"
		install_addons
		echo "install_addons returned $?"
	) 2>&1
}

echo "--- first install"
out=$(ADDONS=streambox run)
echo "$out" | sed 's/^/    /'
check "reports success" '[[ $out == *"install_addons returned 0"* ]]'
check "binary installed and runs" '[ "$("$T/bin/streambox" -version)" = v0.1.0 ]'
check "addons file remembers streambox" '[ "$(cat "$T/etc/filebox/addons")" = streambox ]'
env_file="$T/etc/streambox/streambox.env"
check "env file exists, mode 0640" '[ -f "$env_file" ] && [ "$(stat -c %a "$env_file")" = 640 ]'
admin=$(sed -n 's/^STREAMBOX_ADMIN_TOKEN=//p' "$env_file"); view=$(sed -n 's/^STREAMBOX_VIEW_TOKEN=//p' "$env_file")
check "tokens are 48 hex chars and differ" '[[ $admin =~ ^[0-9a-f]{48}$ && $view =~ ^[0-9a-f]{48}$ && $admin != "$view" ]]'
check "loopback listen, proxy trust, media dir" 'grep -q "^STREAMBOX_LISTEN=127.0.0.1:8090$" "$env_file" && grep -q "^STREAMBOX_TRUST_PROXY=1$" "$env_file" && grep -q "^STREAMBOX_MEDIA=$T/srv/streambox/media$" "$env_file"'
check "the streambox user exists and belongs to the streambox group" '[ "$(cat "$T/state/passwd-streambox")" = streambox ] && [ -e "$T/state/group-streambox" ]'
check "media dir created" '[ -d "$T/srv/streambox/media" ]'
check "unit installed" '[ -f "$T/etc/systemd-system/streambox.service" ]'
check "panel restarted and service enabled" 'grep -q "restart --no-block filebox-panel.service" "$T/calls.log" && grep -q "enable streambox.service" "$T/calls.log"'
check "ffmpeg installed via apt" 'grep -q "apt-get install.*ffmpeg" "$T/calls.log"'
check "filebox user joins the streambox group" 'grep -q "usermod -aG streambox filebox" "$T/calls.log"'

echo "--- second run (as filebox-update would): nothing regenerated or re-downloaded"
: >"$T/http-before"; wc -l <"$RELDIR/good/http.log" >"$T/http-before"
out=$(run)
check "still succeeds" '[[ $out == *"install_addons returned 0"* ]]'
check "tokens unchanged" '[ "$(sed -n "s/^STREAMBOX_ADMIN_TOKEN=//p" "$env_file")" = "$admin" ] && [ "$(sed -n "s/^STREAMBOX_VIEW_TOKEN=//p" "$env_file")" = "$view" ]'
check "no second download" '[ "$(wc -l <"$RELDIR/good/http.log")" = "$(cat "$T/http-before")" ]'
check "addons remembered without ADDONS" '[ "$(cat "$T/etc/filebox/addons")" = streambox ]'

echo "--- no add-on requested and none remembered"
rm -f "$T/etc/filebox/addons" "$T/calls.log"; touch "$T/calls.log"
out=$(run)
check "does nothing" '[[ $out == *"install_addons returned 0"* ]] && ! grep -q ffmpeg "$T/calls.log"'

echo "--- unknown add-on is refused"
out=$(ADDONS=bogus run || true)
check "dies with a clear message" '[[ $out == *"unknown add-on: bogus"* ]]'

echo "--- a failing step aborts before the service or panel restart"
rm -rf "$T/bin/streambox" "$T/etc/streambox" "$T/etc/systemd-system"; : >"$T/calls.log"
mkdir -p "$T/etc/systemd-system"; chmod 0555 "$T/etc/systemd-system"
if [ "$(id -u)" = 0 ]; then
	# root ignores directory permissions; make the unit path a file so install fails
	rmdir "$T/etc/systemd-system"; : >"$T/etc/systemd-system"
fi
out=$(ADDONS=streambox run)
check "unit install failure is reported, setup continues" '[[ $out == *"streambox add-on was NOT installed"* && $out == *"install_addons returned 0"* ]]'
check "service and panel were not restarted" '! grep -q "restart streambox.service" "$T/calls.log" && ! grep -q "restart --no-block filebox-panel.service" "$T/calls.log"'
rm -rf "$T/etc/systemd-system"; mkdir -p "$T/etc/systemd-system"; chmod 0755 "$T/etc/systemd-system"

echo "--- checksum mismatch: not installed, filebox continues"
rm -rf "$T/bin/streambox" "$T/etc/streambox"
out=$(ADDONS=streambox REL=$BAD run)
echo "$out" | sed 's/^/    /'
check "reports the checksum problem" '[[ $out == *"does not match its checksum"* ]]'
check "binary not installed" '[ ! -e "$T/bin/streambox" ]'
check "setup still returns 0 and says filebox is fine" '[[ $out == *"install_addons returned 0"* && $out == *"filebox itself is fine"* ]]'

echo "--- download failure"
out=$(ADDONS=streambox REL=http://127.0.0.1:1 run)
check "reports it and carries on" '[[ $out == *"could not download"* && $out == *"install_addons returned 0"* ]]'

echo "--- bad pinned version"
echo 'v1.2' >"$T/deploy/streambox-version"
out=$(ADDONS=streambox run)
check "refuses a malformed tag" '[[ $out == *"bad streambox version"* && $out == *"install_addons returned 0"* ]]'

echo
[ "$fails" = 0 ] && echo "ALL PASSED" || echo "$fails FAILED"
exit "$fails"
