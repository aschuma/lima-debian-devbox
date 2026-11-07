# shellcheck shell=sh
# Load the single active project's environment in new shells.
# Removal does not unset variables already inherited by an existing process.
if [ -r "$HOME/.config/lima-debian-ctl/environment.sh" ]; then
    # shellcheck disable=SC1091
    . "$HOME/.config/lima-debian-ctl/environment.sh"
fi
