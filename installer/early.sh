#!/bin/sh
# Runs inside the Debian installer before partitioning (partman/early_command).
# Frees the one partition the wizard chose and, if asked, prepares the data partition.
# Partitions are matched by PARTUUID, never by device name, and every refusal stops the install.
# Settings come from /filebox.conf: TARGET_PARTUUID, TARGET_FSTYPE, DATA_PARTUUID, DATA_ACTION
# (keep, format or none) and CLEANUP_PARTUUID (optional, the wizard's own staging partition).

. /filebox.conf

say() {
	logger -t filebox-early "$*"
	echo "filebox-early: $*" >&2
}

# The installer only shows a generic "command failed" box, so the reason is also printed on the
# console, behind that box.
stop() {
	say "REFUSING TO CONTINUE: $*"
	echo "filebox: $*" >/dev/tty1 2>/dev/null
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || anna-install "$2" >/dev/null 2>&1
	command -v "$1" >/dev/null 2>&1 || stop "missing tool $1 (package $2)"
}

device_of() {
	blkid | grep -i "PARTUUID=\"$1\"" | head -n 1 | cut -d: -f1
}

fstype_of() {
	blkid "$1" | sed -n 's/.* TYPE="\([^"]*\)".*/\1/p'
}

part_number() {
	cat "/sys/class/block/$(basename "$1")/partition"
}

disk_of() {
	basename "$(readlink -f "/sys/class/block/$(basename "$1")/..")" | sed 's#^#/dev/#'
}

need parted parted-udeb
[ -n "$TARGET_PARTUUID" ] || stop "no target partition configured"
TARGET=$(device_of "$TARGET_PARTUUID")
[ -b "$TARGET" ] || stop "target partition $TARGET_PARTUUID not found"

for protected in $DATA_PARTUUID; do
	[ "$protected" != "$TARGET_PARTUUID" ] || stop "the target partition is the data partition"
done

found=$(fstype_of "$TARGET")
[ "$found" = "$TARGET_FSTYPE" ] || stop "target is $found, the wizard saw $TARGET_FSTYPE"

DISK=$(disk_of "$TARGET")
NUMBER=$(part_number "$TARGET")
[ -b "$DISK" ] && [ -n "$NUMBER" ] || stop "could not work out the disk of $TARGET"

case $DATA_ACTION in
keep)
	DATA=$(device_of "$DATA_PARTUUID")
	[ -b "$DATA" ] || stop "data partition $DATA_PARTUUID not found"
	kind=$(fstype_of "$DATA")
	case $kind in
	ext4 | ext3 | xfs | btrfs) say "keeping data partition $DATA ($kind)" ;;
	*) stop "data partition is $kind, which filebox cannot use; choose format or copy the data off first" ;;
	esac
	;;
format)
	DATA=$(device_of "$DATA_PARTUUID")
	[ -b "$DATA" ] || stop "data partition $DATA_PARTUUID not found"
	[ "$DATA" != "$TARGET" ] || stop "data and target are the same partition"
	need mkfs.btrfs btrfs-progs-udeb
	say "formatting data partition $DATA as btrfs"
	mkfs.btrfs -f -L filebox-data "$DATA" || stop "mkfs failed on $DATA"
	;;
none) say "no data partition, files will live on the system partition" ;;
*) stop "unknown data action: $DATA_ACTION" ;;
esac

if [ -n "$CLEANUP_PARTUUID" ]; then
	STAGING=$(device_of "$CLEANUP_PARTUUID")
	if [ -b "$STAGING" ] && [ "$STAGING" != "$TARGET" ]; then
		for protected in $DATA_PARTUUID; do
			[ "$CLEANUP_PARTUUID" != "$protected" ] || stop "the staging partition is the data partition"
		done
		say "removing the wizard's staging partition $STAGING"
		parted -s "$(disk_of "$STAGING")" rm "$(part_number "$STAGING")" || stop "could not remove staging partition"
	fi
fi

# Guided partitioning uses the largest free region, so no other free space on the disk may be bigger
# than the partition being replaced, or Debian would be installed somewhere else.
target_bytes=$(($(cat "/sys/class/block/$(basename "$TARGET")/size") * 512))
for block in /sys/block/*; do
	disk=/dev/$(basename "$block")
	case $disk in /dev/loop* | /dev/ram* | /dev/sr* | /dev/fd*) continue ;; esac
	[ -b "$disk" ] || continue
	for free in $(parted -s "$disk" unit B print free 2>/dev/null | grep 'Free Space' | awk '{print $3}' | tr -d B); do
		[ "$free" -le "$target_bytes" ] || stop "there is $free bytes of other free space on $disk, larger than the partition being replaced"
	done
done

# Partition numbers can shift once the staging partition is gone, so look the target up again.
TARGET=$(device_of "$TARGET_PARTUUID")
[ -b "$TARGET" ] || stop "target partition vanished"
NUMBER=$(part_number "$TARGET")
say "removing partition $NUMBER ($TARGET) from $DISK to make room for Debian"
parted -s "$DISK" rm "$NUMBER" || stop "could not remove $TARGET"
say "done"
