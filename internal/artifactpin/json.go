package artifactpin

import (
	"errors"
	"fmt"

	"github.com/gregwebs/agent-vm-images/internal/strictjson"
)

// Refusal is a pin, archive or evidence file this repository will not act on.
// The message is part of the contract: the suites match diagnostic fragments.
//
// It is the shared strict-JSON refusal, so one decoder decides what a reviewed
// document may contain and the artifact and Claude loaders cannot drift apart.
type Refusal = strictjson.Refusal

// refuse builds a Refusal.
func refuse(format string, args ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, args...)}
}

// IsRefusal reports whether err is a refusal rather than an I/O or programming
// failure, so callers can map it onto the refusal exit status.
func IsRefusal(err error) bool {
	var refusal *Refusal
	return errors.As(err, &refusal)
}

// object is a decoded JSON object, array, string, bool or integer. A fraction,
// an exponent, NaN and Infinity never appear: the decoder refuses them.
type object = strictjson.Value

// strictJSON decodes one complete JSON document with no duplicate keys and no
// trailing input.
func strictJSON(raw []byte, what string) (object, error) {
	return strictjson.Decode(raw, what)
}

func fields(value object, keys []string, what string) (map[string]object, error) {
	return strictjson.Fields(value, keys, what)
}

func requireList(value object, what string) ([]object, error) {
	return strictjson.List(value, what)
}

func requireText(value object, what string) (string, error) { return strictjson.Text(value, what) }

func requireInteger(value object, what string) (int64, error) {
	return strictjson.Integer(value, what)
}

func requireBool(value object, what string) (bool, error) { return strictjson.Bool(value, what) }
