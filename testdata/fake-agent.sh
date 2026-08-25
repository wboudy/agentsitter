#!/usr/bin/env bash
# A stand-in for an agent CLI showing a usage-limit prompt.
#
# It renders a two-entry menu, marks the selection with reverse video the way a
# real TUI does, moves on arrow keys, and prints a verdict on Enter. The
# integration test drives this through agentsitter to prove the whole path: read
# the pane, recognize the prompt, walk the cursor, confirm, submit.
#
# Usage: fake-agent.sh [initial-selection-index]
set -u

selected=${1:-0}
options=(
  "Yes, switch models"
  "No, keep current model"
)

render() {
  printf '\033[H\033[2J'
  printf "You've hit your usage limit for the current model.\n"
  printf 'Switch to another model now, or wait for the limit to reset.\n'
  printf '\n'
  local i
  for i in "${!options[@]}"; do
    if [ "$i" -eq "$selected" ]; then
      # Reverse video across the whole row, as a selected menu entry is drawn.
      printf '\033[7m❯ %s\033[0m\n' "${options[$i]}"
    else
      printf '  %s\n' "${options[$i]}"
    fi
  done
}

render
while IFS= read -rsn1 key; do
  case "$key" in
    $'\033')
      # An escape sequence: read the rest of the arrow key. The timeout is a
      # whole number because bash 3.2, still the system bash on macOS, rejects
      # a fractional one and would leave the bracket sequence unconsumed.
      read -rsn2 -t 1 rest || rest=""
      case "$rest" in
        '[A') [ "$selected" -gt 0 ] && selected=$((selected - 1)) ;;
        '[B') [ "$selected" -lt $((${#options[@]} - 1)) ] && selected=$((selected + 1)) ;;
      esac
      render
      ;;
    '')
      # Enter arrives as an empty read.
      printf '\033[H\033[2J'
      printf 'ANSWERED: %s\n' "${options[$selected]}"
      # Stay alive so the test can read the result out of the pane.
      sleep 300
      exit 0
      ;;
    [0-9])
      selected=$((key - 1))
      render
      ;;
  esac
done
