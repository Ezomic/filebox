package system

import (
	"errors"
	"sync"
)

type PowerAction string

const (
	RestartSSHD PowerAction = "restart-sshd"
	Shutdown    PowerAction = "shutdown"
	Reboot      PowerAction = "reboot"
)

var (
	ErrInvalidPowerAction = errors.New("unknown power action")
	ErrPowerFailed        = errors.New("power command failed")
)

type Power interface {
	Do(action PowerAction) error
}

func ParsePowerAction(value string) (PowerAction, bool) {
	switch action := PowerAction(value); action {
	case RestartSSHD, Shutdown, Reboot:
		return action, true
	}

	return "", false
}

type FakePower struct {
	mu      sync.Mutex
	actions []PowerAction
	Err     error
}

func (f *FakePower) Do(action PowerAction) error {
	if _, ok := ParsePowerAction(string(action)); !ok {
		return ErrInvalidPowerAction
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, action)

	return f.Err
}

func (f *FakePower) Actions() []PowerAction {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]PowerAction(nil), f.actions...)
}
