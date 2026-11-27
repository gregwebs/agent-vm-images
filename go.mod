module github.com/gregwebs/agent-vm-images

go 1.27.0

toolchain go1.27.3

require (
	github.com/alexflint/go-arg v1.6.1
	github.com/failsafe-go/failsafe-go v0.9.8
	github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper v0.0.0
	github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper v0.0.0
	github.com/ulikunitz/xz v0.5.17
	go.yaml.in/yaml/v3 v3.0.5
)

require (
	github.com/alexflint/go-scalar v1.2.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
)

// Each example layer's own module is compiled from the layer (what the image builds)
// and imported here (what `avm-pin` stages), so one library owns the pin's rules
// instead of two copies of them. `go-dev` arrived with `avm-pin gopls` (#252) and
// `rust-dev` with `avm-pin rust-toolchain` (#232 slice 4a). The copies in vendor/ are
// compared byte for byte against the layers by internal/vendordrift.
replace github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper => ./examples/layers/go-dev/helper

replace github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper => ./examples/layers/rust-dev/helper
