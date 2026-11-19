#!/usr/bin/env bash
# PreToolUse guard (matcher: Edit|Write|MultiEdit|NotebookEdit|Bash).
#   - Edits to .env*, docs/ and ARCHITECTURE.md -> ask the user to confirm.
#   - rm -rf, git reset --hard -> blocked (exit 2).
#   - git commit, git push (any form) -> blocked (exit 2): the user commits and pushes themselves.
# Reads the hook payload as JSON on stdin. Needs jq.
set -u

if ! command -v jq >/dev/null 2>&1; then
  echo "pre-tool-guard: jq is not installed, so the guard cannot inspect this call. Install jq (brew install jq)." >&2
  exit 2
fi

input=$(cat)
tool=$(jq -r '.tool_name // empty' <<<"$input")
root="${CLAUDE_PROJECT_DIR:-$PWD}"

ask() {
  jq -n --arg reason "$1" '{
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "ask",
      permissionDecisionReason: $reason
    }
  }'
  exit 0
}

block() {
  echo "$1" >&2
  exit 2
}

guard_file() {
  local file rel
  file=$(jq -r '.tool_input.file_path // .tool_input.notebook_path // empty' <<<"$input")
  [ -n "$file" ] || exit 0

  case "$file" in
    "$root"/*) rel="${file#"$root"/}" ;;
    /*) exit 0 ;;          # outside the project
    *) rel="${file#./}" ;; # already relative
  esac

  case "$rel" in
    .env.example | */.env.example) exit 0 ;;
    .env | .env.* | */.env | */.env.*)
      ask "Protected file: $rel holds environment secrets. Approve only if you explicitly asked for this edit." ;;
    docs/*)
      ask "Protected file: $rel is the professor-approved design doc. Approve only if you explicitly asked for this edit." ;;
    ARCHITECTURE.md)
      ask "Protected file: ARCHITECTURE.md is the architecture source of truth. Approve only if you explicitly asked for this edit." ;;
  esac
  exit 0
}

# Strip shell punctuation that can wrap a word: quotes, backticks, $( and ( ).
bare_word() {
  local w="$1"
  w="${w#\$\(}"
  w="${w#[\"\'\`\(]}"
  w="${w%[\"\'\`\)]}"
  printf '%s' "$w"
}

# Print the git subcommand used in one simple command ("commit", "push", ...), or nothing.
# Skips global options, including the ones that take a value (-C <path>, -c <k=v>).
git_subcommand() {
  local -a toks
  local i n tok
  read -ra toks <<<"$1"
  n=${#toks[@]}
  for ((i = 0; i < n; i++)); do
    tok=$(bare_word "${toks[i]}")
    case "$tok" in git | */git) ;; *) continue ;; esac
    for ((i = i + 1; i < n; i++)); do
      tok=$(bare_word "${toks[i]}")
      case "$tok" in
        -C | -c | --git-dir | --work-tree | --namespace) i=$((i + 1)) ;;
        -*) ;;
        *) printf '%s' "$tok"; return ;;
      esac
    done
  done
}

guard_bash() {
  local cmd seg sub
  cmd=$(jq -r '.tool_input.command // empty' <<<"$input")
  [ -n "$cmd" ] || exit 0

  # Inspect each simple command separately (split on ; | & and newlines).
  while IFS= read -r seg; do
    [ -n "$seg" ] || continue

    # rm with both a recursive and a force flag, in any spelling (-rf, -fr, -r -f, --recursive --force).
    if grep -Eq '(^|[[:space:]`(])rm[[:space:]]' <<<"$seg" &&
      grep -Eq '[[:space:]](-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)([[:space:]]|$)' <<<"$seg" &&
      grep -Eq '[[:space:]](-[a-zA-Z]*f[a-zA-Z]*|--force)([[:space:]]|$)' <<<"$seg"; then
      block "Blocked by pre-tool-guard: recursive forced delete (rm -rf). Remove specific files without -f, or give the user the exact command to run themselves."
    fi

    sub=$(git_subcommand "$seg")
    case "$sub" in
      commit)
        block "Blocked by pre-tool-guard: the user commits themselves. Do not commit. Finish the work, then suggest a commit message in your final reply." ;;
      push)
        block "Blocked by pre-tool-guard: the user pushes themselves. Do not push (this includes --force). Tell the user the branch is ready to push." ;;
      reset)
        if grep -Eq '[[:space:]]--hard([[:space:]]|$)' <<<"$seg"; then
          block "Blocked by pre-tool-guard: git reset --hard discards uncommitted work. Use git stash or git restore <file>, or give the user the exact command to run themselves."
        fi ;;
    esac
  done < <(printf '%s\n' "$cmd" | tr ';|&' '\n\n\n')

  exit 0
}

case "$tool" in
  Edit | Write | MultiEdit | NotebookEdit) guard_file ;;
  Bash) guard_bash ;;
esac
exit 0
