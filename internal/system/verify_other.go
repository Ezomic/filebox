//go:build !linux

package system

func NewVerifier() Verifier {
	return &FakeVerifier{Passwords: map[string]string{"alice": "alice-dev-password", "bob": "bob-dev-password"}}
}
