//go:build linux

package system

// These argument lists must match the sudoers rule written by deploy/setup.sh exactly.
func NewPower() Power {
	return &execPower{commands: map[PowerAction][]string{
		RestartSSHD: {"sudo", "-n", "/usr/bin/systemctl", "restart", "ssh"},
		Shutdown:    {"sudo", "-n", "/usr/bin/systemctl", "poweroff"},
		Reboot:      {"sudo", "-n", "/usr/bin/systemctl", "reboot"},
	}}
}
