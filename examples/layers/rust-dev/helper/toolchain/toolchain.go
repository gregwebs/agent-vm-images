// Package toolchain is the rust-dev layer's toolchain library.
//
// It is outside `internal/` on purpose (ADR 0017): the root module imports it in
// slice 4, when `avm-pin rust-toolchain` writes the pin this layer installs, so
// the image and the writer cannot disagree about what a pin means. It may import
// this module's `internal/...` packages, because both share one import-path root.
//
// Slice 1 of #187 lands the package so the module has its final shape; the
// offline reader (`check`, `verify-installed`) is slice 2. Nothing here yet, and
// nothing may be added here that a shipped build could use to honour a fixture
// seam: the suite-only inputs live behind `//go:build example_fixture` in the
// `main` package.
package toolchain
