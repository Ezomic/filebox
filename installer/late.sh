#!/bin/sh
# Runs inside the Debian installer after the base system is installed (preseed/late_command).
# Mounts the data partition at /srv/sftp, installs the SSH key, and schedules the first-boot
# filebox setup. Settings come from /filebox.conf.

. /filebox.conf

say() {
	logger -t filebox-late "$*"
	echo "filebox-late: $*" >&2
}

device_of() {
	blkid | grep -i "PARTUUID=\"$1\"" | head -n 1 | cut -d: -f1
}

if [ "$DATA_ACTION" != "none" ] && [ -n "$DATA_PARTUUID" ]; then
	DATA=$(device_of "$DATA_PARTUUID")
	UUID=$(blkid "$DATA" | sed -n 's/.* UUID="\([^"]*\)".*/\1/p')
	KIND=$(blkid "$DATA" | sed -n 's/.* TYPE="\([^"]*\)".*/\1/p')
	if [ -n "$UUID" ] && [ -n "$KIND" ]; then
		mkdir -p /target/srv/sftp
		echo "UUID=$UUID /srv/sftp $KIND defaults,nofail 0 2" >>/target/etc/fstab
		say "data partition mounted at /srv/sftp by UUID $UUID"
	else
		say "WARNING: could not read the data partition, /srv/sftp is on the system partition"
	fi
fi

if [ -n "$SSH_KEY" ]; then
	install -d -m 0700 "/target/home/$FILEBOX_USER/.ssh"
	echo "$SSH_KEY" >"/target/home/$FILEBOX_USER/.ssh/authorized_keys"
	chmod 0600 "/target/home/$FILEBOX_USER/.ssh/authorized_keys"
	chroot /target chown -R "$FILEBOX_USER:$FILEBOX_USER" "/home/$FILEBOX_USER/.ssh"
fi

install -m 0750 /filebox-update /target/usr/local/sbin/filebox-update
install -m 0644 /filebox-firstboot.service /target/etc/systemd/system/filebox-firstboot.service
mkdir -p /target/var/lib/filebox-firstboot /target/etc/systemd/system/multi-user.target.wants
touch /target/var/lib/filebox-firstboot/pending
ln -sf /etc/systemd/system/filebox-firstboot.service /target/etc/systemd/system/multi-user.target.wants/filebox-firstboot.service
say "first-boot setup scheduled"
