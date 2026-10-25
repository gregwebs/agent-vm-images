# Build-CA fixture

`host-ca.pem` is a synthetic, self-signed public certificate used by
`script/test/install-build-ca.sh` to exercise the real
`images/install-build-ca.sh` hash and PEM structure checks. The private key was
generated in a scratch directory and never committed; nothing here is trusted by
any image or host.
