package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// TestSignatureConsistentRewriteWithStaleSignatureRefused: a new manifest, a matching
// new pin digest -- and the old signature over the old bytes.
func TestSignatureConsistentRewriteWithStaleSignatureRefused(t *testing.T) {
	fixture := testFixture(t)
	directory := fixture.copy(t, "stale-signature")
	path := filepath.Join(directory, "evidence", manifestName)
	manifest := document(t, harness.ReadFile(t, path))
	object(t, manifest, "platforms", string(linuxX64))["checksum"] = strings.Repeat("f", 64)
	raw := encodeDocument(t, manifest)
	harness.WriteFile(t, path, raw)
	rewritePin(t, directory, func(pin map[string]any) {
		object(t, pin, "manifest")["sha256"] = harness.SHA256Hex(raw)
		object(t, pin, "platforms", "linux/amd64")["sha256"] = strings.Repeat("f", 64)
	})
	checkRefused(t, directory, "signature does not verify")
}

// TestSignatureByAnotherKeyRefused: a well-formed signature the committed key did not
// make is a refusal, whatever the pin's digest says about those bytes.
func TestSignatureByAnotherKeyRefused(t *testing.T) {
	fixture := testFixture(t)
	attacker := makeSigner(t, filepath.Join(scratch(t, "attacker"), "signer"), false)
	directory := fixture.copy(t, "foreign-signature")
	path := filepath.Join(directory, "evidence", manifestName)
	signature := sign(t, attacker, path, 0)
	harness.WriteFile(t, filepath.Join(directory, "evidence", signatureName), signature)
	rewritePin(t, directory, func(pin map[string]any) {
		object(t, pin, "signature")["sha256"] = harness.SHA256Hex(signature)
	})
	checkRefused(t, directory, "signature does not verify")
}

// TestSignatureSubstitutedKeyRefused: the pin still names the fixture fingerprint, so
// the replaced key file is refused before the signature is even considered.
func TestSignatureSubstitutedKeyRefused(t *testing.T) {
	fixture := testFixture(t)
	attacker := makeSigner(t, filepath.Join(scratch(t, "attacker-key"), "signer"), false)
	directory := fixture.copy(t, "substituted-key")
	key := harness.ReadFile(t, attacker.Key)
	harness.WriteFile(t, filepath.Join(directory, keyName), key)
	rewritePin(t, directory, func(pin map[string]any) {
		object(t, pin, "key")["sha256"] = harness.SHA256Hex(key)
	})
	checkRefused(t, directory, "exactly the primary")
}

// TestSignatureKeyFileWithTwoPrimariesRefused: a second primary in the key file is a
// second signer, so the file is refused rather than counted.
func TestSignatureKeyFileWithTwoPrimariesRefused(t *testing.T) {
	fixture := testFixture(t)
	other := makeSigner(t, filepath.Join(scratch(t, "second-key"), "signer"), false)
	directory := fixture.copy(t, "two-primaries")
	combined := twoPrimaries(t, filepath.Join(directory, keyName+".2"),
		fixture.signer.Key, other.Key)
	key := harness.ReadFile(t, combined)
	harness.WriteFile(t, filepath.Join(directory, keyName), key)
	if err := os.Remove(combined); err != nil {
		t.Fatalf("cannot remove the combined key file: %v", err)
	}
	rewritePin(t, directory, func(pin map[string]any) {
		object(t, pin, "key")["sha256"] = harness.SHA256Hex(key)
	})
	checkRefused(t, directory, "exactly the primary")
}

// TestSignatureKeyFileCarryingSecretKeyMaterialRefused: the committed key file holds no
// secret key, named or not.
//
// `show-only` lists a secret key as `sec`, so a `pub`-only count would accept this file
// and the import would install the second signer.
func TestSignatureKeyFileCarryingSecretKeyMaterialRefused(t *testing.T) {
	fixture := testFixture(t)
	other := makeSigner(t, filepath.Join(scratch(t, "secret-signer"), "signer"), false)
	for _, secret := range []struct {
		label string
		path  string
	}{
		{"own", fixture.signer.Secret},
		{"foreign", other.Secret},
	} {
		t.Run(secret.label, func(t *testing.T) {
			directory := fixture.copy(t, "secret-key-"+secret.label)
			path := filepath.Join(directory, keyName)
			key := append(harness.ReadFile(t, path), harness.ReadFile(t, secret.path)...)
			harness.WriteFile(t, path, key)
			rewritePin(t, directory, func(pin map[string]any) {
				object(t, pin, "key")["sha256"] = harness.SHA256Hex(key)
			})
			checkRefused(t, directory, "carries secret key material")
		})
	}
}

// TestSignatureValidSigNamingAnotherPrimaryRefused: a good signature reported under
// another primary is what an imported, unnamed signer would look like.
//
// Every other "other signer" vector is refused earlier (NO_PUBKEY, ERRSIG, or the
// single-primary key check), so nothing else tests the VALIDSIG comparison.
func TestSignatureValidSigNamingAnotherPrimaryRefused(t *testing.T) {
	fixture := testFixture(t)
	probe := newGPGStub(t, scratch(t, "validsig-primary"), stubRequest{
		Mode:            stubValidSig,
		ValidSigPrimary: Fingerprint(strings.Repeat("F", 40)),
	})
	checkRefusedAt(t, fixture.closure, Version, probe.Env, "is not by the primary")
	logged := probe.homes(t)
	if len(logged) == 0 {
		t.Fatal("the gpg wrapper logged no invocation")
	}
	for _, home := range logged {
		if _, err := os.Stat(home); err == nil {
			t.Fatalf("the gpg home %s was left behind", home)
		}
	}
}

// TestSignatureGPGPAbsentFromPathRefused: without gpg the verification cannot happen,
// and the refusal says which program is missing rather than blaming the signature.
func TestSignatureGPGPAbsentFromPathRefused(t *testing.T) {
	fixture := testFixture(t)
	// A PATH with nothing in it: the reader falls back to /usr/bin:/bin only when PATH
	// is empty, so the directory has to exist and stay barren.
	empty := scratch(t, "gpg-absent-bin")
	text := checkRefusedAt(t, fixture.closure, Version, map[string]string{"PATH": empty},
		"gpg is required")
	if !strings.Contains(text, "gpg is required") {
		t.Fatal(text)
	}
}
