ARG BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de
FROM ${BASE_IMAGE}

RUN apt-get update \
 && apt-get install -y --no-install-recommends chromium fonts-liberation sudo libnss3-tools \
 && rm -rf /var/lib/apt/lists/* \
 && ln -sf /usr/bin/chromium /usr/bin/google-chrome \
 && ln -sf /usr/bin/chromium /usr/bin/google-chrome-stable \
 && mkdir -p /opt/google/chrome \
 && ln -sf /usr/bin/chromium /opt/google/chrome/chrome \
 && if getent group chrome >/dev/null; then \
      test "$(getent group chrome | cut -d: -f3)" = 9999; \
    elif getent group 9999 >/dev/null; then \
      echo 'chrome group ID 9999 is already assigned' >&2; exit 1; \
    else \
      echo 'chrome:x:9999:' >> /etc/group; \
    fi \
 && test "$(getent group 9999 | cut -d: -f1)" = chrome \
 && if getent passwd chrome >/dev/null; then \
      test "$(getent passwd chrome | cut -d: -f3,4,6,7)" = '9999:9999:/home/chrome:/bin/bash'; \
    elif getent passwd 9999 >/dev/null; then \
      echo 'chrome user ID 9999 is already assigned' >&2; exit 1; \
    else \
      echo 'chrome:x:9999:9999:Chrome MCP:/home/chrome:/bin/bash' >> /etc/passwd; \
      echo 'chrome:!:19000:0:99999:7:::' >> /etc/shadow; \
    fi \
 && test "$(getent passwd 9999 | cut -d: -f1)" = chrome \
 && grep -q '^chrome:!:' /etc/shadow \
 && mkdir -p /home/chrome/.pki/nssdb \
 && if ! test -f /home/chrome/.pki/nssdb/cert9.db; then certutil -d sql:/home/chrome/.pki/nssdb -N --empty-password; fi \
 && chown -R 9999:9999 /home/chrome \
 && echo 'root ALL=(chrome) NOPASSWD: ALL' > /etc/sudoers.d/agent-vm-chrome \
 && chmod 0440 /etc/sudoers.d/agent-vm-chrome

COPY --chmod=0755 agent-vm-chrome-mcp /usr/local/bin/agent-vm-chrome-mcp
# The reviewed selection is installed here, at build time, from the committed
# inputs (#41). The helper verifies bytes and pins before it runs pnpm and
# fails the build on any deviation: first use never resolves or fetches.
COPY chrome-devtools-mcp/ /opt/agent-vm/chrome-devtools-mcp/
RUN --mount=type=bind,source=chrome-devtools-mcp.sh,target=/tmp/chrome-devtools-mcp.sh,ro \
    sh /tmp/chrome-devtools-mcp.sh install
RUN chromium --version \
 && test "$(readlink -f /usr/bin/google-chrome)" = /usr/bin/chromium \
 && test "$(readlink -f /usr/bin/google-chrome-stable)" = /usr/bin/chromium \
 && test "$(readlink -f /opt/google/chrome/chrome)" = /usr/bin/chromium \
 && command -v certutil \
 && test "$(getent passwd chrome | cut -d: -f3,4,6,7)" = '9999:9999:/home/chrome:/bin/bash' \
 && test "$(getent group chrome | cut -d: -f3)" = 9999 \
 && grep -q '^chrome:!:' /etc/shadow \
 && test "$(stat -c '%u:%g:%a' /home/chrome)" = 9999:9999:755 \
 && test "$(stat -c '%u:%g' /home/chrome/.pki/nssdb)" = 9999:9999 \
 && visudo -cf /etc/sudoers.d/agent-vm-chrome \
 && sudo -n -u chrome -- id -u | grep -qx 9999 \
 && sudo -u chrome -H -- test -w /home/chrome/.pki/nssdb \
 && test -x /usr/local/bin/agent-vm-chrome-mcp \
 && test "$(ls -A /home/chrome)" = .pki \
 && mkdir -p /etc/agent-vm-capabilities \
 && : > /etc/agent-vm-capabilities/chrome-devtools-mcp
