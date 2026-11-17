package claudepin

import (
	evidencepkg "github.com/gregwebs/agent-vm-images/internal/evidence"
)

// Load is the reviewed closure's structure, with `pin.json` itself proven regular.
func Load(directory string) (Pin, error) {
	path, err := evidencepkg.RegularFile(directory, "pin.json", "pin.json")
	if err != nil {
		return Pin{}, err
	}
	raw, err := evidencepkg.ReadBounded(path, MaxPinBytes, "pin.json")
	if err != nil {
		return Pin{}, err
	}
	return Parse(raw, directory)
}

// EvidenceBytes is one committed evidence file, re-hashed now, before any use.
func (p Pin) EvidenceBytes(relative, expected string) ([]byte, error) {
	what := "committed " + relative
	path, err := evidencepkg.RegularFile(p.Directory, relative, what)
	if err != nil {
		return nil, err
	}
	raw, err := evidencepkg.ReadBounded(path, MaxEvidenceBytes, what)
	if err != nil {
		return nil, err
	}
	if evidencepkg.Sha256Of(raw) != expected {
		return nil, Refuse("%s does not match the pin digest", what)
	}
	return raw, nil
}
