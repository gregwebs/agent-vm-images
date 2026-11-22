package gpg

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestParseStatus is invariant 4 at the status-line level: every rejected
// keyword, no VALIDSIG at all, a malformed one, and the upper-casing of the
// fingerprint.
func TestParseStatus(t *testing.T) {
	good := "[GNUPG:] NEWSIG\n[GNUPG:] VALIDSIG 108f66205eaeb0aaa8dd5e1c85ab96e6fa1be5fe 2026-10-25 0 4 0 1 10 00 abcdef\n[GNUPG:] TRUST_ULTIMATE\n"
	signers, err := ParseStatus(good, "manifest signature")
	if err != nil {
		t.Fatalf("refused a good status stream: %v", err)
	}
	if len(signers) != 1 || signers[0] != "108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE" {
		t.Fatalf("signers are %v", signers)
	}
	for _, keyword := range RejectedStatuses {
		status := "[GNUPG:] VALIDSIG 108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE 1 2 3\n[GNUPG:] " +
			keyword
		_, err := ParseStatus(status, "what")
		if err == nil || !strings.Contains(err.Error(), "signature rejected: "+keyword) {
			t.Errorf("%s: got %v", keyword, err)
		}
	}
	if _, err := ParseStatus("[GNUPG:] NEWSIG\n", "what"); err == nil ||
		!strings.Contains(err.Error(), "no VALIDSIG status line") {
		t.Errorf("no VALIDSIG: %v", err)
	}
	if _, err := ParseStatus("[GNUPG:] VALIDSIG\n", "what"); err == nil ||
		!strings.Contains(err.Error(), "malformed VALIDSIG status") {
		t.Errorf("malformed VALIDSIG: %v", err)
	}
}

// TestParsePrimaries reads the colon listing the way the source reader does.
func TestParsePrimaries(t *testing.T) {
	listing := strings.Join([]string{
		"pub:-:255:22:108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE:0:0::::::scESC::::::23::0:",
		"fpr:::::::::108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE:",
		"uid:-::::1:0::Rust Release Signing Key::::::::::0:",
		"sub:-:255:22:ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD:0:0::::::e::::::23:",
		"fpr:::::::::ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD:",
		"",
	}, "\n")
	primaries, signers := ParsePrimaries(listing)
	if len(primaries) != 1 || primaries[0] != "108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE" {
		t.Fatalf("primaries are %v", primaries)
	}
	if len(signers) != 1 || signers[0] != "ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD" {
		t.Fatalf("signers are %v", signers)
	}
}

// gpgCommand runs the real gpg with a home that is the only environment the test
// gives it.
func gpgCommand(t *testing.T, home string, arguments ...string) (string, int) {
	t.Helper()
	command := exec.Command("gpg", arguments...)
	command.Env = []string{"GNUPGHOME=" + home, "HOME=" + home, "PATH=" + ImagesPath,
		"LC_ALL=C", "TMPDIR=" + home}
	var out, errOut strings.Builder
	command.Stdout = &out
	command.Stderr = &errOut
	err := command.Run()
	status := 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			status = exit.ExitCode()
		} else {
			t.Fatalf("gpg %v: %v", arguments, err)
		}
	}
	_ = errOut
	return out.String(), status
}

// fixtureKey generates one throwaway signing key and returns its fingerprint and
// armored public key. These tests never skip: a missing gpg is a failure.
func fixtureKey(t *testing.T, name string) (home, fingerprint, armored string) {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Fatalf("gpg is required by this test: %v", err)
	}
	home = t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, status := gpgCommand(t, home, "--batch", "--passphrase", "",
		"--quick-generate-key", name+" <"+name+"@example.test>", "ed25519", "sign", "never"); status != 0 {
		t.Fatalf("key generation failed with status %d", status)
	}
	listing, _ := gpgCommand(t, home, "--batch", "--with-colons", "--fingerprint",
		"--list-keys")
	primaries, _ := ParsePrimaries(listing)
	if len(primaries) != 1 {
		t.Fatalf("primaries: %v", primaries)
	}
	armored, _ = gpgCommand(t, home, "--batch", "--armor", "--export", name+"@example.test")
	return home, primaries[0], armored
}

// sign signs data with a home's key.
func sign(t *testing.T, home, data string, arguments ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	armored := filepath.Join(t.TempDir(), "data.asc")
	full := append([]string{"--batch", "--armor", "--detach-sign", "--output", armored}, arguments...)
	full = append(full, path)
	if _, status := gpgCommand(t, home, full...); status != 0 {
		t.Fatalf("signing failed with status %d", status)
	}
	return armored
}

// TestVerifierRealGPG is invariant 3–5 with the real tool: the key file must hold
// exactly the reviewed primary and every reviewed signer, the verdict needs both
// a zero exit and a VALIDSIG, and the keyring is private and removable.
func TestVerifierRealGPG(t *testing.T) {
	home, fingerprint, armored := fixtureKey(t, "fixture-signer")
	otherHome, otherFingerprint, otherArmored := fixtureKey(t, "other-signer")
	_ = otherHome
	program, err := NewProgram("/opt/homebrew/bin/gpg")
	if err != nil {
		if resolved, resolveErr := Resolve(); resolveErr == nil {
			program = resolved
		} else {
			t.Fatalf("no gpg: %v", resolveErr)
		}
	}
	keyFile := filepath.Join(t.TempDir(), "key.asc")
	if err := os.WriteFile(keyFile, []byte(armored), 0o644); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(data, []byte("manifest bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	goodSignature := sign(t, home, "manifest bytes\n")

	t.Run("good signature", func(t *testing.T) {
		work, err := WorkDirectory()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.RemoveAll(work) }()
		verifier := NewVerifier(program, keyFile, work,
			Policy{Primary: fingerprint, Signers: []string{fingerprint}})
		if err := verifier.Verify(context.Background(), data, goodSignature,
			"manifest signature"); err != nil {
			t.Fatalf("refused a good signature: %v", err)
		}
		info, err := os.Stat(filepath.Join(work, "gnupg"))
		if err != nil {
			t.Fatalf("the keyring is missing: %v", err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("the keyring is %v, want 0700", info.Mode().Perm())
		}
		if err := os.RemoveAll(work); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(work); !os.IsNotExist(err) {
			t.Fatalf("the keyring survived removal: %v", err)
		}
	})

	t.Run("other bytes", func(t *testing.T) {
		work, _ := WorkDirectory()
		defer func() { _ = os.RemoveAll(work) }()
		verifier := NewVerifier(program, keyFile, work,
			Policy{Primary: fingerprint, Signers: []string{fingerprint}})
		changed := filepath.Join(t.TempDir(), "changed")
		if err := os.WriteFile(changed, []byte("other bytes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := verifier.Verify(context.Background(), changed, goodSignature,
			"manifest signature")
		if err == nil {
			t.Fatal("accepted a signature over other bytes")
		}
		if !strings.Contains(err.Error(), "manifest signature check failed with status") &&
			!strings.Contains(err.Error(), "manifest signature: signature rejected") &&
			!strings.Contains(err.Error(), "no VALIDSIG") {
			t.Fatalf("unexpected refusal: %v", err)
		}
	})

	t.Run("two primaries", func(t *testing.T) {
		work, _ := WorkDirectory()
		defer func() { _ = os.RemoveAll(work) }()
		both := filepath.Join(t.TempDir(), "both.asc")
		if err := os.WriteFile(both, []byte(armored+otherArmored), 0o644); err != nil {
			t.Fatal(err)
		}
		verifier := NewVerifier(program, both, work,
			Policy{Primary: fingerprint, Signers: []string{fingerprint}})
		err := verifier.Verify(context.Background(), data, goodSignature, "what")
		if err == nil || !strings.Contains(err.Error(),
			"key file must hold exactly the reviewed primary key "+fingerprint) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("lacks signer", func(t *testing.T) {
		work, _ := WorkDirectory()
		defer func() { _ = os.RemoveAll(work) }()
		verifier := NewVerifier(program, keyFile, work,
			Policy{Primary: fingerprint, Signers: []string{fingerprint, otherFingerprint}})
		err := verifier.Verify(context.Background(), data, goodSignature, "what")
		if err == nil || !strings.Contains(err.Error(),
			"key file lacks reviewed signing key "+otherFingerprint) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("exit 1 with a real VALIDSIG", func(t *testing.T) {
		work, _ := WorkDirectory()
		defer func() { _ = os.RemoveAll(work) }()
		verifier := NewVerifier(program, keyFile, work,
			Policy{Primary: fingerprint, Signers: []string{fingerprint}})
		verifier.WithExec(func(ctx context.Context, argv, env []string) (Result, error) {
			result, err := ExecProcess(ctx, argv, env)
			result.Status = 1
			return result, err
		})
		err := verifier.Verify(context.Background(), data, goodSignature, "what")
		if err == nil || !strings.Contains(err.Error(), "failed with status 1") {
			t.Fatalf("a nonzero exit was read as a verdict: %v", err)
		}
	})

	t.Run("expired signature", func(t *testing.T) {
		work, _ := WorkDirectory()
		defer func() { _ = os.RemoveAll(work) }()
		expiringHome := t.TempDir()
		_ = os.Chmod(expiringHome, 0o700)
		// The key lives in a home of its own so the expiry default is local.
		copyOfKey := filepath.Join(t.TempDir(), "key.asc")
		if err := os.WriteFile(copyOfKey, []byte(armored), 0o644); err != nil {
			t.Fatal(err)
		}
		expiring := sign(t, home, "manifest bytes\n", "--default-sig-expire", "1d")
		// `--faked-system-time` takes an epoch or an ISO timestamp, not a duration:
		// "+2d" is read as epoch 2, which refuses for the wrong reason. The faked
		// clock is therefore now + two days, which makes gpg report EXPSIG.
		faked := strconv.FormatInt(time.Now().Unix()+172800, 10)
		// The same signature verifies while it is valid: otherwise the refusal
		// below could be about the key, the bytes or the expiry default.
		live := NewVerifier(program, keyFile, work,
			Policy{Primary: fingerprint, Signers: []string{fingerprint}})
		if err := live.Verify(context.Background(), data, expiring, "live"); err != nil {
			t.Fatalf("an ageable signature was refused while valid: %v", err)
		}
		verifier := NewVerifier(program, keyFile, work,
			Policy{Primary: fingerprint, Signers: []string{fingerprint}})
		verifier.WithExec(func(ctx context.Context, argv, env []string) (Result, error) {
			for index, argument := range argv {
				if argument != "--verify" {
					continue
				}
				rewritten := append([]string{}, argv[:index]...)
				rewritten = append(rewritten, "--faked-system-time", faked)
				rewritten = append(rewritten, argv[index:]...)
				return ExecProcess(ctx, rewritten, env)
			}
			return ExecProcess(ctx, argv, env)
		})
		// gpg's exit status is checked before the status lines, so the refusal is
		// its nonzero exit; the source reader's own test accepts either wording.
		err := verifier.Verify(context.Background(), data, expiring, "expired signature")
		refusal := ""
		if err != nil {
			refusal = err.Error()
		}
		if !strings.Contains(refusal, "failed with status 1") &&
			!strings.Contains(refusal, "signature rejected: EXPSIG") {
			t.Fatalf("an expired signature was not refused: %q", refusal)
		}
		if strings.Contains(refusal, "Good signature") {
			t.Fatalf("the refusal matched human text instead of the status: %q", refusal)
		}
	})
}

// TestExecEnvironmentIsExactlyFive is invariant 5: the child sees exactly PATH,
// GNUPGHOME, HOME, LC_ALL and TMPDIR, and every call carries --batch
// --no-options.
func TestExecEnvironmentIsExactlyFive(t *testing.T) {
	var got [][]string
	work := t.TempDir()
	verifier := NewVerifier("gpg", "/key", work, Policy{Primary: "A", Signers: []string{"A"}})
	verifier.WithExec(func(ctx context.Context, argv, env []string) (Result, error) {
		got = append(got, append([]string{strings.Join(argv, " ")}, env...))
		if strings.Contains(strings.Join(argv, " "), "--list-keys") {
			return Result{Stdout: "pub:u:1:1:A:0:0:::::::\nfpr:::::::::A:\n"}, nil
		}
		return Result{Stdout: "[GNUPG:] VALIDSIG A 1\n"}, nil
	})
	if err := verifier.Verify(context.Background(), "/data", "/sig", "what"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("calls: %d", len(got))
	}
	for index, call := range got {
		argv := call[0]
		if !strings.Contains(argv, "--batch") || !strings.Contains(argv, "--no-options") {
			t.Errorf("call %d does not pass --batch --no-options: %s", index, argv)
		}
		env := call[1:]
		if len(env) != 5 {
			t.Errorf("call %d has %d variables: %v", index, len(env), env)
		}
		want := map[string]string{"GNUPGHOME": filepath.Join(work, "gnupg"), "HOME": work,
			"LC_ALL": "C", "TMPDIR": work, "PATH": ".:" + ImagesPath}
		for _, entry := range env {
			key, value, _ := strings.Cut(entry, "=")
			if want[key] != value {
				t.Errorf("call %d: %s=%q, want %q", index, key, value, want[key])
			}
		}
	}
	if !strings.Contains(got[2][0], "--status-fd 1 --verify /sig /data") {
		t.Errorf("verify argv is %q", got[2][0])
	}
}
