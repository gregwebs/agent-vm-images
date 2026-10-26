//! Independent native Verus oracle fixture: the harness runs this against the
//! image's Verus so the amd64 acceptance does not depend on the shipped smoke.
use vstd::prelude::*;

verus! {
proof fn independent_smoke()
    ensures true,
{
}
}
