package system

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const powerTimeout = 30 * time.Second

type execPower struct {
	commands map[PowerAction][]string
}

func (p *execPower) Do(action PowerAction) error {
	argv, ok := p.commands[action]
	if !ok {
		return ErrInvalidPowerAction
	}

	ctx, cancel := context.WithTimeout(context.Background(), powerTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		detail := strings.TrimSpace(string(out))
		if len(detail) > maxStderrLogged {
			detail = detail[:maxStderrLogged]
		}

		return fmt.Errorf("%w: %s: %v: %s", ErrPowerFailed, action, err, detail)
	}

	return nil
}
