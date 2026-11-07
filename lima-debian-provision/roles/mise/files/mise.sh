# mise — tool version manager (https://mise.jdx.dev)
# Sourced from /etc/profile (bash login shells; zsh login shells via /etc/zsh/zprofile).
#   zsh:  shims only — interactive zsh gets the full activation from ~/.zshrc
#   bash: interactive → full activation (PATH + env hooks); non-interactive → shims
if command -v mise >/dev/null 2>&1; then
  if [ -n "$ZSH_VERSION" ]; then
    eval "$(mise activate zsh --shims)"
  else
    case $- in
      *i*) eval "$(mise activate bash)" ;;
      *)   eval "$(mise activate bash --shims)" ;;
    esac
  fi
fi
