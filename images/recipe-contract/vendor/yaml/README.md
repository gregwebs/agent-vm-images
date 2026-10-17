# Build-only YAML distribution

Upstream: https://github.com/eemeli/yaml, npm release **yaml@2.9.0**, ISC.
Archive: https://registry.npmjs.org/yaml/-/yaml-2.9.0.tgz
SRI: `sha512-2AvhNX3mb8zd6Zy7INTtSpl1F15HW6Wnqj0srWlkKLcpYl/gMIMJiyuGq2KeI2YFxUPjdlB+3Lc10seMLtL4cA==`

The archive was fetched into invocation-owned scratch and its SHA-512 checked
before reading/extracting members. All members were checked for absolute paths,
traversal and nonregular/non-directory types. Retained upstream bytes are exactly
`LICENSE`, `package.json` and the Node distribution's `dist/**/*.js` (76 files).
`upstream.sha256` enumerates every retained filename and SHA-256. No npm install,
scripts, rebundling or modifications were used. CommonJS package semantics and
relative distribution hierarchy are preserved; the parser imports `dist/index.js`
explicitly. Browser bundles, types and CLI entrypoints are not needed.

License inspected: ISC permission and disclaimer retained verbatim in LICENSE.
Security review: GitHub public advisory API query
`https://api.github.com/advisories?ecosystem=npm&affects=yaml%402.9.0`
returned an empty list on 2026-10-16. This is an advisory lookup, not proof of
absence of vulnerabilities or independent publisher attestation.

`README.md` and `upstream.sha256` are project-authored metadata, not upstream
files. `node script/test/pnpm-vendor.mjs` checks complete set equality, digests
and hostile mutations. Update only after authenticating a reviewed exact archive,
reviewing license/advisories and reviewing every retained-file/digest change.
Do not lint/rewrite pristine sources or COPY this dependency into runtime images.
See [the parser ADR](../../../../docs/adr/0001-build-only-pnpm-graph-parser.md).
