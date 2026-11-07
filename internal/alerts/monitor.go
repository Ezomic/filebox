package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Ezomic/filebox/internal/mail"
)

const stateFile = "alerts.json"

var (
	ErrMailOff   = errors.New("email is not set up on this box")
	ErrNoAddress = errors.New("no alert address is set")
)

// Check looks at one part of the box. A new source of problems, such as the offsite backup, is one more Check.
// The checks of a Monitor run one at a time.
type Check func(now time.Time) Result

// Monitor runs the checks on a timer and emails the alert address. The open alerts and the address live in the panel
// data directory, so a restart neither forgets a problem nor mails it twice.
type Monitor struct {
	path   string
	sender mail.Sender
	checks []Check
	now    func() time.Time

	mu      sync.Mutex
	state   State
	address string
	checked time.Time
}

type stored struct {
	Address string `json:"address"`
	State
}

// Open loads what an earlier run left. sender is nil when mail.json is missing: problems then only show in the panel.
func Open(dir string, sender mail.Sender, checks []Check) (*Monitor, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &Monitor{path: filepath.Join(dir, stateFile), sender: sender, checks: checks, now: time.Now}
	content, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var loaded stored
	if err := json.Unmarshal(content, &loaded); err != nil {
		log.Printf("alerts: %s is damaged (%v), starting with no open problems", m.path, err)
		if err := os.Rename(m.path, m.path+".bad"); err != nil {
			log.Printf("alerts: could not keep the damaged file as %s.bad: %v", m.path, err)
		}

		return m, nil
	}
	m.address, m.state = loaded.Address, loaded.State

	return m, nil
}

// Run checks right away and then every period until ctx ends.
func (m *Monitor) Run(ctx context.Context, period time.Duration) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		m.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick runs every check once, decides, and sends what is due. It stops sending at the first failure; whatever was not
// sent is due again on the next tick.
func (m *Monitor) Tick(ctx context.Context) {
	now := m.now()
	results := make([]Result, 0, len(m.checks))
	for _, check := range m.checks {
		results = append(results, check(now))
	}

	m.mu.Lock()
	mails, next := Decide(m.state, results, now)
	logChanges(m.state, next)
	m.state, m.checked = next, now
	m.saveLocked()
	address := m.address
	m.mu.Unlock()

	if m.sender == nil || address == "" {
		return
	}
	for _, message := range mails {
		if err := m.sender.Send(ctx, address, message.Subject, message.Text); err != nil {
			log.Printf("alerts: %s email for %s not sent, trying again later: %v", message.Kind, message.Key, err)
			return
		}
		m.mu.Lock()
		m.state = Delivered(m.state, message, m.now())
		m.saveLocked()
		m.mu.Unlock()
	}
}

func logChanges(before, after State) {
	was, is := byKey(before.Open), byKey(after.Open)
	for _, alert := range after.Open {
		if _, ok := was[alert.Key]; !ok {
			log.Printf("alerts: problem found: %s", alert.Subject)
		}
	}
	for _, alert := range before.Open {
		if _, ok := is[alert.Key]; !ok {
			log.Printf("alerts: problem over: %s", alert.Subject)
		}
	}
}

func (m *Monitor) saveLocked() {
	if err := writeFileAtomic(m.path, stored{Address: m.address, State: m.state}); err != nil {
		log.Printf("alerts: save %s: %v", m.path, err)
	}
}

// Status is what the admin panel shows.
type Status struct {
	// Checked is zero until the first check after a start.
	Checked time.Time
	Open    []Alert
	Address string
	// Mail is false when mail.json is missing, so no email can go out at all.
	Mail bool
}

func (m *Monitor) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	return Status{Checked: m.checked, Open: slices.Clone(m.state.Open), Address: m.address, Mail: m.sender != nil}
}

// Healthy is false while any problem is open.
func (m *Monitor) Healthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.state.Open) == 0
}

// SetAddress stores where alerts go; "" stops the emails. The caller checks that it is one plain address.
func (m *Monitor) SetAddress(address string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	previous := m.address
	m.address = address
	if err := writeFileAtomic(m.path, stored{Address: m.address, State: m.state}); err != nil {
		m.address = previous
		return err
	}

	return nil
}

const testText = "This is a test email from the filebox admin panel.\n\n" +
	"Emails about problems with the disks, the disk mirror, the earlier versions, the portal certificate and unclean " +
	"restarts go to this address.\n"

func (m *Monitor) SendTest(ctx context.Context) error {
	if m.sender == nil {
		return ErrMailOff
	}
	m.mu.Lock()
	address := m.address
	m.mu.Unlock()
	if address == "" {
		return ErrNoAddress
	}

	return m.sender.Send(ctx, address, subjectPrefix+"Test email", testText)
}
