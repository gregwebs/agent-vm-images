# Agent VM Images

Shared language for the image sources and their package-installation defaults.

## Language

**Agent**:
A coding-agent CLI the standard image installs and certifies under the recipe contract. The agent-vm launcher calls the same thing a tool.
_Avoid_: Tool (launcher vocabulary), optional extra

**Default install path**:
The recommended route for installing Node packages in an agent-vm image, intended to delay adoption of newly published packages. It is a preference, not a restriction on alternate package managers.
_Avoid_: Enforced package manager, guaranteed-safe installation

**Image-owned install**:
A Node package installation performed by this repository's image build or setup code, rather than by a user or their project.
_Avoid_: User install, project install

**Pinned image input**:
An external input to an image-owned build or setup operation whose selection and expected content identity are fixed in reviewed repository data. A version selection alone is not a content pin; neither pinning nor integrity verification establishes that the content is safe.
_Avoid_: Version-only pin, guaranteed-safe dependency

**Floating distro input**:
An image-build package input from an official Debian apt repository that may change between builds. It is the explicit exception to image-input pinning, not a category that includes third-party apt repositories.
_Avoid_: Pinned OS package, any apt package

**Network request**:
A TLS destination (one exact hostname, optionally a port) that software in the finished image contacts at runtime in ordinary use under its default configuration. The image declares it so the launcher can report when a launch's egress does not cover it; it never grants egress. Hosts a build downloads from are not network requests, and neither are hosts that depend on the user's project.
_Avoid_: Allow list, allowance, required host, build egress

**Image request**:
A network request that applies to every launch of an image, whichever agent runs. Each component that adds software to the image (the base, a tool example) declares its own.
_Avoid_: Layer allowance, global host

**Agent request**:
A network request that applies only when one named agent is launched from the image. A single-vendor agent requests its vendor's service hosts. A multi-provider agent requests only what it contacts before the user picks a provider; hosts reached only through a provider choice or a link to another agent (such as the Pi Claude bridge) are the user's decision, not requests.
_Avoid_: Tool allowance, provider allow list

**Recipe contract**:
The agreement between an agent's install recipe and the image build: how an install is run, recorded and checked.
_Avoid_: Helpers, contract scripts

**Build helper**:
A program the image build runs during a build step that leaves no trace in the finished image.
_Avoid_: Image tool, installed helper

**Runtime payload**:
Files this repository deliberately places in the finished image for the guest to use.
_Avoid_: Build helper, image tool
