package closure

import (
	"bytes"
	"sort"
	"strings"
)

// This file owns the two go.mod grammars, which are deliberately different.
//
//   * The wrapper `go.mod` is this repository's own file: it must be exactly the
//     canonical bytes `renderWrapper` produces, so a hand edit of any kind is a
//     refusal rather than a parser question.
//   * `gopls.go.mod` is upstream evidence: the bytes the proxy served. Its
//     grammar is Go's, not ours, so the reader binds the four facts the closure
//     uses -- module identity, the go directive and its requires -- with Go's
//     separators (runs of blanks, trailing `//` comments) and refuses the
//     directives that would change what compiles.
//
// The evidence grammar used to be lenient in three ways that mattered (#188,
// owner ruling G3): it never read the `module` directive, so a wrong one passed;
// it accepted an unfinished `require` block, so requirements after the missing
// `)` were silently read as a block body or dropped; and it split the directive
// on one space, so `require\tpath version` was invisible. Each is now a named
// refusal with its own test, and `TestRootEvidenceAgreesWithTheGoCommand`
// measures the wider grammar against the committed bytes.

// WrapperGoMod is the reviewed wrapper module: the go directive and the exact
// selection.
type WrapperGoMod struct {
	Go       GoLanguage
	Requires map[ModulePath]ModuleVersion
}

// RootGoMod is one projection of the upstream root module's evidence.
type RootGoMod struct {
	Module   ModulePath
	Go       GoLanguage
	Requires map[ModulePath]ModuleVersion
}

// ParseRootGoMod reads upstream root evidence the writer has just downloaded (#252).
// It is the same grammar the loader applies to the committed evidence file, exported
// because the stager must know the upstream `go` directive before it retargets the
// wrapper. A caller that has committed bytes should use Load instead: the loader also
// binds the evidence hash.
func ParseRootGoMod(raw []byte, what string) (RootGoMod, error) {
	return parseRootGoMod(raw, what)
}

// renderWrapper is the one canonical form: a byte comparison against this
// rendering, not a parser leniency, is what accepts the committed wrapper.
func renderWrapper(language GoLanguage, requires map[ModulePath]ModuleVersion) []byte {
	paths := make([]string, 0, len(requires))
	for path := range requires {
		paths = append(paths, string(path))
	}
	sort.Strings(paths)
	lines := []string{"module " + Wrapper, "", "go " + string(language), "", "tool " + Gopls,
		"", "require ("}
	for _, path := range paths {
		lines = append(lines, "\t"+path+" "+string(requires[ModulePath(path)])+" // indirect")
	}
	lines = append(lines, ")", "")
	return []byte(strings.Join(lines, "\n"))
}

// canonicalRequireEntry splits `path version` with one space between the fields,
// optionally followed by ` // indirect`.
func canonicalRequireEntry(line, what string, allowIndirect bool) (ModulePath, ModuleVersion, error) {
	fields := strings.Split(line, " ")
	if len(fields) == 4 && fields[2] == "//" && fields[3] == "indirect" {
		if !allowIndirect {
			return "", "", refuse("%s is not in the reviewed canonical form", what)
		}
		path, err := parseModulePath(fields[0], what)
		if err != nil {
			return "", "", err
		}
		version, err := parseModuleVersion(fields[1], what)
		return path, version, err
	}
	if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
		return "", "", refuse("%s is not in the reviewed canonical form", what)
	}
	path, err := parseModulePath(fields[0], what)
	if err != nil {
		return "", "", err
	}
	version, err := parseModuleVersion(fields[1], what)
	return path, version, err
}

// parseWrapperGoMod reads our own wrapper file. Every branch here exists to
// produce the reviewed canonical refusal; the byte comparison in Load is the
// authority, and this parse supplies the values that comparison renders.
func parseWrapperGoMod(raw []byte, what string) (WrapperGoMod, error) {
	text, err := decodeText(raw, what)
	if err != nil {
		return WrapperGoMod{}, err
	}
	if !strings.HasSuffix(text, "\n") {
		return WrapperGoMod{}, refuse("%s is not in the reviewed canonical form", what)
	}
	lines := strings.Split(text, "\n")
	lines = lines[:len(lines)-1]
	var module *string
	var language *GoLanguage
	tools := []string{}
	requires := map[ModulePath]ModuleVersion{}
	inBlock := false
	for _, line := range lines {
		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
			entry := strings.TrimPrefix(line, "\t")
			path, version, err := canonicalRequireEntry(entry, what, true)
			if err != nil {
				return WrapperGoMod{}, err
			}
			if _, duplicate := requires[path]; duplicate {
				return WrapperGoMod{}, refuse("%s is not in the reviewed canonical form", what)
			}
			requires[path] = version
			continue
		}
		if line == "" {
			continue
		}
		directive, rest := cutDirective(line)
		if contains(ForbiddenWrapperDirectives, directive) {
			return WrapperGoMod{}, refuse("directive not allowed: %s", directive)
		}
		switch directive {
		case "module":
			module = &rest
		case "go":
			parsed, err := parseGoLanguage(rest, what+" go directive")
			if err != nil {
				return WrapperGoMod{}, err
			}
			language = &parsed
		case "tool":
			tools = append(tools, rest)
		case "require":
			if rest == "(" {
				inBlock = true
				continue
			}
			path, version, err := canonicalRequireEntry(rest, what, false)
			if err != nil {
				return WrapperGoMod{}, err
			}
			if _, duplicate := requires[path]; duplicate {
				return WrapperGoMod{}, refuse("%s is not in the reviewed canonical form", what)
			}
			requires[path] = version
		default:
			return WrapperGoMod{}, refuse("%s is not in the reviewed canonical form", what)
		}
	}
	if inBlock || module == nil || language == nil {
		return WrapperGoMod{}, refuse("%s is not in the reviewed canonical form", what)
	}
	parsedModule, err := parseModulePath(*module, what+" module directive")
	if err != nil {
		return WrapperGoMod{}, err
	}
	if string(parsedModule) != Wrapper {
		return WrapperGoMod{}, refuse("%s: module directive must be %s", what, Wrapper)
	}
	if len(tools) != 1 || tools[0] != Gopls {
		return WrapperGoMod{}, refuse("%s: tool directive must be exactly %s", what, Gopls)
	}
	return WrapperGoMod{Go: *language, Requires: requires}, nil
}

// cutDirective splits a go.mod line into its directive and the rest, with any run
// of blanks as the separator and the remainder trimmed. Go accepts that; the
// Python evidence reader split on one space, which is defect G3 (a
// tab-separated directive was invisible to it).
func cutDirective(line string) (string, string) {
	index := strings.IndexFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
	if index < 0 {
		return line, ""
	}
	return line[:index], strings.TrimLeft(line[index:], " \t")
}

// parseRootGoMod reads the upstream root module's committed bytes as evidence.
func parseRootGoMod(raw []byte, what string) (RootGoMod, error) {
	text, err := decodeText(raw, what)
	if err != nil {
		return RootGoMod{}, err
	}
	var module *ModulePath
	var language *GoLanguage
	requires := map[ModulePath]ModuleVersion{}
	inBlock := false
	for _, line := range strings.Split(text, "\n") {
		stripped := strings.TrimSpace(line)
		if inBlock {
			if stripped == ")" {
				inBlock = false
				continue
			}
			if stripped == "" || strings.HasPrefix(stripped, "//") {
				continue
			}
			path, version, err := evidenceRequireEntry(stripped, what)
			if err != nil {
				return RootGoMod{}, err
			}
			requires[path] = version
			continue
		}
		if stripped == "" || strings.HasPrefix(stripped, "//") {
			continue
		}
		directive, rest := cutDirective(stripped)
		if contains(ForbiddenRootModuleDirective, directive) {
			return RootGoMod{}, refuse("root module declares %s; review required", directive)
		}
		switch directive {
		case "module":
			parsed, err := parseModulePath(rest, what+" module directive")
			if err != nil {
				return RootGoMod{}, err
			}
			if string(parsed) != Gopls {
				return RootGoMod{}, refuse("%s: the module directive must be %s; review required",
					what, Gopls)
			}
			module = &parsed
		case "go":
			parsed, err := parseGoLanguage(rest, what+" go directive")
			if err != nil {
				return RootGoMod{}, err
			}
			language = &parsed
		case "require":
			if rest == "(" {
				inBlock = true
				continue
			}
			path, version, err := evidenceRequireEntry(rest, what)
			if err != nil {
				return RootGoMod{}, err
			}
			requires[path] = version
		}
	}
	if inBlock {
		return RootGoMod{}, refuse("%s: an unterminated require block; review required", what)
	}
	if module == nil {
		return RootGoMod{}, refuse("%s: missing the module directive", what)
	}
	if language == nil {
		return RootGoMod{}, refuse("%s: missing the go directive", what)
	}
	return RootGoMod{Module: *module, Go: *language, Requires: requires}, nil
}

// evidenceRequireEntry reads one require entry with Go's separators: any run of
// blanks between the two fields, and a trailing `// indirect` comment.
func evidenceRequireEntry(line, what string) (ModulePath, ModuleVersion, error) {
	if comment := strings.Index(line, "//"); comment >= 0 {
		line = strings.TrimSpace(line[:comment])
	}
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return "", "", refuse("%s: a require entry must be a path and a version: %s", what,
			repr(line))
	}
	path, err := parseModulePath(fields[0], what)
	if err != nil {
		return "", "", err
	}
	version, err := parseModuleVersion(fields[1], what)
	return path, version, err
}

// decodeText reads one committed input as UTF-8 text, refusing a byte-order mark:
// a BOM is invisible in a diff and would move the first newline of every digest.
func decodeText(raw []byte, what string) (string, error) {
	if bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		return "", refuse("%s: leading UTF-8 BOM is not accepted", what)
	}
	text, err := decodeUTF8(raw)
	if err != nil {
		return "", refuse("%s: not valid UTF-8: %s", what, err)
	}
	return text, nil
}

func contains(list []string, name string) bool {
	for _, entry := range list {
		if entry == name {
			return true
		}
	}
	return false
}
