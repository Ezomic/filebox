package system

import (
	"errors"
	"testing"
)

func TestParsePowerAction(t *testing.T) {
	for _, valid := range []string{"restart-sshd", "shutdown", "reboot"} {
		if _, ok := ParsePowerAction(valid); !ok {
			t.Errorf("%q rejected", valid)
		}
	}
	for _, invalid := range []string{"", "poweroff", "Reboot", "reboot ", "restart-sshd;reboot"} {
		if _, ok := ParsePowerAction(invalid); ok {
			t.Errorf("%q accepted", invalid)
		}
	}
}

func TestExecPowerRejectsUnknownAction(t *testing.T) {
	p := &execPower{commands: map[PowerAction][]string{}}
	if err := p.Do("halt"); !errors.Is(err, ErrInvalidPowerAction) {
		t.Fatalf("got %v", err)
	}
}

func TestExecPowerReportsFailure(t *testing.T) {
	p := &execPower{commands: map[PowerAction][]string{Reboot: {"this-command-does-not-exist"}}}
	if err := p.Do(Reboot); !errors.Is(err, ErrPowerFailed) {
		t.Fatalf("got %v", err)
	}
}

func TestFakePowerRecordsActions(t *testing.T) {
	f := &FakePower{}
	if err := f.Do(Shutdown); err != nil {
		t.Fatal(err)
	}
	if got := f.Actions(); len(got) != 1 || got[0] != Shutdown {
		t.Fatalf("got %v", got)
	}
	if err := f.Do("halt"); !errors.Is(err, ErrInvalidPowerAction) {
		t.Fatalf("got %v", err)
	}
}
