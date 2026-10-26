//! A no_std library compiled for the selected musl target: it proves the target
//! standard library and compiler are present, without assuming linker names.
#![no_std]

pub fn selected_answer() -> u32 {
    42
}
