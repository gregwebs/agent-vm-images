// Package buildcheck verifies properties of the Go toolchain and module layout a
// checkout and a CI runner must agree on.
//
// The exact-patch check lives behind the `recipe_contract_image_compiler` build
// tag: ordinary `go test` never asserts a patch release, because a supported
// newer compiler is allowed for development, while CI asserts the toolchain
// go.mod selects. The tag name is historical (ADR 0004 built the helper inside
// the image); ADR 0017 moved compilation to the host, and
// script/ci/assert-go-toolchain.sh is what runs it.
package buildcheck
