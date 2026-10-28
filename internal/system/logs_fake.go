package system

import "fmt"

type FakeLogs struct {
	lines map[LogSource][]string
}

func NewFakeLogs() *FakeLogs {
	sample := map[LogSource][]string{}
	for i := range 40 {
		minute := fmt.Sprintf("2026-10-29T09:%02d:12+0100", i)
		sample[LogSSHD] = append(sample[LogSSHD], fmt.Sprintf("%s filebox sshd[%d]: Accepted password for alice from 100.64.0.%d port 5%04d ssh2", minute, 2000+i, i+2, i))
		if i%7 == 0 {
			sample[LogSSHD] = append(sample[LogSSHD], fmt.Sprintf("%s filebox sshd[%d]: Failed password for invalid user admin from 203.0.113.9 port 4%04d ssh2", minute, 3000+i, i))
		}
		sample[LogPanel] = append(sample[LogPanel], fmt.Sprintf("%s filebox filebox-panel[311]: listening on https://100.64.0.1:8443 (sample line %d)", minute, i))
	}

	return &FakeLogs{lines: sample}
}

func (f *FakeLogs) Read(source LogSource, lines int, filter string) ([]string, error) {
	if err := validateLogRequest(source, lines); err != nil {
		return nil, err
	}

	return filterTail(f.lines[source], filter, lines), nil
}
