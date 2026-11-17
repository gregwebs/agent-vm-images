package codexpin

import (
	"encoding/base64"
	"errors"
	"unicode/utf8"

	"github.com/gregwebs/agent-vm-images/internal/strictjson"
)

// decodeBase64 is the strict decoder a bundle's fields must satisfy: a field that
// only decodes with errors ignored is not the field the signer wrote.
func decodeBase64(value any, what string) ([]byte, error) {
	raw, err := strictjsonText(value, what)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, Refuse("%s is not base64", what)
	}
	return decoded, nil
}

// strictjsonText is the non-empty-string check, named here so every field in this
// package reports it the same way.
func strictjsonText(value any, what string) (string, error) {
	text, err := strictjson.Text(value, what)
	if err != nil {
		return "", err
	}
	return text, nil
}

// errorsAs keeps the refusal classification in one place, so a wrapped refusal is
// still a refusal.
func errorsAs(err error, target **Refusal) bool { return errors.As(err, target) }

func isUTF8(raw []byte) bool { return utf8.Valid(raw) }
