#!/bin/bash
# The token drift check (docs/70 K10). Every colour in Theme.swift is written
# exactly as its source writes it, and the comment after it names that source:
#
#   static let bg = Color(css: "#0a0b0d") // global.css --bg
#   static let s3 = Color(css: "#1d2029") // main.slint s3
#
# This reads each such line, looks the name up in gawk-app's global.css or
# in main.slint's `global T`, and fails on any value that differs, character
# for character, or on a colour with no source named. CI runs it in ios.yml.
#
#   gawk-ios/scripts/check-theme.sh [Theme.swift [global.css [main.slint]]]
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
theme="${1:-$root/gawk-ios/app/Gawk/Design/Theme.swift}"
css="${2:-$root/gawk-app/src/styles/global.css}"
slint="${3:-$root/gawk-broadcast-desktop/crates/ui/main.slint}"

token='^[[:space:]]*static let ([A-Za-z0-9]+) = Color\(css: "([^"]+)"\) // (global\.css|main\.slint) ([A-Za-z0-9-]+)[[:space:]]*$'

# The value a source gives `key`, as written, without the trailing `;`.
source_value() {
  case "$1" in
    global.css)
      sed -nE "s/^[[:space:]]*$2:[[:space:]]*([^;]*);.*$/\1/p" "$css" | head -1
      ;;
    main.slint)
      # Only `global T { … }`: other globals reuse the property names.
      awk '/^global T \{/ { on = 1; next } on && /^\}/ { exit } on' "$slint" |
        sed -nE "s/^[[:space:]]*out property <color> $2:[[:space:]]*([^;]*);.*$/\1/p" | head -1
      ;;
  esac
}

checked=0
failed=0
while IFS= read -r line; do
  if [[ $line =~ $token ]]; then
    name="${BASH_REMATCH[1]}" value="${BASH_REMATCH[2]}"
    src="${BASH_REMATCH[3]}" key="${BASH_REMATCH[4]}"
    want="$(source_value "$src" "$key")"
    if [ -z "$want" ]; then
      echo "Theme.$name: $src has no $key" >&2
      failed=1
    elif [ "$value" != "$want" ]; then
      echo "Theme.$name is \"$value\", but $src $key is \"$want\"" >&2
      failed=1
    fi
    checked=$((checked + 1))
  else
    echo "a colour with no source named (\`// global.css --name\` or \`// main.slint name\`):" >&2
    echo "  $line" >&2
    failed=1
  fi
done < <(grep -E 'Color\(css: "' "$theme")

if [ "$checked" -eq 0 ]; then
  echo "no tokens found in $theme" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "Theme.swift has drifted from the tokens it copies (docs/70 §3.1)." >&2
  exit 1
fi
echo "Theme.swift: $checked tokens match global.css and main.slint"
