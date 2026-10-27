package main

import "testing"

func TestGreeting(t *testing.T) {
	if got := greeting(); got != "go example layer smoke OK" {
		t.Fatalf("greeting() = %q", got)
	}
}
