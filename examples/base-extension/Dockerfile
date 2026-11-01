# The base's source identity is the hash of this repository's committed
# `images/base-inputs.sha256`; the generator rewrites the default below, so a
# copied extension keeps the default of the checkout it came from. Overriding it
# is an explicit acceptance of another base, not authentication of it.
ARG BASE_IMAGE=agent-vm-base:local
ARG AGENT_VM_BASE_IDENTITY=sha256:c081400447c0f7cbadd050ab709edd9136022f3aabd3741f6b79b041fbbab298
FROM ${BASE_IMAGE}
ARG AGENT_VM_BASE_IDENTITY
RUN --mount=type=bind,source=check-base-identity.sh,target=/tmp/check-base-identity.sh,ro \
    sh /tmp/check-base-identity.sh "$AGENT_VM_BASE_IDENTITY"
COPY --chmod=0755 hello-image /usr/local/bin/hello-image
CMD ["hello-image"]
