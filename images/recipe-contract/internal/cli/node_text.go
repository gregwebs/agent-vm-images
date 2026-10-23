package cli

// The ported helpers keep the retired diagnostic prefixes byte for byte: the
// callers, the fixtures and the Rust guards match on them.
const (
	pnpmLockPrefix        = "pnpm-lock: "
	pnpmEnvironmentPrefix = "pnpm environment: "
	pnpmYAMLPrefix        = "pnpm-yaml: "
	dshLockUpdatePrefix   = "dsh lock update: "
	dshDerivationPrefix   = "dsh Cordis derivation: "
	dshUpgradePrefix      = "dsh upgrade: "
)

// Usage lines name the new invocation. Every consumer moves in the same commit,
// so nothing matches the retired `.mjs`/`.js` spellings (ADR 0004 already froze
// its own usage text for a parser that kept running, which is not this case).
const (
	pnpmLockUsage      = "usage: recipe-contract pnpm-lock check|graph|pin|peers dsh|pi|bridge PREFIX\n"
	pnpmYAMLUsage      = "usage: recipe-contract pnpm-yaml FILE\n"
	dshLockUpdateUsage = "usage: recipe-contract dsh-lock-update OLD_PREFIX NEW_PREFIX DSH_VERSION [--refresh-lock] [--json]\n"
	dshDerivationUsage = "usage: recipe-contract dsh-cordis-overrides TENTATIVE_PREFIX [--before COMMITTED_PREFIX] [--propose FILE | --cordis-overrides FILE]\n"
)
