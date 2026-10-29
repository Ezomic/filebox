#!/usr/bin/env bash
# Provisions a fresh Debian 12/13 box as a filebox: hardened SFTP, fail2ban,
# Tailscale, the admin panel and the DNS updater. Run as root; safe to re-run.
#   ./setup.sh /path/to/filebox-panel
set -euo pipefail

SFTP_PORT=${SFTP_PORT:-2222}
PANEL_PORT=8443
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
	[ -f "$1" ] || die "panel binary not found: $1"
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
		ca-certificates curl gnupg jq sudo openssh-server fail2ban nftables python3-systemd

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
	install -d -o filebox -g filebox -m 0750 /var/lib/filebox
	install -d -o root -g root -m 0750 /etc/filebox
	install -d -o root -g root -m 0755 /usr/local/libexec
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
	install -m 0750 -o root -g root "$SRC/filebox-dns-update" /usr/local/sbin/filebox-dns-update
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
	/usr/bin/systemctl restart ssh, \
	/usr/bin/systemctl poweroff, \
	/usr/bin/systemctl reboot
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
		"$SRC"/systemd/filebox-dns.service "$SRC"/systemd/filebox-dns.timer /etc/systemd/system/
	systemctl daemon-reload
	systemctl enable filebox-panel.service filebox-dns.timer
	# The panel cannot start until Tailscale has an address, so do not wait on it.
	systemctl restart --no-block filebox-panel.service
	systemctl restart filebox-dns.timer
}

print_next_steps() {
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

4. On your router forward only TCP $SFTP_PORT to this machine.

5. Once you have confirmed you can SSH in over the tailnet, re-run with LAN_SSH_CIDRS=""
   to close port 22 to the LAN:
     LAN_SSH_CIDRS="" ./setup.sh /path/to/filebox-panel
EOF
}

main() {
	preflight "$@"
	PANEL_BINARY=$1
	install_packages
	create_accounts
	configure_sshd
	configure_fail2ban
	configure_firewall
	install_helpers
	install_services
	print_next_steps
}

main "$@"
