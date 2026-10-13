#!/bin/sh
# Late-mutation payload appended after a tool verifier body: this changes the
# pnpm launcher, so the post-verifier identity gate must reject it. The launcher
# path is supplied as data through the environment, not embedded in test code.
printf '# late mutation\n' >> "${PNPM_MUTATION_TARGET}"
