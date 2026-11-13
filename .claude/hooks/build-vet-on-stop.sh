#!/usr/bin/env bash
# Stop: before Claude ends its turn, run `go build ./... && go vet ./...`.
# Silent on success. On failure the output is returned to Claude (exit 2) so it keeps working.
# If Claude is already continuing because of this hook, it only warns the user, to avoid a loop.
# Tests are deliberately not run here; they belong to the ticket's verification command.
set -u

input=$(cat)

cd "${CLAUDE_PROJECT_DIR:-.}" 2>/dev/null || exit 0
[ -f go.mod ] || exit 0 # module not initialised yet (before ticket T1.1)

go_bin=$(command -v go 2>/dev/null || true)
[ -n "$go_bin" ] || { [ -x /usr/local/go/bin/go ] && go_bin=/usr/local/go/bin/go; }
[ -n "$go_bin" ] || exit 0

# Nothing to check until the first Go file exists.
find . -name '*.go' -not -path './.git/*' -print -quit 2>/dev/null | grep -q . || exit 0

step=build
if out=$("$go_bin" build ./... 2>&1); then
  step=vet
  if out=$("$go_bin" vet ./... 2>&1); then
    exit 0
  fi
fi

summary=$(printf 'go %s failed:\n%s\n' "$step" "$(printf '%s\n' "$out" | head -60)")

already_retrying=false
if command -v jq >/dev/null 2>&1; then
  already_retrying=$(jq -r '.stop_hook_active // false' <<<"$input" 2>/dev/null || echo false)
fi

if [ "$already_retrying" = "true" ]; then
  if command -v jq >/dev/null 2>&1; then
    jq -n --arg m "Stop hook: the tree still does not pass go $step after a retry. $summary" '{systemMessage: $m}'
  fi
  exit 0
fi

{
  echo "$summary"
  echo "Fix this before finishing the turn."
} >&2
exit 2
