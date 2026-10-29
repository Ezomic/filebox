package system

import "sync"

type Verifier interface {
	// A wrong password or unknown user is (false, nil); an error means the check itself failed.
	Verify(name, password string) (bool, error)
}

type FakeVerifier struct {
	Passwords map[string]string
	Err       error

	mu    sync.Mutex
	Calls int
}

func (f *FakeVerifier) Verify(name, password string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++

	if f.Err != nil {
		return false, f.Err
	}
	expected, found := f.Passwords[name]

	return found && expected == password, nil
}
