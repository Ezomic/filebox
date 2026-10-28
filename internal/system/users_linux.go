//go:build linux

package system

const helperPath = "/usr/local/sbin/filebox-user"

func NewUsers() Users {
	return &execUsers{argv: []string{"sudo", "-n", helperPath}}
}
