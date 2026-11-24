package toolchain

// Reviewed policy for the rust-dev layer's toolchain pin (ADR 0007).
//
// Everything here is a compiled constant, never something pin.json can widen.
// The trust root is the Rust release signing key published at
// https://static.rust-lang.org/rust-key.gpg.ascii (primary fingerprint also
// published via https://keybase.io/rust). A different primary key or signer set
// is a reviewed policy change in the image input policy and the ADR.

// ReviewedPrimaryFingerprint is the one trust root a pin may name.
const ReviewedPrimaryFingerprint Fingerprint = "108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE"

const (
	PinFile                = "pin.json"
	ManifestPath           = "channel.toml"
	ManifestChecksumPath   = "channel.toml.sha256"
	ManifestSignaturePath  = "channel.toml.asc"
	KeyPath                = "rust-release-key.asc"
	EvidenceDir            = "evidence"
	DistServer             = "https://static.rust-lang.org"
	UpdateRoot             = "https://static.rust-lang.org/rustup"
	RequiredRustupVersion  = "1.28.2"
	Profile                = "minimal"
	MaxEvidencePathRunes   = 200
	MaxURLRunes            = 512
	MaxTripleRunes         = 64
	MaxReviewerHandleRunes = 64

	// Download and archive bounds (ADR 0017 D2). A measured review, not an
	// automatic increase, changes them.
	MaxBootstrapBytes     int64 = 32 << 20
	MaxArchiveBytes       int64 = 256 << 20
	MaxEvidenceBytes      int64 = 2 << 20
	MaxMembers                  = 100000
	MaxMetadataFileBytes  int64 = 2 << 20
	MaxMetadataTotalBytes int64 = 8 << 20
	MaxMetadataLines            = 100000
	MaxLineBytes                = 4096
	MaxExpandedBytes      int64 = 2 << 30
	RustInstallerVersion        = "3\n"
	NoUpdatesDirectory          = "no-updates"
)

// ComponentExecutables are the executables a component payload must carry and
// that must be 64-bit little-endian ELF for the host architecture.
var ComponentExecutables = map[string][]string{
	"cargo":           {"bin/cargo"},
	"clippy-preview":  {"bin/cargo-clippy", "bin/clippy-driver"},
	"rustc":           {"bin/rustc"},
	"rustfmt-preview": {"bin/cargo-fmt", "bin/rustfmt"},
}

// ReviewedSigningFingerprints is the compiled signer set. It is compared for
// equality against the committed record by TestReviewedTrustIsTheCommittedRecord,
// so widening it is a test failure as well as a policy change.
var ReviewedSigningFingerprints = []Fingerprint{ReviewedPrimaryFingerprint}

// SupportedRustupVersions is the audited installer set. The format preference,
// installation argv and hash-only bootstrap exception are properties of the
// audited installer, not of "a rustup", so a new version needs code and policy
// review rather than the existing record.
var SupportedRustupVersions = []string{RequiredRustupVersion}

// Packages are the distribution package names the pin may select.
var Packages = []string{"cargo", "clippy-preview", "rust-std", "rustc", "rustfmt-preview"}

// FilenameStems maps a distribution package name to its archive filename stem.
// The signed manifest renames clippy/rustfmt to *-preview while the tarball
// keeps the short name.
var FilenameStems = map[string]string{
	"cargo":           "cargo",
	"clippy-preview":  "clippy",
	"rust-std":        "rust-std",
	"rustc":           "rustc",
	"rustfmt-preview": "rustfmt",
}

// CLIComponents are the explicitly installed components: the example needs
// clippy and rustfmt beyond the minimal profile.
var CLIComponents = []string{"clippy", "rustfmt"}

// ReviewedProfile is rustup 1.28.2's minimal profile for a Linux host.
var ReviewedProfile = []string{"rustc", "cargo", "rust-std", "rust-mingw"}

// ReviewedRenames are the manifest's reviewed package renames.
var ReviewedRenames = [][2]string{{"clippy", "clippy-preview"}, {"rustfmt", "rustfmt-preview"}}

// ComponentKeys are the only keys a rustup component row may carry. rustup's
// `is_extension` changes none of the lookups modelled here, so a row carrying it
// (or any other unknown key, which rustup's deserializer silently drops) is
// refused for review instead of being reinterpreted.
var ComponentKeys = []string{"pkg", "target"}

// SelectedEntryKeys are the keys a selected archive's manifest entry may carry.
var SelectedEntryKeys = []string{"available", "url", "hash", "xz_url", "xz_hash"}

// RustHostEntryKeys adds the two merged component tables the rust package's
// per-host entry carries.
var RustHostEntryKeys = append(append([]string{}, SelectedEntryKeys...), "components", "extensions")

// settingsKeys are the rustup settings.toml values the installer must have
// written, in the source reader's order.
var settingsKeys = [][2]string{
	{"profile", Profile},
	{"auto_self_update", "disable"},
	{"auto_install", "disable"},
}

// ambientToolchainVariables would let the ambient environment choose or fetch a
// different toolchain. The child environment is built, never inherited.
var ambientToolchainVariables = []string{
	"RUSTUP_TOOLCHAIN", "RUSTUP_VERSION", "RUSTUP_USE_CURL", "RUSTUP_USE_REQWEST",
	"RUSTUP_DIST_SERVER", "RUSTUP_UPDATE_ROOT", "RUSTUP_HOME", "CARGO_HOME",
	"RUSTC", "RUSTC_WRAPPER", "RUSTC_WORKSPACE_WRAPPER", "LD_PRELOAD",
	"LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "RUSTFLAGS",
}

// TrustPolicy is one reviewed trust: a single primary key and the signers a
// manifest may use.
type TrustPolicy struct {
	primary Fingerprint
	signers []Fingerprint
}

// TrustRoot builds a policy. It is called by the reviewed constant below and by
// the `example_fixture` seam, which is the only other way to reach it.
func TrustRoot(primary Fingerprint, signers []Fingerprint) TrustPolicy {
	return TrustPolicy{primary: primary, signers: append([]Fingerprint{}, signers...)}
}

// ReviewedTrust is the compiled policy every shipped build uses.
var ReviewedTrust = TrustRoot(ReviewedPrimaryFingerprint, ReviewedSigningFingerprints)

// Primary is the policy's trust root.
func (p TrustPolicy) Primary() Fingerprint { return p.primary }

// Signers is the policy's allowed signer set.
func (p TrustPolicy) Signers() []Fingerprint { return append([]Fingerprint{}, p.signers...) }

// Allows reports whether signer is a reviewed signing key.
func (p TrustPolicy) Allows(signer Fingerprint) bool {
	for _, allowed := range p.signers {
		if allowed == signer {
			return true
		}
	}
	return false
}

// requireReviewedTrust applies the two reviewed-trust rules in the source
// reader's order: the primary first, then every signer.
func requireReviewedTrust(primary Fingerprint, signers []Fingerprint, policy TrustPolicy) error {
	if primary != policy.primary {
		return refuse("pin trust root %s is not the reviewed primary key %s; a new key "+
			"needs a reviewed policy change", primary, policy.primary)
	}
	for _, signer := range signers {
		if !policy.Allows(signer) {
			return refuse("pin signing key %s is not in the reviewed signer set", signer)
		}
	}
	return nil
}
