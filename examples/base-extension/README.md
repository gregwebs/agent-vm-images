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

Copy the example into your own directory; from this repository root:

```bash
cp -R examples/base-extension "$HOME/my-base-extension"
# Edit hello-image: change the greeting to "hello from my edited extension".
# Edit FROM if you chose a different local base tag.
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
