FROM alpine:3.21.3@sha256:a8560b36e8b8210634f77d9f7f9efd7ffa463e380b75e2e74aff4511df3ef88c
RUN apk add --no-cache bash
RUN true
RUN true
COPY --chmod=0755 probe /usr/local/bin/release-probe
LABEL org.agent-vm.fixture=release-transport
ENV HOME=/tmp
USER 12345:23456
CMD ["bash", "-c", "release-probe"]
