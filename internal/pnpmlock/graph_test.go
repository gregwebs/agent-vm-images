package pnpmlock

import (
	"strings"
	"testing"
)

// The retired resolver compared archiveKey(snapshot) to the full context key.
func TestNestedContextWithoutExactSnapshot(t *testing.T) {
	packageID, err := parseSnapshotID("@deepseek-ai/cordis@4.0.2")
	if err != nil {
		t.Fatal(err)
	}
	contextID, err := parseSnapshotID("@deepseek-ai/cordis@4.0.2(@deepseek-ai/dsh-sandbox-local@1.0.0)")
	if err != nil {
		t.Fatal(err)
	}
	index := indexSnapshots([]Snapshot{{ID: packageID}})
	_, err = index.resolve(PeerContextID(contextID), "owner")
	if err == nil || !strings.Contains(err.Error(), "dangling peer context") {
		t.Fatalf("got %v", err)
	}
}
