module github.com/gregwebs/agent-vm-images

go 1.27.0

toolchain go1.27.3

require (
	github.com/alexflint/go-arg v1.6.1
	github.com/failsafe-go/failsafe-go v0.9.8
	github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper v0.0.0
	github.com/ulikunitz/xz v0.5.17
	go.yaml.in/yaml/v3 v3.0.5
)

require (
	github.com/alexflint/go-scalar v1.2.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
)

// The rust-dev example layer's own module is compiled from the layer (what the
// image builds) and imported here (what `avm-pin rust-toolchain` stages), so one
// library owns the pin's rules. The copy in vendor/ is compared byte for byte
// against the layer by internal/vendordrift.
replace github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper => ./examples/layers/rust-dev/helper
