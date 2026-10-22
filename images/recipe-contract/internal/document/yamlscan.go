package document

import (
	"bytes"
	"errors"
	"fmt"
)

// The retired parser was npm yaml@2.9.0 with {uniqueKeys, merge:false,
// strict:true}. yaml.v3's node API cannot express three of those policies, and
// the policies are what ADR 0001's trust boundary is made of, so the lexical
// rules are reproduced here from the vendored lexer before Decode runs:
//
//   - an explicit tag indicator at a node start (yaml.v3 silently discards the
//     non-specific `!`);
//   - `%YAML`/`%TAG` directives (v3 accepts them without exposing the schema);
//   - raw NEL/LS/PS, which v3 turns into line breaks or folded spaces;
//   - quoted/flow continuation lines indented below the enclosing block, which
//     v3 folds into one scalar.
//
// Parsing, collection shape and scalar resolution stay yaml.v3's job. This is a
// guard, not a second grammar: it never rewrites the source and never produces
// values.

type scanState uint8

const (
	scanStream scanState = iota
	scanLineStart
	scanBlockStart
	scanDocument
	scanFlow
	scanQuoted
	scanBlockScalar
	scanPlain
)

// scanner is a transliteration of the npm lexer's state machine. States,
// transitions and the indentation bookkeeping are the lexer's because they
// decide which bytes are scalar content and which are node starts.
type scanner struct {
	source      []byte
	pos         int
	indentNext  int
	indentValue int
	flowLevel   int
	flowKey     bool
	// blockScalarIndent is the offset from the containing indentation set by an
	// explicit `|2`-style header; -1 when the header names none.
	blockScalarIndent int
}

var errTagForbidden = errors.New("anchors, aliases and explicit tags forbidden")

func scanYAMLSource(source []byte) error {
	if err := rejectLineSeparators(source); err != nil {
		return err
	}
	s := &scanner{source: source, blockScalarIndent: -1, indentNext: 0}
	state := scanStream
	for {
		var err error
		switch state {
		case scanStream:
			state, err = s.parseStream()
		case scanLineStart:
			state, err = s.parseLineStart()
		case scanBlockStart:
			state, err = s.parseBlockStart()
		case scanDocument:
			state, err = s.parseDocument()
		case scanFlow:
			state, err = s.parseFlowCollection()
		case scanQuoted:
			state, err = s.parseQuotedScalar()
		case scanBlockScalar:
			state, err = s.parseBlockScalar()
		case scanPlain:
			state, err = s.parsePlainScalar()
		default:
			return fmt.Errorf("unhandled scanner state")
		}
		if err != nil {
			return err
		}
		if s.pos >= len(s.source) {
			return nil
		}
	}
}

// rejectLineSeparators rejects raw U+0085, U+2028 and U+2029 anywhere in the
// source. npm yaml treats them as content; v3 treats them as line breaks, which
// changes the accepted value, so the input fails closed instead (D5).
func rejectLineSeparators(source []byte) error {
	for index := 0; index < len(source); index++ {
		if source[index] < 0xc2 {
			continue
		}
		switch {
		case bytes.HasPrefix(source[index:], []byte("\u0085")):
			return errors.New("unsupported line break character U+0085")
		case bytes.HasPrefix(source[index:], []byte("\u2028")):
			return errors.New("unsupported line break character U+2028")
		case bytes.HasPrefix(source[index:], []byte("\u2029")):
			return errors.New("unsupported line break character U+2029")
		}
	}
	return nil
}

func (s *scanner) at(index int) byte {
	if index < 0 || index >= len(s.source) {
		return 0
	}
	return s.source[index]
}

func isEmptyChar(c byte) bool {
	switch c {
	case 0, ' ', '\n', '\r', '\t':
		return true
	default:
		return false
	}
}

func isFlowIndicator(c byte) bool {
	switch c {
	case ',', '[', ']', '{', '}':
		return true
	default:
		return false
	}
}

func isNotAnchorChar(c byte) bool {
	return isEmptyChar(c) || isFlowIndicator(c)
}

// getLine returns the current line's text, without its line break.
func (s *scanner) getLine() []byte {
	end := bytes.IndexByte(s.source[s.pos:], '\n')
	if end < 0 {
		return s.source[s.pos:]
	}
	end += s.pos
	if end > s.pos && s.source[end-1] == '\r' {
		end--
	}
	return s.source[s.pos:end]
}

func (s *scanner) atLineEnd() bool {
	index := s.pos
	for c := s.at(index); c == ' ' || c == '\t'; c = s.at(index) {
		index++
	}
	switch c := s.at(index); c {
	case 0, '#', '\n':
		return true
	case '\r':
		return s.at(index+1) == '\n'
	default:
		return false
	}
}

func (s *scanner) pushCount(count int) int {
	s.pos += count
	return count
}

func (s *scanner) pushSpaces(allowTabs bool) int {
	index := s.pos
	for {
		c := s.at(index)
		if c == ' ' || (allowTabs && c == '\t') {
			index++
			continue
		}
		break
	}
	count := index - s.pos
	s.pos = index
	return count
}

func (s *scanner) pushNewline() int {
	c := s.at(s.pos)
	if c == '\n' {
		return s.pushCount(1)
	}
	if c == '\r' && s.at(s.pos+1) == '\n' {
		return s.pushCount(2)
	}
	return 0
}

func (s *scanner) pushUntil(test func(byte) bool) int {
	index := s.pos
	for !test(s.at(index)) {
		index++
	}
	return s.pushCount(index - s.pos)
}

// lineColumn turns a byte offset into the 1-based line and column diagnostics
// use.
func (s *scanner) lineColumn(offset int) (int, int) {
	line, lineStart := 1, 0
	for index := 0; index < offset && index < len(s.source); index++ {
		if s.source[index] == '\n' {
			line++
			lineStart = index + 1
		}
	}
	return line, offset - lineStart + 1
}

func (s *scanner) atLineMessage(offset int) error {
	line, column := s.lineColumn(offset)
	return fmt.Errorf("expected indented scalar continuation at line %d, column %d", line, column)
}

// continueScalar is the lexer's minimum-continuation-indentation rule: a
// non-blank continuation line must be indented at least indentNext, and a root
// scalar may not continue past a document boundary.
func (s *scanner) continueScalar(offset int) int {
	c := s.at(offset)
	if s.indentNext > 0 {
		indent := 0
		for c == ' ' {
			indent++
			c = s.at(offset + indent)
		}
		if c == '\r' {
			if s.at(offset+indent+1) == '\n' {
				return offset + indent + 1
			}
		}
		if c == '\n' || indent >= s.indentNext {
			return offset + indent
		}
		return -1
	}
	if c == '-' || c == '.' {
		marker := string(s.source[offset:min(offset+3, len(s.source))])
		if (marker == "---" || marker == "...") && isEmptyChar(s.at(offset+3)) {
			return -1
		}
	}
	return offset
}

func (s *scanner) parseStream() (scanState, error) {
	line := s.getLine()
	if len(line) > 0 && line[0] == '%' {
		lineNumber, _ := s.lineColumn(s.pos)
		return scanStream, fmt.Errorf("YAML directives are forbidden at line %d", lineNumber)
	}
	if s.atLineEnd() {
		spaces := s.pushSpaces(true)
		s.pushCount(len(line) - spaces)
		s.pushNewline()
		return scanStream, nil
	}
	return s.parseLineStart()
}

func (s *scanner) parseLineStart() (scanState, error) {
	c := s.at(s.pos)
	if c == '-' || c == '.' {
		marker := string(s.source[s.pos:min(s.pos+3, len(s.source))])
		if (marker == "---" || marker == "...") && isEmptyChar(s.at(s.pos+3)) {
			s.pushCount(3)
			s.indentValue = 0
			s.indentNext = 0
			if marker == "---" {
				return scanDocument, nil
			}
			return scanStream, nil
		}
	}
	s.indentValue = s.pushSpaces(false)
	if s.indentNext > s.indentValue && !isEmptyChar(s.at(s.pos+1)) {
		s.indentNext = s.indentValue
	}
	return s.parseBlockStart()
}

func (s *scanner) parseBlockStart() (scanState, error) {
	c0, c1 := s.at(s.pos), s.at(s.pos+1)
	if (c0 == '-' || c0 == '?' || c0 == ':') && isEmptyChar(c1) {
		count := s.pushCount(1) + s.pushSpaces(true)
		s.indentNext = s.indentValue + 1
		s.indentValue += count
		return scanBlockStart, nil
	}
	return scanDocument, nil
}

func (s *scanner) parseDocument() (scanState, error) {
	s.pushSpaces(true)
	line := s.getLine()
	n, indicatorErr := s.pushIndicators()
	if indicatorErr != nil {
		return scanDocument, indicatorErr
	}
	switch {
	case n >= len(line):
		s.pushNewline()
		return scanLineStart, nil
	}
	switch line[n] {
	case '#':
		s.pushCount(len(line) - n)
		s.pushNewline()
		return scanLineStart, nil
	case '{', '[':
		s.pushCount(1)
		s.flowKey = false
		s.flowLevel = 1
		return scanFlow, nil
	case '}', ']':
		s.pushCount(1)
		return scanDocument, nil
	case '*':
		s.pushUntil(isNotAnchorChar)
		return scanDocument, nil
	case '"', '\'':
		return scanQuoted, nil
	case '|', '>':
		n += s.parseBlockScalarHeader()
		n += s.pushSpaces(true)
		s.pushCount(len(line) - n)
		s.pushNewline()
		return scanBlockScalar, nil
	default:
		return scanPlain, nil
	}
}

// pushIndicators consumes the node properties and block indicators that may
// precede a node. A tag indicator is a policy violation here, before yaml.v3 can
// discard the non-specific `!`.
func (s *scanner) pushIndicators() (int, error) {
	n := 0
	for {
		switch s.at(s.pos) {
		case '!':
			return n, errTagForbidden
		case '&':
			n += s.pushUntil(isNotAnchorChar)
			n += s.pushSpaces(true)
			continue
		case '-', '?', ':':
			inFlow := s.flowLevel > 0
			c1 := s.at(s.pos + 1)
			if isEmptyChar(c1) || (inFlow && isFlowIndicator(c1)) {
				if !inFlow {
					s.indentNext = s.indentValue + 1
				} else if s.flowKey {
					s.flowKey = false
				}
				n += s.pushCount(1)
				n += s.pushSpaces(true)
				continue
			}
		}
		return n, nil
	}
}

func (s *scanner) parseFlowCollection() (scanState, error) {
	indent := -1
	for {
		lineBreaks := s.pushNewline()
		spaces := 0
		if lineBreaks > 0 {
			spaces = s.pushSpaces(false)
			s.indentValue = spaces
			indent = spaces
		}
		spaces += s.pushSpaces(true)
		if lineBreaks+spaces == 0 {
			break
		}
	}
	line := s.getLine()
	boundary := len(line) >= 3 && (string(line[:3]) == "---" || string(line[:3]) == "...") &&
		(len(line) == 3 || isEmptyChar(line[3]))
	if (indent != -1 && indent < s.indentNext && (len(line) == 0 || line[0] != '#')) || (indent == 0 && boundary) {
		atFlowEndMarker := indent == s.indentNext-1 && s.flowLevel == 1 && len(line) > 0 && (line[0] == ']' || line[0] == '}')
		if !atFlowEndMarker {
			return scanFlow, s.atLineMessage(s.pos)
		}
	}
	n := 0
	for n < len(line) && line[n] == ',' {
		n += s.pushCount(1)
		n += s.pushSpaces(true)
		s.flowKey = false
	}
	indicators, indicatorErr := s.pushIndicators()
	n += indicators
	if indicatorErr != nil {
		return scanFlow, indicatorErr
	}
	if n >= len(line) {
		return scanFlow, nil
	}
	switch line[n] {
	case '#':
		s.pushCount(len(line) - n)
		return scanFlow, nil
	case '{', '[':
		s.pushCount(1)
		s.flowKey = false
		s.flowLevel++
		return scanFlow, nil
	case '}', ']':
		s.pushCount(1)
		s.flowKey = true
		s.flowLevel--
		if s.flowLevel > 0 {
			return scanFlow, nil
		}
		return scanDocument, nil
	case '*':
		s.pushUntil(isNotAnchorChar)
		return scanFlow, nil
	case '"', '\'':
		s.flowKey = true
		return scanQuoted, nil
	case ':':
		next := s.at(s.pos + 1)
		if s.flowKey || isEmptyChar(next) || next == ',' {
			s.flowKey = false
			s.pushCount(1)
			s.pushSpaces(true)
			return scanFlow, nil
		}
	}
	s.flowKey = false
	return scanPlain, nil
}

func (s *scanner) parseQuotedScalar() (scanState, error) {
	quote := s.at(s.pos)
	end := bytes.IndexByte(s.source[s.pos+1:], quote)
	if end >= 0 {
		end += s.pos + 1
	}
	flow := s.flowLevel > 0
	if quote == '\'' {
		for end >= 0 && s.at(end+1) == '\'' {
			next := bytes.IndexByte(s.source[end+2:], '\'')
			if next < 0 {
				end = -1
				break
			}
			end += 2 + next
		}
	} else {
		for end >= 0 {
			backslashes := 0
			for s.at(end-1-backslashes) == '\\' {
				backslashes++
			}
			if backslashes%2 == 0 {
				break
			}
			next := bytes.IndexByte(s.source[end+1:], '"')
			if next < 0 {
				end = -1
				break
			}
			end += 1 + next
		}
	}
	if end < 0 {
		// Unterminated: leave the diagnostic to the decoder.
		s.pos = len(s.source)
		return scanDocument, nil
	}
	for offset := bytes.IndexByte(s.source[s.pos:end], '\n'); offset >= 0; {
		lineBreak := s.pos + offset
		continuation := s.continueScalar(lineBreak + 1)
		if continuation < 0 {
			return scanDocument, s.atLineMessage(lineBreak + 1)
		}
		if continuation >= end {
			break
		}
		next := bytes.IndexByte(s.source[continuation:end], '\n')
		if next < 0 {
			break
		}
		offset = continuation + next - s.pos
	}
	s.pos = end + 1
	if flow {
		return scanFlow, nil
	}
	return scanDocument, nil
}

func (s *scanner) parseBlockScalarHeader() int {
	s.blockScalarIndent = -1
	for index := s.pos + 1; ; index++ {
		c := s.at(index)
		switch {
		case c == '+':
		case c > '0' && c <= '9':
			s.blockScalarIndent = int(c-'0') - 1
		case c != '-':
			return s.pushUntil(func(c byte) bool { return isEmptyChar(c) || c == '#' })
		}
	}
}

func (s *scanner) parseBlockScalar() (scanState, error) {
	lineBreak := s.pos - 1
	indent := 0
	index := s.pos
	for {
		c := s.at(index)
		switch c {
		case 0:
			goto scan
		case ' ':
			indent++
		case '\n':
			lineBreak = index
			indent = 0
		case '\r':
			if s.at(index+1) != '\n' {
				goto scan
			}
		default:
			goto scan
		}
		index++
	}
scan:
	if indent >= s.indentNext {
		if s.blockScalarIndent == -1 {
			s.indentNext = indent
		} else if s.indentNext == 0 {
			s.indentNext = s.blockScalarIndent + 1
		} else {
			s.indentNext = s.blockScalarIndent + s.indentNext
		}
		for {
			continuation := s.continueScalar(lineBreak + 1)
			if continuation < 0 {
				break
			}
			next := bytes.IndexByte(s.source[continuation:], '\n')
			if next < 0 {
				lineBreak = len(s.source)
				break
			}
			lineBreak = continuation + next
		}
	}
	// Mirrors the lexer's trailing-content rule: an insufficiently indented line
	// ends the body and is rescanned as a node start, so a tag there is caught.
	trailing := lineBreak + 1
	for s.at(trailing) == ' ' {
		trailing++
	}
	if s.at(trailing) == '\t' {
		for {
			c := s.at(trailing)
			if c != '\t' && c != ' ' && c != '\r' && c != '\n' {
				break
			}
			trailing++
		}
		lineBreak = trailing - 1
	}
	if lineBreak+1 > s.pos {
		s.pos = lineBreak + 1
	}
	return scanLineStart, nil
}

func (s *scanner) parsePlainScalar() (scanState, error) {
	inFlow := s.flowLevel > 0
	end := s.pos - 1
	index := s.pos - 1
	for {
		index++
		c := s.at(index)
		if c == 0 {
			break
		}
		switch {
		case c == ':':
			next := s.at(index + 1)
			if isEmptyChar(next) || (inFlow && isFlowIndicator(next)) {
				goto done
			}
			end = index
		case isEmptyChar(c):
			next := s.at(index + 1)
			if c == '\r' {
				if next == '\n' {
					index++
					next = s.at(index + 1)
				} else {
					end = index
				}
			}
			if next == '#' || (inFlow && isFlowIndicator(next)) {
				goto done
			}
			if c == '\n' {
				continuation := s.continueScalar(index + 1)
				if continuation < 0 {
					goto done
				}
				if continuation-2 > index {
					index = continuation - 2
				}
			}
		default:
			if inFlow && isFlowIndicator(c) {
				goto done
			}
			end = index
		}
	}
done:
	if end+1 > s.pos {
		s.pos = end + 1
	}
	if inFlow {
		return scanFlow, nil
	}
	return scanDocument, nil
}
