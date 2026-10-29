//go:build linux

package system

const verifyHelperPath = "/usr/local/sbin/filebox-verify"

func NewVerifier() Verifier {
	return &execVerifier{argv: []string{"sudo", "-n", verifyHelperPath}}
}
