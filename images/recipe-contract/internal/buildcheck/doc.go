// Package buildcheck verifies properties of the Go toolchain the *image* builder
// supplies, not the host's.
//
// Its checks live behind the `recipe_contract_image_compiler` build tag so
// ordinary `go test ./...` never asserts an exact patch: a supported newer host
// compiler is allowed, while a stale pinned image compiler must fail the opt-in
// builder check.
package buildcheck
