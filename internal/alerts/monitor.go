package alerts

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Ezomic/filebox/internal/mail"
)

const (
	stateFile    = "alerts.json"
	settingsFile = "alert-settings.json"
)

var (
	ErrMailOff   = errors.New("email is not set up on this box")
	ErrNoAddress = errors.New("no alert address is set")
)

// Check looks at one part of the box. A new source of problems, such as the offsite backup, is one more Check.
// The checks of a Monitor run one at a time.
type Check func(now time.Time) Result

// Monitor runs the checks on a timer and emails the alert address. The alerts and the address live in separate files
// in the panel data directory, so a restart neither forgets a problem nor mails it twice, and a damaged state file
// does not lose the address.
type Monitor struct {
	statePath    string
	settingsPath string
	sender       mail.Sender
	checks       []Check
	now          func() time.Time

	mu      sync.Mutex
	state   State
	address string
	checked time.Time
}

type settings struct {
	Address string `json:"address"`
}

// Open loads what an earlier run left. sender is nil when mail.json is missing: problems then only show in the panel.
func Open(dir string, sender mail.Sender, checks []Check) (*Monitor, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &Monitor{
		statePath: filepath.Join(dir, stateFile), settingsPath: filepath.Join(dir, settingsFile),
		sender: sender, checks: checks, now: time.Now,
	}
	if err := load(m.statePath, &m.state, "starting with no open problems"); err != nil {
		return nil, err
	}
	var saved settings
	if err := load(m.settingsPath, &saved, "no alert address is set"); err != nil {
		return nil, err
	}
	m.address = saved.Address

	return m, nil
}

// load leaves into as it is when the file does not exist, and sets a damaged file aside.
func load(path string, into any, without string) error {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(content, into); err != nil {
		log.Printf("alerts: %s is damaged (%v), %s", path, err, without)
		if err := os.Rename(path, path+".bad"); err != nil {
			log.Printf("alerts: could not keep the damaged file as %s.bad: %v", path, err)
		}
	}

	return nil
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

// Tick runs every check once, decides, and sends each email that is due on its own: one the mail service refuses
// waits for its next try without holding back the others.
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
		err := m.sender.Send(ctx, address, message.Subject, message.Text)
		m.mu.Lock()
		if err != nil {
			m.state = Failed(m.state, message, m.now())
			log.Printf("alerts: %s email for %s not sent, trying again after %s: %v",
				message.Kind, message.Key, retryAt(m.state, message).Format(time.TimeOnly), err)
		} else {
			m.state = Delivered(m.state, message, m.now())
		}
		m.saveLocked()
		m.mu.Unlock()
	}
}

func retryAt(state State, message Mail) time.Time {
	for _, alert := range slices.Concat(state.Open, state.Closed) {
		if alert.Key == message.Key {
			return alert.RetryAt
		}
	}

	return time.Time{}
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
	if err := writeFileAtomic(m.statePath, m.state); err != nil {
		log.Printf("alerts: save %s: %v", m.statePath, err)
	}
}

// Status is what the admin panel shows.
type Status struct {
	// Checked is zero until the first check after a start.
	Checked time.Time
	Open    []Alert
	// Unchecked lists the checks that could not look on their last run, by name.
	Unchecked []Unchecked
	Address   string
	// Mail is false when mail.json is missing, so no email can go out at all.
	Mail bool
}

func (m *Monitor) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	unchecked := slices.SortedFunc(maps.Values(m.state.Unchecked), func(a, b Unchecked) int { return cmp.Compare(a.Name, b.Name) })

	return Status{Checked: m.checked, Open: slices.Clone(m.state.Open), Unchecked: unchecked, Address: m.address, Mail: m.sender != nil}
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

	if err := writeFileAtomic(m.settingsPath, settings{Address: address}); err != nil {
		return err
	}
	m.address = address

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
