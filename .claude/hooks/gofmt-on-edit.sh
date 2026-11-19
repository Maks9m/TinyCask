#!/usr/bin/env bash
# PostToolUse (matcher: Edit|Write): run gofmt -w on the Go file that was just modified.
# Silent on success. A gofmt failure (syntax error) is reported back to Claude via exit 2.
set -u

cd "${CLAUDE_PROJECT_DIR:-.}" 2>/dev/null || exit 0
[ -f go.mod ] || exit 0 # module not initialised yet (before ticket T1.1)
command -v jq >/dev/null 2>&1 || exit 0

gofmt_bin=$(command -v gofmt 2>/dev/null || true)
[ -n "$gofmt_bin" ] || { [ -x /usr/local/go/bin/gofmt ] && gofmt_bin=/usr/local/go/bin/gofmt; }
[ -n "$gofmt_bin" ] || exit 0

file=$(jq -r '.tool_input.file_path // .tool_response.filePath // empty')
case "$file" in
  *.go) ;;
  *) exit 0 ;;
esac
[ -f "$file" ] || exit 0

if ! out=$("$gofmt_bin" -w "$file" 2>&1); then
  {
    echo "gofmt could not format $file (syntax error):"
    echo "$out" | head -20
  } >&2
  exit 2
fi
exit 0
