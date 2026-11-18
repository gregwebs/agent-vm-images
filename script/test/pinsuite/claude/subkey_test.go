package claude

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// TestSubkeySecretSubkeyRecordInTheKeyListingRefused: an `ssb` record in the key
// listing is a refusal, not a primary.
//
// `show-only` never prints this shape -- both a secret primary and a secret subkey
// begin with `sec`, which the fixture-less cases refuse on its own -- so this is the
// short vector that reaches the `ssb` rule with no earlier `sec` record.
func TestSubkeySecretSubkeyRecordInTheKeyListingRefused(t *testing.T) {
	fixture := testFixture(t)
	listing := filepath.Join(scratch(t, "secret-subkey-listing"), "listing.txt")
	harness.WriteFile(t, listing, []byte(strings.Join([]string{
		"pub:u:255:22:E010800BADA0909D:1793422096:::u:::cSC:::::ed25519:::0:",
		fmt.Sprintf("fpr:::::::::%s:", fixture.signer.Fingerprint),
		"uid:u::::1793422096::8719982D789267C30FA69B421197E3BE59AE39FF::" +
			"fixture claude signer <fixture@example.invalid>::::::::::0:",
		"ssb:-:255:22:DFB3BE9DEA71B340:1793444866::::::s:::#::ed25519::",
		"fpr:::::::::C68152E062F6665D6BA0AD42DFB3BE9DEA71B340:",
		"",
	}, "\n")))
	probe := newGPGStub(t, scratch(t, "secret-subkey-listing-bin"),
		stubRequest{Mode: stubListing, Listing: listing})
	checkRefusedAt(t, fixture.closure, Version, probe.Env, "carries secret key material")
}

// TestSubkeyClosureSignedBySigningSubkeyVerifiesAsItsPrimary: a signature made by a
// reviewed signing subkey is accepted as its primary, and the subkey is not a pin
// identity.
func TestSubkeyClosureSignedBySigningSubkeyVerifiesAsItsPrimary(t *testing.T) {
	base := scratch(t, "subkey")
	signer := makeSigner(t, filepath.Join(base, "signer"), true)
	if signer.Fingerprint == signer.Signing {
		t.Fatal("the fixture did not create a separate signing subkey")
	}
	x64 := elfBlob(t, filepath.Join(base, "assets", "x64"), amd64Machine, elfShape{})
	arm64 := elfBlob(t, filepath.Join(base, "assets", "arm64"), arm64Machine, elfShape{})
	closure := buildClosure(t, closureRequest{
		Out:     filepath.Join(base, "closure"),
		Signer:  signer,
		Version: Version,
		AMD64:   x64,
		ARM64:   arm64,
	})
	printed := checkAccepted(t, closure, Version).Stdout
	if !strings.Contains(printed, string(signer.Fingerprint)) {
		t.Fatalf("check reports %q, not the primary %s", printed, signer.Fingerprint)
	}
	// The same closure with the *subkey* as the pin's identity is refused: a pin may
	// name a primary only.
	rewritePin(t, closure, func(pin map[string]any) {
		object(t, pin, "key")["fingerprint"] = string(signer.Signing)
	})
	checkRefused(t, closure, "exactly the primary")
}
