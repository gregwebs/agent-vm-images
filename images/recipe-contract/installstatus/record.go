package installstatus

import (
	"bytes"
	"errors"
	"os"
	"strings"
)

// RecordKind is the closed vocabulary of recipe-contract install records.
type RecordKind uint8

const (
	// Invalid is the zero value: an unparsed record authorises nothing.
	Invalid RecordKind = iota
	Installed
	Pending
	AbsentPi
	AbsentTransport
)

// TransportCode is a classified network-failure code. The text is unexported and
// only the record grammar builds one, so a code that was never looked up in the
// curl/npm vocabulary cannot reach a caller.
type TransportCode struct{ text string }

// String returns the wire spelling the record carried.
func (c TransportCode) String() string { return c.text }

// ErrInvalidRecord reports a record that is not one exact known line. Callers
// turn it into the legacy status without printing anything.
var ErrInvalidRecord = errors.New("invalid recipe-contract record")

// curlCodes and npmCodes mirror the classified-transport vocabulary written by
// download.sh (curl exit codes) and run-npm.sh (npm error codes).
var curlCodes = map[string]struct{}{
	"5": {}, "6": {}, "7": {}, "18": {}, "28": {}, "35": {}, "52": {}, "55": {}, "56": {}, "60": {},
}

var npmCodes = map[string]struct{}{
	"EAI_AGAIN": {}, "ENOTFOUND": {}, "ECONNREFUSED": {}, "ECONNRESET": {}, "ETIMEDOUT": {},
}

const absentTransportPrefix = "absent-transport "

// Record is a validated install record; its fields are unexported so no caller
// can construct one that skipped the grammar.
type Record struct {
	kind RecordKind
	code TransportCode
}

// ParseRecord validates one complete record file's bytes.
func ParseRecord(data []byte) (Record, error) {
	if !bytes.HasSuffix(data, []byte("\n")) || bytes.Count(data, []byte("\n")) != 1 {
		return Record{}, ErrInvalidRecord
	}
	line := data[:len(data)-1]
	for _, b := range line {
		if b >= 0x80 {
			return Record{}, ErrInvalidRecord
		}
	}
	text := string(line)
	switch text {
	case "installed":
		return Record{kind: Installed}, nil
	case "pending":
		return Record{kind: Pending}, nil
	case "absent-pi":
		return Record{kind: AbsentPi}, nil
	}
	if codeText, ok := strings.CutPrefix(text, absentTransportPrefix); ok {
		if code, known := lookupCode(codeText); known {
			return Record{kind: AbsentTransport, code: code}, nil
		}
	}
	return Record{}, ErrInvalidRecord
}

// ReadRecord reads a record file. A missing, unreadable or non-regular file is
// an ordinary invalid record, not a distinguishable error: the legacy contract
// prints nothing for either.
func ReadRecord(path string) (Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, ErrInvalidRecord
	}
	return ParseRecord(data)
}

// Text renders the record's canonical one-line spelling.
func (r Record) Text() string {
	switch r.kind {
	case Installed:
		return "installed"
	case Pending:
		return "pending"
	case AbsentPi:
		return "absent-pi"
	case AbsentTransport:
		return absentTransportPrefix + r.code.String()
	case Invalid:
		return ""
	}
	return ""
}

// Kind reports the closed record alternative.
func (r Record) Kind() RecordKind { return r.kind }

// Code reports the transport code of an absent-transport record.
func (r Record) Code() (TransportCode, bool) {
	if r.kind != AbsentTransport {
		return TransportCode{}, false
	}
	return r.code, true
}

func lookupCode(text string) (TransportCode, bool) {
	if _, ok := curlCodes[text]; ok {
		return TransportCode{text: text}, true
	}
	if _, ok := npmCodes[text]; ok {
		return TransportCode{text: text}, true
	}
	return TransportCode{}, false
}
