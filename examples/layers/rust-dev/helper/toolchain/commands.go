package toolchain

// The two offline verbs. `check` proves the pin directory is exactly what the
// pin names; `verify-installed` does the same and then interrogates the
// installed toolchain. Both return the pinned channel on success, and both
// refuse in the source reader's order so the two readers report the same first
// refusal.

import (
	"context"
	"os"
	"path/filepath"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/gpg"
)

// PolicyFor builds the gpg policy from a reviewed trust.
func PolicyFor(trust TrustPolicy) gpg.Policy {
	signers := make([]string, 0, len(trust.Signers()))
	for _, signer := range trust.Signers() {
		signers = append(signers, string(signer))
	}
	return gpg.Policy{Primary: string(trust.Primary()), Signers: signers}
}

// CheckRequest is one `check` invocation.
type CheckRequest struct {
	InputDir string
	// ExpectChannel is the assertion. A nil pointer means no assertion; an empty
	// string is an assertion that fails, exactly as the source reader's argparse
	// default of None distinguishes them.
	ExpectChannel *string
	Trust         TrustPolicy
	GPG           gpg.Program
}

// Check verifies the committed pin data offline and returns the channel.
func Check(ctx context.Context, request CheckRequest) (Channel, error) {
	directory := filepath.Clean(request.InputDir)
	work, err := gpg.WorkDirectory()
	if err != nil {
		return "", err
	}
	verifier := gpg.NewVerifier(request.GPG, filepath.Join(directory, KeyPath), work,
		PolicyFor(request.Trust))
	defer removeKeyring(work)
	pin, err := LoadPin(directory, request.Trust)
	if err != nil {
		return "", err
	}
	if request.ExpectChannel != nil && string(pin.Channel) != *request.ExpectChannel {
		return "", refuse("pin channel %s != asserted channel %s", pin.Channel,
			*request.ExpectChannel)
	}
	if _, err := VerifyInputs(ctx, directory, pin, verifier); err != nil {
		return "", err
	}
	return pin.Channel, nil
}

// VerifyInstalledRequest is one `verify-installed` invocation.
type VerifyInstalledRequest struct {
	InputDir      string
	Arch          Architecture
	ExpectChannel string
	Homes         Homes
	Trust         TrustPolicy
	GPG           gpg.Program
	Runner        Runner
}

// VerifyInstalled verifies the committed pin data and then the installed
// toolchain, and returns the channel.
func VerifyInstalled(ctx context.Context, request VerifyInstalledRequest) (Channel, error) {
	directory := filepath.Clean(request.InputDir)
	pin, err := LoadPin(directory, request.Trust)
	if err != nil {
		return "", err
	}
	if string(pin.Channel) != request.ExpectChannel {
		return "", refuse("pin channel %s != asserted channel %s", pin.Channel,
			request.ExpectChannel)
	}
	work, err := gpg.WorkDirectory()
	if err != nil {
		return "", err
	}
	verifier := gpg.NewVerifier(request.GPG, filepath.Join(directory, KeyPath), work,
		PolicyFor(request.Trust))
	// The keyring is scratch: it must not become image or host state after
	// success or failure.
	defer removeKeyring(work)
	if _, err := VerifyInputs(ctx, directory, pin, verifier); err != nil {
		return "", err
	}
	runner := request.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	if err := CheckInstalled(ctx, pin, request.Arch, request.Homes, runner); err != nil {
		return "", err
	}
	return pin.Channel, nil
}

// removeKeyring removes the invocation-private keyring on every return path,
// ignoring a removal failure as the source reader's `ignore_errors=True` does: the
// status is already decided, a leftover `rust-verify-*` directory is a
// `rust-example.sh` inspect's finding, and a warning on stderr would break the
// empty-stderr contract of a successful run.
func removeKeyring(work string) {
	_ = os.RemoveAll(work)
}
