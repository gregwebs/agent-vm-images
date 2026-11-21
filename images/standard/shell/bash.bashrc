
# agent-vm interactive defaults: the guest HOME is the caller's mount, so the
# settings every session of this image shares live here, under /etc, and a
# user-owned ~/.bashrc is still read after this file.
export SHELL=/bin/bash
PS1='\[\e[32m\]\w\[\e[0m\] \$ '

# History: append rather than overwrite (several panes share one history file).
shopt -s histappend checkwinsize
HISTSIZE=50000
HISTFILESIZE=50000
HISTCONTROL=ignoreboth:erasedups

# Full programmable completion (git, docker, ssh, ...) when the package is present.
# Without it bash still completes filenames and its own builtins.
if [ -f /usr/share/bash-completion/bash_completion ]; then
    . /usr/share/bash-completion/bash_completion
fi
