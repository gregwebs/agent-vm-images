package pnpmlock

import (
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

// Selector is one reviewed Cordis override key. The set is closed: an unknown
// selector is an unexpected key, not a new override.
type Selector uint8

const (
	SelectorCordis Selector = iota
	SelectorLoader
	SelectorTimer
	SelectorInclude
)

// selectorOrder is the sorted name order JavaScript's Object.fromEntries sort
// produced, and therefore the emission order of a derived override map.
var selectorOrder = []Selector{SelectorCordis, SelectorInclude, SelectorLoader, SelectorTimer}

func (s Selector) String() string {
	switch s {
	case SelectorCordis:
		return "@deepseek-ai/dsh>@deepseek-ai/cordis"
	case SelectorLoader:
		return "@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-loader"
	case SelectorTimer:
		return "@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer"
	case SelectorInclude:
		return "@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-include"
	default:
		panic("pnpmlock: unhandled selector")
	}
}

// Peer is the family name a selector pins.
func (s Selector) Peer() PackageName {
	switch s {
	case SelectorCordis:
		return PackageName{value: "@deepseek-ai/cordis"}
	case SelectorLoader:
		return PackageName{value: "@deepseek-ai/cordis-plugin-loader"}
	case SelectorTimer:
		return PackageName{value: "@deepseek-ai/cordis-plugin-timer"}
	case SelectorInclude:
		return PackageName{value: "@deepseek-ai/cordis-plugin-include"}
	default:
		panic("pnpmlock: unhandled selector")
	}
}

// ParseSelector reads a selector name.
func ParseSelector(text string) (Selector, bool) {
	for _, selector := range selectorOrder {
		if selector.String() == text {
			return selector, true
		}
	}
	return SelectorCordis, false
}

func selectorNames() []string {
	names := make([]string, 0, len(selectorOrder))
	for _, selector := range selectorOrder {
		names = append(names, selector.String())
	}
	return names
}

// OverrideEntry is one derived or reviewed override.
type OverrideEntry struct {
	Selector Selector
	Version  ExactVersion
}

// OverrideMap is a complete override map in selector order. The zero value is
// the retired `undefined`, which the derivation treats as "no reviewed map".
type OverrideMap struct{ entries []OverrideEntry }

// Entries returns the map in selector order.
func (m OverrideMap) Entries() []OverrideEntry { return m.entries }

// Get returns the reviewed version for a selector.
func (m OverrideMap) Get(selector Selector) (ExactVersion, bool) {
	for _, entry := range m.entries {
		if entry.Selector == selector {
			return entry.Version, true
		}
	}
	return ExactVersion{}, false
}

// Value renders the map as the JSON object the proposal file and graph carry.
func (m OverrideMap) Value() document.Value {
	entries := make([]mappingEntry, 0, len(m.entries))
	for _, item := range m.entries {
		entries = append(entries, entry(item.Selector.String(), jsStringValue(item.Version.String())))
	}
	return mappingValue(entries...)
}

// Equal compares two maps by selector and version.
func (m OverrideMap) Equal(other OverrideMap) bool {
	if len(m.entries) != len(other.entries) {
		return false
	}
	for _, entry := range m.entries {
		version, ok := other.Get(entry.Selector)
		if !ok || version != entry.Version {
			return false
		}
	}
	return true
}

// ParseOverrideMap validates a complete override map value.
func ParseOverrideMap(value document.Value) (OverrideMap, error) {
	if isUndefined(value) {
		return OverrideMap{}, nil
	}
	if _, err := recordOf(value); err != nil {
		return OverrideMap{}, err
	}
	if err := checkKeys(value, selectorNames(), selectorNames()); err != nil {
		return OverrideMap{}, err
	}
	map_ := OverrideMap{entries: make([]OverrideEntry, 0, len(selectorOrder))}
	for _, selector := range selectorOrder {
		version, err := exactVersionOf(value.Get(selector.String()))
		if err != nil {
			return OverrideMap{}, err
		}
		map_.entries = append(map_.entries, OverrideEntry{Selector: selector, Version: version})
	}
	return map_, nil
}

// ReadOverrideMap reads a reviewer-supplied override file. It takes the bounded
// JSON path plus the same duplicate and policy checks as the workspace.
func ReadOverrideMap(file string) (OverrideMap, error) {
	value, err := document.ReadFile(file, document.JSON)
	if err != nil {
		return OverrideMap{}, err
	}
	return ParseOverrideMap(value)
}

// validateWorkspace applies the closed per-consumer workspace policy.
func validateWorkspace(workspace document.Value, kind Kind, mode readMode) error {
	if _, err := recordOf(workspace); err != nil {
		return err
	}
	policy := workspacePolicy(kind)
	allowed := make([]string, 0, len(policy)+1)
	for _, item := range policy {
		allowed = append(allowed, item.key)
	}
	if kind == KindDsh {
		allowed = append(allowed, "overrides")
	}
	if err := checkKeys(workspace, allowed, allowedKeyNames(policy)); err != nil {
		return err
	}
	for _, item := range policy {
		if !deepEqual(workspace.Get(item.key), item.value) {
			return errorf("workspace policy: %s", item.key)
		}
	}
	if kind == KindDsh && mode == readTentative {
		if !isUndefined(workspace.Get("overrides")) {
			return errorf("tentative derivation must remove old overrides")
		}
		return nil
	}
	if kind == KindDsh && (mode != readReport || !isUndefined(workspace.Get("overrides"))) {
		// A report may describe a graph with no overrides (the negative leg's own
		// re-resolution), so the map is validated whenever it is present.
		overrides, err := recordOf(workspace.Get("overrides"))
		if err != nil {
			return err
		}
		if err := checkKeys(overrides, selectorNames(), selectorNames()); err != nil {
			return err
		}
		for _, name := range selectorNames() {
			if _, err := exactVersionOf(overrides.Get(name)); err != nil {
				return err
			}
		}
	}
	return nil
}

type policyItem struct {
	key   string
	value document.Value
}

func allowedKeyNames(items []policyItem) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.key)
	}
	return names
}

func stringSequence(values ...string) document.Value {
	items := make([]document.Value, 0, len(values))
	for _, value := range values {
		items = append(items, jsStringValue(value))
	}
	return sequenceValue(items)
}

// workspacePolicy is the closed workspace map per consumer, in the retired
// insertion order so deepEqual diagnostics and key checks agree with it.
func workspacePolicy(kind Kind) []policyItem {
	common := []policyItem{
		{"packages", stringSequence(".")},
		{"autoInstallPeers", boolValue(kind != KindBridge)},
		{"strictPeerDependencies", boolValue(false)},
		{"enableGlobalVirtualStore", boolValue(false)},
		{"pmOnFail", jsStringValue("error")},
		{"sideEffectsCache", boolValue(false)},
		{"allowBuilds", mappingValue()},
		{"extendNodePath", boolValue(false)},
	}
	switch kind {
	case KindDsh:
		return append(common,
			policyItem{"nodeLinker", jsStringValue("isolated")},
			policyItem{"hoist", boolValue(true)},
			policyItem{"trustPolicyExclude", stringSequence("chokidar@4.0.3")})
	case KindPi:
		return append(common,
			policyItem{"nodeLinker", jsStringValue("isolated")},
			policyItem{"hoist", boolValue(false)},
			policyItem{"preferSymlinkedExecutables", boolValue(true)},
			policyItem{"trustPolicyExclude", stringSequence("undici-types@6.21.0")})
	case KindBridge:
		return append(common,
			policyItem{"nodeLinker", jsStringValue("hoisted")},
			policyItem{"ignoredOptionalDependencies", stringSequence("@anthropic-ai/claude-agent-sdk-*")})
	default:
		panic("pnpmlock: unhandled kind")
	}
}
