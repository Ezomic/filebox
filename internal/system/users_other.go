//go:build !linux

package system

func NewUsers() Users {
	return NewFakeUsers("alice", "bob", "family-share")
}
