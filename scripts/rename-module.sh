#!/usr/bin/env bash
#
# Rename the Go module path throughout the entire project.
#
# Usage:
#   ./scripts/rename-module.sh github.com/yourname/yourproject
#

set -euo pipefail

OLD_MODULE="github.com/chuanghiduoc/fiber-golang-boilerplate"
NEW_MODULE="${1:-}"

if [ -z "$NEW_MODULE" ]; then
  echo "Usage: $0 <new-module-path>"
  echo "Example: $0 github.com/yourname/myapi"
  exit 1
fi

if [ "$OLD_MODULE" = "$NEW_MODULE" ]; then
  echo "New module path is the same as current. Nothing to do."
  exit 0
fi

echo "Renaming module: $OLD_MODULE → $NEW_MODULE"

# Replace in go.mod
sed -i "s|module $OLD_MODULE|module $NEW_MODULE|g" go.mod

# Replace in all Go files
find . -name '*.go' -not -path './vendor/*' -exec sed -i "s|\"$OLD_MODULE/|\"$NEW_MODULE/|g" {} +

# Replace in sqlc.yaml if it references the module
if [ -f sqlc.yaml ]; then
  sed -i "s|$OLD_MODULE|$NEW_MODULE|g" sqlc.yaml
fi

# Regenerate Swagger docs if swag is available
if command -v swag &> /dev/null; then
  echo "Regenerating Swagger docs..."
  swag init -g cmd/api/main.go -o docs
fi

echo ""
echo "Done! Module renamed to: $NEW_MODULE"
echo ""
echo "Next steps:"
echo "  1. Run 'go mod tidy' to verify"
echo "  2. Run 'go build ./...' to check"
echo "  3. Update this script's OLD_MODULE if you plan to rename again"
