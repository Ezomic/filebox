package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	helperTimeout   = 30 * time.Second
	maxStderrLogged = 200
)

type execUsers struct {
	argv []string
	env  []string
}

func (u *execUsers) List() ([]string, error) {
	out, err := u.run("", "list")
	if err != nil {
		return nil, err
	}

	var names []string
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}

	return names, nil
}

func (u *execUsers) Create(name, password string) error {
	return u.withPassword("create", name, password)
}

func (u *execUsers) SetPassword(name, password string) error {
	return u.withPassword("passwd", name, password)
}

func (u *execUsers) Delete(name string) error {
	if !ValidUsername(name) {
		return ErrInvalidUsername
	}
	_, err := u.run("", "delete", name)

	return err
}

func (u *execUsers) withPassword(action, name, password string) error {
	if !ValidUsername(name) {
		return ErrInvalidUsername
	}
	if err := ValidatePassword(password); err != nil {
		return err
	}
	_, err := u.run(password, action, name)

	return err
}

// The password goes to the helper on stdin only, so it never shows up in the process list.
func (u *execUsers) run(password string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()

	argv := append(append([]string{}, u.argv...), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), u.env...)
	if password != "" {
		cmd.Stdin = strings.NewReader(password + "\n")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > maxStderrLogged {
			detail = detail[:maxStderrLogged]
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("%w: %v: %s", ErrHelperFailed, err, detail)
		}

		return "", fmt.Errorf("%w: %v", ErrHelperFailed, err)
	}

	return stdout.String(), nil
}
