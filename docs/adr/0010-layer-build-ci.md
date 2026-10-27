# ADR 0010: build every example layer on CI, selected by its own changes

Status: accepted ([#33](https://github.com/gregwebs/agent-vm-images/issues/33)).

## Context

The example layers under `examples/layers/` are built by hand or by the native
harnesses. Static contracts check their shape — the parent `ARG`, the pin
references, the guarded scripts — but nothing downloads the pinned inputs, so a
withdrawn Verus release, a rotated Go archive or a changed parent manifest is
discovered by whoever next builds the layer. Only `chrome-devtools` had a
workflow.

The layers are also not products: they are not published, tagged or covered by
the release contract. What they need is narrower than release coverage — every
context has to be *built* on the change that touches it, and an unrelated change
has to build nothing.

Two failure modes shaped the design:

- A path filter alone cannot narrow *within* a relevant event. One workflow per
  layer duplicates the harness and its bounds, and a hand-maintained list drifts
  from the directory it describes.
- A warm build cache hides the motivating failure. A cache key built from the
  release plus the parent digest stays valid after upstream withdraws the bytes,
  and the cached install step then proves nothing.

## Decision

One workflow (`.github/workflows/example-layers.yml`) with two levels of
selection, and cold builds on both native architectures.

**Level one, the workflow envelope.** `pull_request.paths` and `push.paths` are a
conservative superset of every build input — the contexts as
`examples/layers/*/**`, the harnesses, the shared watchdog and builder guard, the
parent inventory and source policy, and the workflow itself. An unrelated PR
starts no layer workflow at all. GitHub evaluates `on.*.paths` against only the
first 300 changed files, so a very large changeset can miss a layer file that the
selector would otherwise have selected; the selector's full diff cannot
compensate, and no engineering is spent on that here.

**Level two, the selector.** `script/ci/layer-changes.py` diffs the event's own
endpoints (`base...head` from the merge base for a PR, `before..after` for a push,
nothing for a dispatch) and returns a validated matrix. A layer's own directory
selects that layer; a shared build input selects all of them; the Chrome runtime
helpers select Chrome only; everything else selects nothing. Discovery is the
filesystem, so a new `examples/layers/<name>` with a `Dockerfile` produces rows
with no YAML edit, and a context that loses its recipe fails closed rather than
disappearing from the matrix.

**Every row builds cold.** `script/test/layer-build.sh` pulls the layer's
committed, digest-pinned released-standard parent on every invocation, asserts
the daemon, host and inspected parent platform, then builds the exact context
with `--no-cache --progress=plain` under an invocation-owned tag. The built image
is inspected for identity and platform, and only that one tag is ever removed.
`chrome-devtools` then keeps its existing runtime suite and its arm64
interrupted-run control; no other runtime claim is made, and the Rust and Go
suites stay manual maintenance evidence.

**Both native architectures by default.** `linux/amd64` and `linux/arm64` are the
two rows every layer gets. An architecture exception would need a reviewed policy
change here, not a per-layer YAML edit.

## Consequences

A withdrawn pin now fails a build that was triggered by an unrelated edit to the
same layer or by any shared input — the URL is printed by the installer and the
failure lands in the log. A pin withdrawal with no selected event is still only
found on the next relevant build or a manual dispatch; no scheduled monitor is
added.

Cold cost is real, so the workflow's step and job bounds are provisional floors,
not measurements: the first green run must record per-layer, per-architecture
durations and bytes, and the bounds then move to at least 1.5× those measured
maxima.

### "Cache both" was considered and declined

The issue suggested caching both the tool builds and the example builds. This
decision declines caching for the layer builds until the cold cost is measured,
because the cache key that would be available here is the release plus the
committed parent digest — exactly the key that stays warm when upstream withdraws
the bytes behind an unchanged digest. A cache would convert the motivating
failure into a silent reuse of an artifact no longer available. If measurements
show the cold cost is prohibitive, the options to review are a deliberately
time-bounded cache with an explicit bypass, or narrower path filtering — not a
cache keyed only on identity that can remain valid after a withdrawal.
