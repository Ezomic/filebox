package system

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

const smartTimeout = 30 * time.Second

func readSmartHelper(argv []string) ([]DiskHealth, error) {
	ctx, cancel := context.WithTimeout(context.Background(), smartTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSmartUnavailable, err)
	}

	return parseSmartLines(string(out)), nil
}
