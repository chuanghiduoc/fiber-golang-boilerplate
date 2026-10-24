#!/usr/bin/env bash
#
# Rename the Go module path throughout the entire project.
#
# Usage:
#   ./scripts/rename-module.sh github.com/yourname/yourproject
#
# The current path is read from go.mod, so the script can be run again after
# a rename. Every text file that contains the old path (Go sources, sqlc.yaml,
# Markdown, scripts, CI and deploy config) is rewritten in place. Inside a git
# checkout only tracked files are considered.

set -euo pipefail

NEW_MODULE="${1:-}"

if [ -z "$NEW_MODULE" ]; then
  echo "Usage: $0 <new-module-path>"
  echo "Example: $0 github.com/yourname/myapi"
  exit 1
fi

# Module paths are slash-separated and never contain whitespace or quotes.
if ! printf '%s' "$NEW_MODULE" | grep -Eq '^[A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)*$'; then
  echo "Invalid module path: $NEW_MODULE" >&2
  exit 1
fi

for tool in go perl; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "$tool is required but was not found in PATH" >&2
    exit 1
  fi
done

cd "$(dirname "$0")/.."

OLD_MODULE="$(go list -m)"

if [ "$OLD_MODULE" = "$NEW_MODULE" ]; then
  echo "New module path is the same as current. Nothing to do."
  exit 0
fi

echo "Renaming module: $OLD_MODULE -> $NEW_MODULE"

go mod edit -module "$NEW_MODULE"

# List text files (-I skips binaries) that mention the old path.
list_files() {
  if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    git ls-files -z | xargs -0 grep -lIF -- "$OLD_MODULE" || true
  else
    grep -rlIF --exclude-dir=.git --exclude-dir=vendor --exclude-dir=node_modules \
      -- "$OLD_MODULE" . || true
  fi
}

# perl -pi rewrites in place identically on Linux and macOS (unlike sed -i)
# and keeps line endings; \Q...\E matches the old path literally.
count=0
go_files=()
while IFS= read -r file; do
  OLD="$OLD_MODULE" NEW="$NEW_MODULE" perl -pi -e 's/\Q$ENV{OLD}\E/$ENV{NEW}/g' "$file"
  echo "  updated $file"
  count=$((count + 1))
  case "$file" in *.go) go_files+=("$file") ;; esac
done < <(list_files)
echo "Updated $count file(s)."

# Import paths may sort differently under the new prefix; format only the
# files this script rewrote.
if [ "${#go_files[@]}" -gt 0 ]; then
  echo "Formatting ${#go_files[@]} Go file(s)..."
  gofmt -w "${go_files[@]}"
fi

if ! go mod tidy; then
  echo "WARNING: 'go mod tidy' failed; fix the error above and run it manually." >&2
fi

if command -v swag >/dev/null 2>&1; then
  echo "Regenerating Swagger docs..."
  swag init -g cmd/api/main.go -o docs
else
  echo "swag not found; run 'make swagger' to regenerate the Swagger docs."
fi

echo ""
echo "Done! Module renamed to: $NEW_MODULE"
echo ""
echo "Next steps:"
echo "  1. Run 'go build ./...' and 'make check'"
echo "  2. Review the diff (README badges and links may now point at the new path)"
