# shellcheck shell=sh
# Load the single active project's environment in new shells.
# Removal does not unset variables already inherited by an existing process.
if [ -r "$HOME/.config/lima-debian-ctl/environment.sh" ]; then
    # shellcheck disable=SC1091
    . "$HOME/.config/lima-debian-ctl/environment.sh"
fi

# Point SSH at Lima's forwarded host agent when it is available, so git and
# ssh use host keys without copying any private key onto the guest disk.
# Guarded so a missing socket (agent forwarding off) leaves SSH_AUTH_SOCK unset.
if [ -S /run/host-services/ssh-auth.sock ]; then
    export SSH_AUTH_SOCK=/run/host-services/ssh-auth.sock
fi
