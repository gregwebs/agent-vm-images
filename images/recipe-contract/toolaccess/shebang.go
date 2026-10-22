package toolaccess

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/legacy"
)

type interpreter struct {
	command string
	envPath pathOverride
}

// parseShebang returns every (interpreter, effective PATH) pair the kernel and
// env would use for this script.
func parseShebang(contents []byte, inheritedPath pathOverride, path string) ([]interpreter, error) {
	if !bytes.HasPrefix(contents, []byte("#!")) {
		return nil, nil
	}
	line := contents
	if index := bytes.IndexByte(contents, '\n'); index >= 0 {
		line = contents[:index]
	}
	line = line[2:]
	name, argument, ok := splitShebangLine(line)
	if !ok {
		return nil, violation(path, "shebang names no interpreter")
	}
	interpreterPath := string(name)
	if string(baseName(name)) == "env" {
		command, override, err := envInterpreters(argument, interpreterPath)
		if err != nil {
			return nil, err
		}
		effective := inheritedPath
		if override.set {
			effective = override
		}
		return []interpreter{{command: interpreterPath, envPath: inheritedPath}, {command: command, envPath: effective}}, nil
	}
	return []interpreter{{command: interpreterPath, envPath: inheritedPath}}, nil
}

// splitShebangLine splits a `#!` line the way the Linux kernel does: skip
// leading spaces/tabs, take the interpreter up to the next space/tab, skip that
// run, and pass the remainder (trailing spaces/tabs trimmed) as ONE argument. A
// carriage return is not whitespace, so a CRLF shebang names `/bin/sh\r`.
func splitShebangLine(line []byte) (name, argument []byte, ok bool) {
	start := 0
	for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
		start++
	}
	body := line[start:]
	if len(body) == 0 {
		return nil, nil, false
	}
	for index, b := range body {
		if b == ' ' || b == '\t' {
			name = body[:index]
			argument = bytes.Trim(body[index:], " \t")
			if len(argument) == 0 {
				argument = nil
			}
			break
		}
	}
	if name == nil && len(body) > 0 {
		name = body
	}
	if len(name) == 0 {
		return nil, nil, false
	}
	return name, argument, true
}

func baseName(path []byte) []byte {
	if index := bytes.LastIndexByte(path, '/'); index >= 0 {
		return path[index+1:]
	}
	return path
}

// envInterpreters parses `#!/usr/bin/env ...`. `argument` is the single optional
// argument the kernel hands env. Without `-S` the whole argument is one word, so
// `#!/usr/bin/env sh -e` names a program literally called `sh -e`.
func envInterpreters(argument []byte, env string) (string, pathOverride, error) {
	if argument == nil {
		return "", noPathOverride, violation(env, "env shebang names no interpreter")
	}
	var tokens [][]byte
	if string(argument) == "-S" {
		return "", noPathOverride, violation(env, "env -S needs its split string in the same argument")
	}
	if bytes.HasPrefix(argument, []byte("-S")) {
		raw := strings.TrimLeft(string(argument[2:]), " \t")
		split, err := splitSplitString(raw, env)
		if err != nil {
			return "", noPathOverride, err
		}
		for _, word := range split {
			tokens = append(tokens, []byte(word))
		}
	} else {
		tokens = [][]byte{argument}
	}

	override := noPathOverride
	index := 0
optionLoop:
	for index < len(tokens) {
		token := string(tokens[index])
		switch {
		case token == "--":
			index++
			break optionLoop
		case strings.HasPrefix(token, "-") && token != "-":
			switch token {
			case "-i", "--ignore-environment":
				return "", noPathOverride, violation(env, "env -i changes the interpreter lookup")
			case "-u", "--unset":
				index++
				if index >= len(tokens) {
					return "", noPathOverride, violation(env, "env -u needs an argument")
				}
				if string(tokens[index]) == "PATH" {
					return "", noPathOverride, violation(env, "env -u PATH changes the interpreter lookup")
				}
				index++
				continue
			case "-C", "--chdir":
				return "", noPathOverride, violation(env, "env -C changes the interpreter lookup")
			case "-0", "--null", "-v", "--debug":
				index++
				continue
			}
			if strings.HasPrefix(token, "--unset=") {
				if strings.TrimPrefix(token, "--unset=") == "PATH" {
					return "", noPathOverride, violation(env, "env -u PATH changes the interpreter lookup")
				}
				index++
				continue
			}
			if strings.HasPrefix(token, "--chdir=") {
				return "", noPathOverride, violation(env, "env -C changes the interpreter lookup")
			}
			return "", noPathOverride, violation(env, "unsupported env option "+legacy.ReprReplacing(string(tokens[index])))
		case strings.Contains(token, "=") && !strings.HasPrefix(token, "="):
			name, value, _ := strings.Cut(token, "=")
			if name == "PATH" {
				override = explicitPath(value)
			}
			index++
			continue
		default:
			break optionLoop
		}
	}
	if index >= len(tokens) {
		return "", noPathOverride, violation(env, "env shebang names no interpreter")
	}
	return string(tokens[index]), override, nil
}

// splitSplitString splits an `env -S` string the way GNU env would, or refuses a
// quoting form whose exact word-splitting is not modelled.
func splitSplitString(raw, env string) ([]string, error) {
	if strings.ContainsAny(raw, "\\'\"") {
		return nil, violation(env, "unsupported env -S quoting")
	}
	return splitPythonWhitespace(raw), nil
}

// splitPythonWhitespace reproduces str.split(): Unicode whitespace, including
// the C0 separators U+001C..U+001F that Go's unicode.IsSpace omits.
func splitPythonWhitespace(value string) []string {
	var words []string
	var current strings.Builder
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		if !isPythonSpace(r) {
			current.WriteString(value[:size])
			value = value[size:]
			continue
		}
		if current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}
		value = value[size:]
	}
	if current.Len() > 0 {
		words = append(words, current.String())
	}
	return words
}

func isPythonSpace(r rune) bool {
	if r >= 0x1C && r <= 0x1F {
		return true
	}
	return unicode.IsSpace(r)
}
