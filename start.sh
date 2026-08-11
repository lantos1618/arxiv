#!/bin/sh
set -eu

load_secret() {
	name="$1"
	file_var="${name}_FILE"
	eval "secret_file=\${$file_var:-}"
	if [ -n "$secret_file" ]; then
		if [ ! -r "$secret_file" ]; then
			echo "$file_var is not readable: $secret_file" >&2
			exit 1
		fi
		value=$(cat "$secret_file")
		export "$name=$value"
	fi
}

load_secret DATABASE_URL
load_secret ADMIN_TOKEN
load_secret QWEN_WORKER_TOKEN
load_secret GOOGLE_CLIENT_SECRET

if [ -z "${DATABASE_URL:-}" ] || [ -z "${ADMIN_TOKEN:-}" ]; then
	echo "DATABASE_URL and ADMIN_TOKEN must be delivered through *_FILE secrets" >&2
	exit 1
fi

exec ./arxiv-server serve -port "${PORT:-8080}"
