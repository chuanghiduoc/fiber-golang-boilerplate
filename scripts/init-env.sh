#!/usr/bin/env sh
# Create a local .env from .env.example and fill every required secret.
# The generated file is chmod 600 and is never committed. Run `make env` or
# `bash scripts/init-env.sh`; pass --force to replace an existing .env.
set -eu
umask 077

force=false
if [ "${1:-}" = "--force" ]; then
	force=true
fi

if [ -e .env ] && [ "$force" != true ]; then
	echo ".env already exists; refusing to overwrite it (use --force to replace)." >&2
	exit 1
fi

if ! command -v openssl >/dev/null 2>&1; then
	echo "openssl is required to generate local secrets." >&2
	exit 1
fi

cp .env.example .env
chmod 600 .env

generate() {
	openssl rand -base64 "$1" | tr -d '\n'
}

set_value() {
	key=$1
	value=$2
	tmp=$(mktemp)
	awk -v k="$key" -v v="$value" '
		BEGIN { FS = "="; OFS = "=" }
		$1 == k { print k "=" v; replaced = 1; next }
		{ print }
		END { if (!replaced) print k "=" v }
	' .env >"$tmp"
	mv "$tmp" .env
}

for key in \
	DB_PASSWORD \
	DB_BYPASS_PASSWORD \
	DB_MIGRATE_PASSWORD \
	JWT_SECRET \
	DATA_ENCRYPTION_KEY \
	WEBHOOK_SECRET_ENCRYPTION_KEY \
	OAUTH_STATE_KEY \
	STORAGE_S3_SECRET_KEY; do
	set_value "$key" "$(generate 32)"
done

admin_email=$(awk -F= '$1 == "ADMIN_EMAIL" { sub(/^[^=]*=/, ""); print; exit }' .env)
if [ -z "$admin_email" ]; then
	admin_email=admin@example.com
	set_value ADMIN_EMAIL "$admin_email"
fi
admin_password=$(generate 24)
set_value ADMIN_PASSWORD "$admin_password"
chmod 600 .env

printf 'Wrote .env with generated secrets.\nAdmin email: %s (password written to .env)\n' "$admin_email"
