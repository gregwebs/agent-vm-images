# Your own base extension

This is an ordinary user-owned Dockerfile, not a launcher layer. Build the
[base first](../../README.md#clone-and-build), then from the repository root:

```bash
BUILDER="$(docker context show)"
docker buildx inspect "$BUILDER"       # require Driver: docker
docker build --builder "$BUILDER" --platform linux/arm64 \
  -t my-agent-vm:local examples/base-extension
docker run --rm --user 12345:23456 --cap-drop ALL --network none \
  -e HOME=/tmp my-agent-vm:local
# hello from my base extension
# Linux
# 12345
```

Use `linux/amd64` on x86-64 Linux. No generated account or fixed image USER is
needed. The Bash script is readable/executable in `/usr/local/bin`, outside
HOME where mounts could hide it. Native executables must match the Linux image
architecture; a macOS binary does not become a Linux executable through COPY.

The extension also carries this checkout's **base source identity**. The
`AGENT_VM_BASE_IDENTITY` default is generated from
[`images/base-inputs.sha256`](../../images/base-inputs.sha256) (the honest
`sha256:` of the committed base-input manifest), and `check-base-identity.sh`
compares it with the stamp the base wrote at
`/opt/agent-vm/base/source-identity`. A base built from other sources fails the
build instead of being extended silently. That is source identity, not
image-byte identity: two builds of the same sources may still differ, and a
forged stamp is not prevented. To extend a base you deliberately built from a
different tree, accept it explicitly:

```bash
docker build --builder "$BUILDER" --platform linux/arm64 \
  --build-arg AGENT_VM_BASE_IDENTITY="$(docker run --rm --entrypoint cat \
    your-base:tag /opt/agent-vm/base/source-identity)" \
  -t my-agent-vm:local "$HOME/my-base-extension"
```

Changing a file the base consumes is a reviewed source change, not a rebuild:
edit it (adding a new one means `git add`), run
`bash script/build/base-inputs.sh --write`, review the manifest and this default,
then rebuild the base before the extension. `bash script/build/base-inputs.sh --check`
names what drifted. See
[repository base identity](../../docs/adr/0005-repository-base-identity.md).

Copy the example into your own directory; from this repository root:

```bash
cp -R examples/base-extension "$HOME/my-base-extension"
# Edit hello-image: change the greeting to "hello from my edited extension".
# Edit the BASE_IMAGE default if you chose a different local base tag. Keep the
# AGENT_VM_BASE_IDENTITY default unless that tag was built from a *different*
# checkout: a base built from this checkout's sources needs no override, and
# copying a stamp into the default only makes the check agree with itself.
docker build --builder "$BUILDER" --platform linux/arm64 \
  -t my-edited-agent-vm:local "$HOME/my-base-extension"
docker run --rm --user 12345:23456 --cap-drop ALL --network none \
  -e HOME=/tmp my-edited-agent-vm:local
# hello from my edited extension
# Linux
# 12345
```

Add ordinary apt/RUN/COPY instructions, or edit the standard recipe if you want
its agents. A failed rebuild must be treated as failure even though Docker
leaves an old successful tag runnable. No agent-vm installation is needed for
these demonstrations.

Docker runs this image's CMD. A downstream microsandbox runtime can supply its
own agentd/PID1 and override the command; this example does not emulate that
entrypoint. Actual build/import/guest boot integration is downstream work and
is not verified by the container demonstration. Bash must remain available for
that downstream shell integration.

Builder/registry TLS, image RUN-layer trust, and language/runtime TLS stores are
independent; adding a CA to one does not fix all others. See the base's explicit
[trust option](../../README.md#optional-build-trust).

The base has an empty seed-hook directory. Standard's hooks are optional shipped
integrations for user-state merging and the Pi bridge; Docker CMD does not run
them automatically. Unrelated custom images need not implement any seed hook,
agent membership, marker or universal boot contract.
