package system

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

type journalRunner func(ctx context.Context, args []string, limit int64) (output []byte, truncated bool, err error)

type execLogs struct {
	run journalRunner
}

func (l *execLogs) Read(source LogSource, lines int, filter string) ([]string, error) {
	if err := validateLogRequest(source, lines); err != nil {
		return nil, err
	}
	window := lines
	if filter != "" {
		window = filterWindow
	}

	ctx, cancel := context.WithTimeout(context.Background(), journalTimeout)
	defer cancel()
	output, truncated, err := l.run(ctx, journalArgs(source, window), maxJournalBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLogsUnavailable, err)
	}

	newestFirst := splitJournal(string(output))
	if truncated && len(newestFirst) > 0 {
		newestFirst = newestFirst[:len(newestFirst)-1]
	}
	oldestFirst := make([]string, len(newestFirst))
	for i, line := range newestFirst {
		oldestFirst[len(newestFirst)-1-i] = line
	}

	return filterTail(oldestFirst, filter, lines), nil
}

func splitJournal(output string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, "-- ") {
			lines = append(lines, line)
		}
	}

	return lines
}

type cappedBuffer struct {
	bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}

	return len(p), nil
}

// No shell is involved and args come from journalArgs only.
func runJournalctl(ctx context.Context, args []string, limit int64) ([]byte, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, journalctlPath, args...)
	cmd.WaitDelay = time.Second
	cmd.Env = []string{"LC_ALL=C.UTF-8", "PATH=/usr/bin:/bin"}
	stderr := &cappedBuffer{max: 300}
	cmd.Stderr = stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}

	data, readErr := io.ReadAll(io.LimitReader(pipe, limit+1))
	truncated := int64(len(data)) > limit
	if truncated {
		data = data[:limit]
		cancel()
	}
	waitErr := cmd.Wait()

	switch {
	case readErr != nil:
		return nil, false, readErr
	case truncated:
		return data, true, nil
	case waitErr != nil:
		return nil, false, fmt.Errorf("%v: %s", waitErr, strings.TrimSpace(stderr.String()))
	case len(data) == 0 && stderr.Len() > 0:
		return nil, false, fmt.Errorf("journalctl: %s", strings.TrimSpace(stderr.String()))
	}

	return data, false, nil
}
