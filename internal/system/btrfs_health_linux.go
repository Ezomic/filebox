//go:build linux

package system

// Must match the sudoers rule written by deploy/setup.sh.
var btrfsHealthHelper = []string{"sudo", "-n", "/usr/local/sbin/filebox-disk-health"}

func ReadBtrfsHealth() (BtrfsHealth, error) {
	return readBtrfsHealthHelper(btrfsHealthHelper)
}
