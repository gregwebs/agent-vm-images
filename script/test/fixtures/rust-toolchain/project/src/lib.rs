//! Zero-dependency sample: it proves the preinstalled toolchain builds, tests,
//! lints and formats without any registry access or toolchain download.
pub fn answer() -> u32 {
    42
}

#[cfg(test)]
mod tests {
    #[test]
    fn answer_is_stable() {
        assert_eq!(super::answer(), 42);
    }
}
