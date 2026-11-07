#!/usr/bin/env bash
# Provisions a fresh Debian 12/13 box as a filebox: hardened SFTP, fail2ban,
# Tailscale, the admin panel, the DNS updater and the public user portal. Run as root; safe to re-run.
#   ./setup.sh /path/to/filebox-panel
#   ADDONS=streambox ./setup.sh /path/to/filebox-panel     also install the TV streaming add-on
# The chosen add-ons are remembered in /etc/filebox/addons, so filebox-update keeps them up to date.
#
# Get the binary from the latest GitHub release and check it before running:
#   curl -fLO https://github.com/Ezomic/filebox/releases/latest/download/filebox-panel
#   curl -fLO https://github.com/Ezomic/filebox/releases/latest/download/filebox-panel.sha256
#   sha256sum -c filebox-panel.sha256
set -euo pipefail

SFTP_PORT=${SFTP_PORT:-2222}
PANEL_PORT=8443
PORTAL_PORT=443
STREAMBOX_PORT=8090
STREAMBOX_RELEASES=${STREAMBOX_RELEASES:-https://github.com/Ezomic/streambox/releases}
KNOWN_ADDONS="streambox"
# Admin SSH stays on port 22 and is reachable from these networks until you
# have confirmed Tailscale access works (tailnet traffic is always allowed). A
# firewall that dropped port 22 before you could test the tailnet would lock
# you out of a headless box. Set to "" and re-run to drop LAN access.
LAN_SSH_CIDRS=${LAN_SSH_CIDRS-10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16}

SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

die() {
	echo "setup.sh: $*" >&2
	exit 1
}

log() {
	echo "==> $*"
}

preflight() {
	[ "$(id -u)" -eq 0 ] || die "run as root"
	[ $# -eq 1 ] || die "usage: $0 /path/to/filebox-panel"
	[ -f "$1" ] || die "panel binary not found: $1 (download it from https://github.com/Ezomic/filebox/releases, see the top of this script)"
	[ "$(head -c 4 "$1")" = $'\177ELF' ] || die "$1 is not a Linux executable"

	# shellcheck disable=SC1091
	. /etc/os-release
	[ "${ID:-}" = debian ] || die "Debian only"
	case ${VERSION_ID:-} in
	12 | 13) CODENAME=$VERSION_CODENAME ;;
	*) die "Debian 12 or 13 only" ;;
	esac

	[[ $SFTP_PORT =~ ^[0-9]+$ ]] && [ "$SFTP_PORT" -ge 1024 ] && [ "$SFTP_PORT" -le 65535 ] ||
		die "SFTP_PORT must be 1024-65535"
	[ "$SFTP_PORT" != "$PANEL_PORT" ] || die "SFTP_PORT clashes with the panel port"

	# Password login is switched off below, so a missing key would end in lockout.
	local has_key=0 u
	[ -s /root/.ssh/authorized_keys ] && has_key=1
	for u in $(getent group sudo | cut -d: -f4 | tr ',' ' '); do
		[ -s "$(getent passwd "$u" | cut -d: -f6)/.ssh/authorized_keys" ] && has_key=1
	done
	[ "$has_key" -eq 1 ] || die "no authorized_keys for root or a sudo user; add your SSH public key first"
}

install_packages() {
	log "Installing packages"
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq
	apt-get install -y -qq --no-install-recommends \
		ca-certificates curl gnupg jq sudo openssh-server fail2ban nftables python3 python3-systemd smartmontools unattended-upgrades btrfs-progs \
		libvips-tools ffmpeg libheif-plugin-libde265

	# Only in Debian 13: without it the panel works as before and the web portal stays off.
	apt-get install -y -qq --no-install-recommends lego ||
		echo "setup.sh: lego is not available on this Debian, the web portal certificate needs Debian 13" >&2

	log "Adding the Tailscale apt repository"
	install -d -m 0755 /usr/share/keyrings
	curl -fsSL "https://pkgs.tailscale.com/stable/debian/$CODENAME.noarmor.gpg" \
		-o /usr/share/keyrings/tailscale-archive-keyring.gpg
	echo "deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/debian $CODENAME main" \
		>/etc/apt/sources.list.d/tailscale.list
	apt-get update -qq
	apt-get install -y -qq tailscale
	systemctl enable --now tailscaled
}

create_accounts() {
	log "Creating groups, users and directories"
	getent group sftpusers >/dev/null || groupadd --system sftpusers
	getent passwd filebox >/dev/null ||
		useradd --system --user-group --no-create-home --home-dir /var/lib/filebox \
			--shell /usr/sbin/nologin filebox
	# sftpusers: reach the users' files directories (setgid, group-writable).
	# systemd-journal: read the sshd and panel logs.
	usermod -aG sftpusers filebox
	getent group systemd-journal >/dev/null && usermod -aG systemd-journal filebox

	install -d -o root -g root -m 0755 /srv/sftp
	# The shared Family folder sits beside the homes, so it is in nobody's quota. Only the panel touches it (SFTP users
	# are chrooted elsewhere), and the panel cannot create it itself because it cannot write to /srv/sftp.
	convert_family
	install -d -o filebox -g filebox -m 0770 /srv/sftp/_family
	install -d -o filebox -g filebox -m 0750 /var/lib/filebox
	# filebox-snapshot writes its status here; root owned, so the panel can read it but never swap it.
	install -d -o root -g filebox -m 0750 /var/lib/filebox-snapshot
	install -d -o root -g root -m 0750 /etc/filebox
	install -d -o root -g root -m 0755 /usr/local/libexec
}

# The family folder conversion builds the subvolume beside the live folder and swaps it in only once it checks out.
FAMILY_DIR=/srv/sftp/_family
FAMILY_NEW=/srv/sftp/_family.new
FAMILY_OLD=/srv/sftp/_family.pre-snapshots
PANEL_STOPPED=0

# used*100 >= total*percent for the data disk, like the panel's storage levels.
disk_at_least() {
	local total free
	read -r total free <<<"$(stat -f -c '%b %f' /srv/sftp)"
	((total > 0 && (total - free) * 100 >= total * $1))
}

# A btrfs subvolume root always has inode number 256.
is_subvolume() {
	[ -d "$1" ] && [ ! -L "$1" ] && [ "$(stat -c %i "$1")" = 256 ]
}

# Same columns for the live and the new family folder, so the copy can be checked before anything moves.
tree_listing() {
	(cd "$1" && find . -mindepth 1 \( -type d -printf 'd %m %u %g %p\n' \) -o \( ! -type d -printf '%y %m %u %g %s %T@ %p\n' \) | sort)
}

family_warning() {
	echo "setup.sh: WARNING: $*" >&2
}

discard_new_family() {
	if is_subvolume "$FAMILY_NEW"; then
		btrfs subvolume delete "$FAMILY_NEW" >/dev/null
	elif [ -e "$FAMILY_NEW" ] || [ -L "$FAMILY_NEW" ]; then
		rm -rf --one-file-system "$FAMILY_NEW"
	fi
}

put_family_back() {
	mv -T "$FAMILY_OLD" "$FAMILY_DIR" && chown filebox:filebox "$FAMILY_DIR" && chmod 0770 "$FAMILY_DIR"
}

# The portal writes into the family folder, so it is stopped while the copy is made, checked and swapped in. Every way
# out of convert_family starts it again, also when setup stops on an error halfway.
stop_panel_for_family() {
	systemctl is-active --quiet filebox-panel.service 2>/dev/null || return 0
	systemctl stop filebox-panel.service
	PANEL_STOPPED=1
	trap 'systemctl start --no-block filebox-panel.service' EXIT
}

start_panel_after_family() {
	[ "$PANEL_STOPPED" -eq 1 ] || return 0
	trap - EXIT
	PANEL_STOPPED=0
	systemctl start --no-block filebox-panel.service
}

# The copy is checked before anything moves, so a run that stopped between the two moves of the swap left a complete
# new folder, and the swap is finished. In any other case the old folder may be the only full copy.
resume_family_conversion() {
	if [ -e "$FAMILY_DIR" ] || [ -L "$FAMILY_DIR" ]; then
		family_warning "$FAMILY_OLD is left over from converting the family folder and may hold files that $FAMILY_DIR lacks. Compare the two before removing anything by hand."
	elif is_subvolume "$FAMILY_NEW"; then
		log "Finishing the interrupted conversion of the family folder"
		mv -T "$FAMILY_NEW" "$FAMILY_DIR"
		rm -rf --one-file-system "$FAMILY_OLD"
	else
		put_family_back
		family_warning "the family folder was found moved aside to $FAMILY_OLD and is put back; it is not converted yet"
	fi
}

build_and_swap_family() {
	if ! btrfs subvolume create "$FAMILY_NEW" >/dev/null; then
		family_warning "could not create the family subvolume; the family folder is unchanged"
		return 0
	fi
	if ! cp -a --reflink=always "$FAMILY_DIR/." "$FAMILY_NEW/" || ! chown filebox:filebox "$FAMILY_NEW" || ! chmod 0770 "$FAMILY_NEW" ||
		[ "$(tree_listing "$FAMILY_DIR")" != "$(tree_listing "$FAMILY_NEW")" ]; then
		discard_new_family || true
		family_warning "could not make a matching copy of the family folder; it is unchanged and the next update tries again"
		return 0
	fi
	if ! mv -T "$FAMILY_DIR" "$FAMILY_OLD"; then
		discard_new_family || true
		family_warning "could not move the family folder aside; it is unchanged"
		return 0
	fi
	if ! mv -T "$FAMILY_NEW" "$FAMILY_DIR"; then
		put_family_back
		discard_new_family || true
		family_warning "could not swap in the converted family folder; it is unchanged"
		return 0
	fi
	rm -rf --one-file-system "$FAMILY_OLD"
}

# The family folder becomes a btrfs subvolume once, so filebox-snapshot can keep earlier versions of it. The copy is
# built as _family.new with reflinks (quick, and almost no extra space) while the live folder stays as it is, checked
# against it, and only then swapped in; the old folder goes last. Never on a nearly full disk.
convert_family() {
	if [ "$(stat -f -c %T /srv/sftp)" != btrfs ] || ! command -v btrfs >/dev/null; then
		return 0
	fi
	if [ -e "$FAMILY_OLD" ] || [ -L "$FAMILY_OLD" ]; then
		resume_family_conversion
		return 0
	fi
	if ! discard_new_family; then
		family_warning "could not delete the leftover $FAMILY_NEW; the family folder is not converted"
		return 0
	fi
	if [ ! -e "$FAMILY_DIR" ] && [ ! -L "$FAMILY_DIR" ]; then
		btrfs subvolume create "$FAMILY_DIR" >/dev/null || family_warning "could not create the family folder as a subvolume"
		return 0
	fi
	if is_subvolume "$FAMILY_DIR"; then
		return 0
	fi
	if disk_at_least 95; then
		family_warning "the disk is at least 95% full, so the family folder was not converted and has no earlier versions yet. Free some space; the next update tries again."
		return 0
	fi

	log "Converting the family folder to a btrfs subvolume"
	stop_panel_for_family
	build_and_swap_family
	start_panel_after_family
}

configure_sshd() {
	log "Configuring OpenSSH"
	grep -q '^Include /etc/ssh/sshd_config.d/\*.conf' /etc/ssh/sshd_config ||
		die "/etc/ssh/sshd_config does not include sshd_config.d; refusing to guess"
	install -d -m 0755 /run/sshd

	local dropin=/etc/ssh/sshd_config.d/00-filebox.conf backup=""
	if [ -f "$dropin" ]; then
		backup=$(mktemp)
		cp -p "$dropin" "$backup"
	fi

	# Must sort first: sshd keeps the first value it sees for each keyword.
	cat >"$dropin" <<EOF
# Managed by filebox setup.sh
Port 22
Port $SFTP_PORT
DenyUsers filebox
PermitRootLogin prohibit-password
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
PermitEmptyPasswords no

# The internet-facing port serves SFTP users only. Admin shells use port 22,
# which the firewall limits to the LAN and the tailnet; SFTP users get no
# password surface there.
Match LocalPort $SFTP_PORT
    AllowGroups sftpusers

Match LocalPort 22
    DenyGroups sftpusers

# These settings confine SFTP users: the chroot is their root-owned home, the
# forced command is the SFTP server only (no shell, whatever the client asks
# for), and forwarding, TTYs and key files are all off.
Match Group sftpusers
    ChrootDirectory %h
    ForceCommand internal-sftp -d /files -u 0007
    PasswordAuthentication yes
    AuthorizedKeysFile none
    PermitTTY no
    AllowTcpForwarding no
    AllowStreamLocalForwarding no
    AllowAgentForwarding no
    GatewayPorts no
    PermitTunnel no
    X11Forwarding no
EOF

	if ! sshd -t; then
		if [ -n "$backup" ]; then cp -p "$backup" "$dropin"; else rm -f "$dropin"; fi
		die "sshd rejected the new configuration, reverted"
	fi
	rm -f "$backup"

	# With socket activation sshd ignores Port, so switch to the plain service.
	if systemctl is-enabled --quiet ssh.socket 2>/dev/null || systemctl is-active --quiet ssh.socket 2>/dev/null; then
		systemctl mask --now ssh.socket
		systemctl enable ssh.service
		systemctl restart ssh.service
	else
		systemctl enable ssh.service
		systemctl reload-or-restart ssh.service
	fi
	local p
	for p in 22 "$SFTP_PORT"; do
		# A reload briefly closes and reopens the listening sockets.
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			ss -ltn "sport = :$p" | grep -q LISTEN && break
			sleep 0.5
		done
		ss -ltn "sport = :$p" | grep -q LISTEN || die "sshd is not listening on port $p"
	done
}

configure_fail2ban() {
	log "Configuring fail2ban"
	cat >/etc/fail2ban/jail.d/filebox.local <<EOF
[DEFAULT]
backend = systemd
banaction = nftables
banaction_allports = nftables[type=allports]
ignoreip = 127.0.0.1/8 ::1 100.64.0.0/10

[sshd]
enabled = true
port = 22,$SFTP_PORT
maxretry = 5
findtime = 10m
bantime = 1h

[filebox-portal]
enabled = true
filter = filebox-portal
journalmatch = _SYSTEMD_UNIT=filebox-panel.service
port = $PORTAL_PORT
maxretry = 10
findtime = 10m
bantime = 1h
EOF
	# The portal logs the client address last on each failure line; anchoring on the end keeps a
	# crafted username from planting a different address earlier in the line.
	cat >/etc/fail2ban/filter.d/filebox-portal.conf <<'EOF'
[Definition]
failregex = portal: (login|password change|tv) (failed|blocked) (user=.* )?ip=<HOST>$
EOF
	systemctl enable --now fail2ban
	fail2ban-client reload >/dev/null
}

configure_firewall() {
	log "Configuring the firewall"
	local lan_rule=""
	if [ -n "$LAN_SSH_CIDRS" ]; then
		lan_rule="ip saddr { $LAN_SSH_CIDRS } tcp dport 22 accept"
	fi

	# Own table only, so Tailscale's and fail2ban's rules are left alone.
	cat >/etc/nftables.conf <<EOF
#!/usr/sbin/nft -f
# Managed by filebox setup.sh
table inet filebox
delete table inet filebox

table inet filebox {
	chain input {
		type filter hook input priority 0; policy drop;
		iif lo accept
		ct state established,related accept
		ct state invalid drop
		iifname "tailscale0" accept
		meta l4proto { icmp, ipv6-icmp } accept
		udp sport 67 udp dport 68 accept
		udp sport 547 udp dport 546 accept
		tcp dport $SFTP_PORT accept
		meta nfproto ipv4 tcp dport $PORTAL_PORT accept
		ip6 saddr { fe80::/10, fc00::/7 } tcp dport 22 accept
		$lan_rule
	}
}
EOF
	nft -c -f /etc/nftables.conf
	nft -f /etc/nftables.conf
	systemctl enable nftables
}

install_helpers() {
	log "Installing the user helper and sudo rule"
	install -m 0750 -o root -g filebox "$SRC/filebox-user" /usr/local/sbin/filebox-user
	install -m 0750 -o root -g filebox "$SRC/filebox-smart" /usr/local/sbin/filebox-smart
	install -m 0750 -o root -g filebox "$SRC/filebox-verify" /usr/local/sbin/filebox-verify
	install -m 0750 -o root -g root "$SRC/filebox-dns-update" /usr/local/sbin/filebox-dns-update
	install -m 0750 -o root -g root "$SRC/filebox-portal-cert" /usr/local/sbin/filebox-portal-cert
	install -m 0750 -o root -g root "$SRC/filebox-update" /usr/local/sbin/filebox-update
	install -m 0750 -o root -g root "$SRC/filebox-snapshot" /usr/local/sbin/filebox-snapshot
	install -m 0755 -o root -g root "$SRC/filebox-panel-start" /usr/local/libexec/filebox-panel-start

	local tmp
	tmp=$(mktemp)
	# use_pty off: a pty would mangle the password the panel pipes to the helper.
	cat >"$tmp" <<'EOF'
Defaults:filebox !use_pty, !lecture, timestamp_timeout=0
filebox ALL=(root) NOPASSWD: /usr/local/sbin/filebox-user list, \
	/usr/local/sbin/filebox-user create *, \
	/usr/local/sbin/filebox-user delete *, \
	/usr/local/sbin/filebox-user passwd *, \
	/usr/local/sbin/filebox-user quota-list, \
	/usr/local/sbin/filebox-user quota-set *, \
	/usr/local/sbin/filebox-user quota-clear *, \
	/usr/local/sbin/filebox-verify *, \
	/usr/bin/systemctl restart ssh, \
	/usr/bin/systemctl poweroff, \
	/usr/bin/systemctl reboot, \
	/usr/local/sbin/filebox-smart
EOF
	visudo -cf "$tmp" >/dev/null || die "generated sudoers rule is invalid"
	install -m 0440 -o root -g root "$tmp" /etc/sudoers.d/filebox
	rm -f "$tmp"
}

install_services() {
	log "Installing the panel and DNS updater"
	install -m 0755 -o root -g root "$PANEL_BINARY" /usr/local/bin/filebox-panel.new
	mv -f /usr/local/bin/filebox-panel.new /usr/local/bin/filebox-panel

	install -m 0644 -o root -g root "$SRC"/systemd/filebox-panel.service \
		"$SRC"/systemd/filebox-dns.service "$SRC"/systemd/filebox-dns.timer \
		"$SRC"/systemd/filebox-portal-cert.service "$SRC"/systemd/filebox-portal-cert.timer \
		"$SRC"/systemd/filebox-scrub.service "$SRC"/systemd/filebox-scrub.timer \
		"$SRC"/systemd/filebox-snapshot.service "$SRC"/systemd/filebox-snapshot.timer \
		"$SRC"/systemd/filebox-snapshot-check.service "$SRC"/systemd/filebox-snapshot-check.timer /etc/systemd/system/
	systemctl daemon-reload
	systemctl enable filebox-panel.service filebox-dns.timer filebox-portal-cert.timer filebox-scrub.timer \
		filebox-snapshot.timer filebox-snapshot-check.timer
	# The panel cannot start until Tailscale has an address, so do not wait on it.
	systemctl restart --no-block filebox-panel.service
	systemctl restart filebox-dns.timer filebox-portal-cert.timer filebox-scrub.timer filebox-snapshot.timer filebox-snapshot-check.timer
	# Writes the first status for the panel now; the first earlier versions are taken tonight.
	systemctl start --no-block filebox-snapshot-check.service
}

# The add-ons asked for, one per line in /etc/filebox/addons. ADDONS (comma separated) adds to them.
resolve_addons() {
	local file=/etc/filebox/addons wanted="" name
	[ -f "$file" ] && wanted=$(cat "$file")
	wanted="$wanted"$'\n'"$(echo "${ADDONS:-}" | tr ',' '\n')"
	ADDON_LIST=""
	for name in $(echo "$wanted" | tr -s ' \n' '\n\n' | sort -u); do
		[[ " $KNOWN_ADDONS " == *" $name "* ]] || die "unknown add-on: $name (known: $KNOWN_ADDONS)"
		ADDON_LIST="$ADDON_LIST $name"
	done
	# shellcheck disable=SC2086
	printf '%s\n' $ADDON_LIST >"$file.new"
	install -m 0600 -o root -g root "$file.new" "$file"
	rm -f "$file.new"
}

has_addon() {
	[[ " $ADDON_LIST " == *" $1 "* ]]
}

random_token() {
	head -c 24 /dev/urandom | od -An -vtx1 | tr -d ' \n'
}

# Installs the streambox release pinned in deploy/streambox-version. Returns non-zero on any
# problem instead of exiting: a failed add-on must not undo a working filebox. It is called
# from an || list, where set -e is off, so every step that matters ends in || return 1.
install_streambox() {
	log "Installing the streambox add-on"
	local tag tmp base file current=""
	tag=$(tr -d ' \n' <"$SRC/streambox-version") || return 1
	[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
		echo "setup.sh: bad streambox version: $tag" >&2
		return 1
	}

	apt-get install -y -qq --no-install-recommends ffmpeg || return 1

	getent group streambox >/dev/null || groupadd --system streambox || return 1
	getent passwd streambox >/dev/null ||
		useradd --system --gid streambox --no-create-home --home-dir /var/lib/streambox \
			--shell /usr/sbin/nologin streambox || return 1
	# The panel reads the tokens in /etc/streambox to talk to the add-on.
	usermod -aG streambox filebox || return 1
	install -d -o root -g streambox -m 0750 /etc/streambox || return 1
	# Videos for the looped channels go here; the add-on can read but not change them.
	install -d -o root -g streambox -m 0750 /srv/streambox /srv/streambox/media || return 1

	if [ ! -f /etc/streambox/streambox.env ]; then
		tmp=$(mktemp) || return 1
		cat >"$tmp" <<ENV
# Managed by filebox setup.sh; the tokens are generated once and kept.
STREAMBOX_ADMIN_TOKEN=$(random_token)
STREAMBOX_VIEW_TOKEN=$(random_token)
STREAMBOX_LISTEN=127.0.0.1:$STREAMBOX_PORT
STREAMBOX_MEDIA=/srv/streambox/media
# The panel and the portal sit in front of it and set the X-Forwarded-* headers.
STREAMBOX_TRUST_PROXY=1
ENV
		install -m 0640 -o root -g streambox "$tmp" /etc/streambox/streambox.env || return 1
		rm -f "$tmp"
	fi

	[ -x /usr/local/bin/streambox ] && current=$(/usr/local/bin/streambox -version 2>/dev/null || true)
	if [ "$current" != "$tag" ]; then
		tmp=$(mktemp -d) || return 1
		base=$STREAMBOX_RELEASES/download/$tag
		for file in streambox streambox.sha256; do
			curl -fsSL --max-time 300 -o "$tmp/$file" "$base/$file" || {
				echo "setup.sh: could not download $file from streambox $tag" >&2
				rm -rf "$tmp"
				return 1
			}
		done
		(cd "$tmp" && sha256sum -c streambox.sha256 >/dev/null) || {
			echo "setup.sh: streambox $tag does not match its checksum, not installed" >&2
			rm -rf "$tmp"
			return 1
		}
		[ "$(head -c 4 "$tmp/streambox")" = $'\177ELF' ] || {
			echo "setup.sh: streambox $tag is not a Linux executable" >&2
			rm -rf "$tmp"
			return 1
		}
		install -m 0755 -o root -g root "$tmp/streambox" /usr/local/bin/streambox.new || return 1
		mv -f /usr/local/bin/streambox.new /usr/local/bin/streambox || return 1
		rm -rf "$tmp"
	fi

	install -m 0644 -o root -g root "$SRC/systemd/streambox.service" /etc/systemd/system/streambox.service || return 1
	systemctl daemon-reload || return 1
	systemctl enable streambox.service || return 1
	systemctl restart streambox.service || return 1
	# The panel picks the add-on up when it starts, and now has the streambox group.
	systemctl restart --no-block filebox-panel.service
}

install_addons() {
	resolve_addons
	if has_addon streambox; then
		install_streambox || echo "setup.sh: the streambox add-on was NOT installed; filebox itself is fine. Fix the cause above and re-run: filebox-update" >&2
	fi
}

print_next_steps() {
	if [ -n "${FILEBOX_UPDATE:-}" ]; then
		echo "Update finished. Roll back with: filebox-update rollback"
		return
	fi
	cat <<EOF

Setup finished. Do these in order:

1. Join the tailnet (opens a login URL):
     tailscale up

2. Open the panel from another device on your tailnet and set the admin password.
   /setup asks for a one-time token; read it on this machine with:
     cat /var/lib/filebox/setup-token
     https://<output of: tailscale ip -4>:$PANEL_PORT/setup
   (the certificate is self-signed, expect a browser warning)

3. Add a domain-scoped DigitalOcean token (domain records only) for DNS updates:
     umask 077 && read -rsp 'token: ' t && printf %s "\$t" > /etc/filebox/do-token; unset t
     systemctl start filebox-dns.service

4. On your router forward only TCP $SFTP_PORT and TCP $PORTAL_PORT (IPv4) to this machine. Never forward $PANEL_PORT.

5. Get the certificate for the web portal at https://files.thijssensoftware.nl (needs step 3 first; running
   this accepts the Let's Encrypt terms of service and uses your email for expiry notices):
     filebox-portal-cert issue you@example.com
   Renewal is automatic (filebox-portal-cert.timer) and the panel picks the new certificate up by itself. The
   dashboard shows the days left.

6. Once you have confirmed you can SSH in over the tailnet, re-run with LAN_SSH_CIDRS=""
   to close port 22 to the LAN:
     LAN_SSH_CIDRS="" ./setup.sh /path/to/filebox-panel
EOF
	if has_addon streambox; then
		cat <<TV

TV add-on (streambox): open the panel's TV page to add channels. Copy video files for looped channels to
/srv/streambox/media. Family members find their playlist link under TV in the web portal.
TV
	fi
}

configure_auto_updates() {
	log "Enabling automatic security updates"
	cat >/etc/apt/apt.conf.d/20auto-upgrades <<'CONF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
CONF
	cat >/etc/apt/apt.conf.d/52filebox-unattended-upgrades <<'CONF'
Unattended-Upgrade::Automatic-Reboot "false";
Unattended-Upgrade::Remove-Unused-Dependencies "true";
CONF
}

main() {
	preflight "$@"
	PANEL_BINARY=$1
	install_packages
	create_accounts
	configure_auto_updates
	configure_sshd
	configure_fail2ban
	configure_firewall
	install_helpers
	install_services
	install_addons
	print_next_steps
}

main "$@"
