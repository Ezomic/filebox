package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// The helper exits with this code when the box cannot do quotas: not btrfs, quotas not enabled, or the user not converted.
	quotaUnavailableExit = 4

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

func (u *execUsers) Quotas() (map[string]Quota, error) {
	out, err := u.run("", "quota-list")
	if err != nil {
		return nil, err
	}

	return parseQuotaList(out)
}

func (u *execUsers) SetQuota(name string, bytes int64) error {
	if !ValidUsername(name) {
		return ErrInvalidUsername
	}
	if !ValidQuotaBytes(bytes) {
		return ErrInvalidQuota
	}
	_, err := u.run("", "quota-set", name, strconv.FormatInt(bytes, 10))

	return err
}

func (u *execUsers) ClearQuota(name string) error {
	if !ValidUsername(name) {
		return ErrInvalidUsername
	}
	_, err := u.run("", "quota-clear", name)

	return err
}

// One line per user: name, bytes used, then the limit in bytes or "none". A folder that is not a subvolume yet is "name - -".
func parseQuotaList(out string) (map[string]Quota, error) {
	quotas := map[string]Quota{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, " ")
		if len(fields) != 3 || !ValidUsername(fields[0]) {
			return nil, fmt.Errorf("%w: unexpected quota line", ErrHelperFailed)
		}
		if _, dup := quotas[fields[0]]; dup {
			return nil, fmt.Errorf("%w: duplicate quota line", ErrHelperFailed)
		}
		if fields[1] == "-" && fields[2] == "-" {
			continue
		}
		used, usedOK := parseCanonical(fields[1])
		limit, limitOK := uint64(0), fields[2] == "none"
		if !limitOK {
			limit, limitOK = parseCanonical(fields[2])
			limitOK = limitOK && limit > 0
		}
		if !usedOK || !limitOK {
			return nil, fmt.Errorf("%w: unexpected quota line", ErrHelperFailed)
		}
		quotas[fields[0]] = Quota{Used: used, Limit: limit}
	}

	return quotas, nil
}

// Digits only, no sign and no leading zeros, so what the helper printed is exactly the number we read.
func parseCanonical(text string) (uint64, bool) {
	value, err := strconv.ParseUint(text, 10, 64)

	return value, err == nil && strconv.FormatUint(value, 10) == text
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
		if errors.As(err, &exit) && exit.ExitCode() == quotaUnavailableExit {
			return "", fmt.Errorf("%w: %w: %s", ErrQuotasUnavailable, ErrHelperFailed, detail)
		}
		if errors.As(err, &exit) {
			return "", fmt.Errorf("%w: %v: %s", ErrHelperFailed, err, detail)
		}

		return "", fmt.Errorf("%w: %v", ErrHelperFailed, err)
	}

	return stdout.String(), nil
}
