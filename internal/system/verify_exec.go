package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	verifyMismatch = 10
	verifyTimeout  = 10 * time.Second
)

type execVerifier struct {
	argv []string
	env  []string
}

// The password goes to the helper on stdin only, so it never shows up in the process list.
func (v *execVerifier) Verify(name, password string) (bool, error) {
	if !ValidUsername(name) || ValidatePassword(password) != nil {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
	defer cancel()

	argv := append(append([]string{}, v.argv...), name)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), v.env...)
	cmd.Stdin = strings.NewReader(password + "\n")

	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == verifyMismatch {
		return false, nil
	}

	return false, fmt.Errorf("%w: %v", ErrHelperFailed, err)
}
