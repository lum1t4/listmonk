#!/usr/bin/env bash
# Disposable stock-v5 installation, upgrade, and full HTTP/SMTP integration test.
set -euo pipefail
cd "$(dirname "$0")/.."
image="${LISTMONK_TEST_IMAGE:-listmonk-fork:local}"
prefix="listmonk-e2e-$$"
task_tmp="$(mktemp -d)"
cleanup() {
  docker rm -f "$prefix-app" "$prefix-db" "$prefix-mail" >/dev/null 2>&1 || true
  docker network rm "$prefix" >/dev/null 2>&1 || true
  rm -rf "$task_tmp"
}
trap cleanup EXIT
umask 077
cat > "$task_tmp/config.toml" <<CONFIG
[app]
address = "0.0.0.0:9000"
[db]
host = "$prefix-db"
port = 5432
user = "listmonk"
password = "test-only"
database = "listmonk"
ssl_mode = "disable"
max_open = 25
max_idle = 25
max_lifetime = "300s"
CONFIG
docker network create "$prefix" >/dev/null
docker run -d --name "$prefix-db" --network "$prefix" -e POSTGRES_USER=listmonk -e POSTGRES_PASSWORD=test-only -e POSTGRES_DB=listmonk postgres:17-alpine >/dev/null
docker run -d --name "$prefix-mail" --network "$prefix" -p 127.0.0.1::8025 axllent/mailpit:v1.27 >/dev/null
for i in {1..60}; do
  if docker exec "$prefix-db" pg_isready -U listmonk >/dev/null 2>&1; then break; fi
  sleep 1
done
docker run --rm --network "$prefix" -v "$task_tmp/config.toml:/listmonk/config.toml:ro" \
  -e LISTMONK_ADMIN_USER=test-admin -e LISTMONK_ADMIN_PASSWORD=local-test-password \
  -e LISTMONK_ADMIN_API_USER=integration-test listmonk/listmonk:v5.0.0 \
  ./listmonk --install --yes > "$task_tmp/install.log" 2>&1
# Populate a fixture before migration, then verify data survived the upgrade.
cat <<'SQL' | docker exec -i "$prefix-db" psql -v ON_ERROR_STOP=1 -U listmonk >/dev/null
INSERT INTO subscribers(uuid,email,name,attribs) VALUES(gen_random_uuid(),'preserved@example.test','Preserved contact','{"source":"fixture"}');
UPDATE settings SET value='"http://localhost:19000"' WHERE key='app.root_url';
UPDATE settings SET value='"Example <hello@example.test>"' WHERE key='app.from_email';
UPDATE settings SET value='false' WHERE key='app.check_updates';
UPDATE settings SET value='[]' WHERE key='app.notify_emails';
SQL
printf "UPDATE settings SET value='[{\"enabled\":true,\"host\":\"%s-mail\",\"port\":1025,\"auth_protocol\":\"none\",\"tls_type\":\"none\",\"max_conns\":2,\"idle_timeout\":\"15s\",\"wait_timeout\":\"5s\",\"max_msg_retries\":2,\"email_headers\":{}}]' WHERE key='smtp';\n" "$prefix" | docker exec -i "$prefix-db" psql -v ON_ERROR_STOP=1 -U listmonk >/dev/null
docker run --rm --network "$prefix" -v "$task_tmp/config.toml:/listmonk/config.toml:ro" "$image" ./listmonk --upgrade --yes > "$task_tmp/upgrade.log" 2>&1
docker run --rm --network "$prefix" -v "$task_tmp/config.toml:/listmonk/config.toml:ro" "$image" ./listmonk --upgrade --yes >> "$task_tmp/upgrade.log" 2>&1
count="$(docker exec "$prefix-db" psql -U listmonk -Atc "SELECT count(*) FROM subscribers WHERE email='preserved@example.test' AND attribs->>'source'='fixture'")"
test "$count" = 1
app_port="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"
docker run -d --name "$prefix-app" --network "$prefix" -v "$task_tmp/config.toml:/listmonk/config.toml:ro" -p "127.0.0.1:$app_port:9000" "$image" ./listmonk >/dev/null
app_port="$(docker port "$prefix-app" 9000/tcp | cut -d: -f2)"
mail_port="$(docker port "$prefix-mail" 8025/tcp | cut -d: -f2)"
# Correct the root URL and reload before testing native generated links.
printf "UPDATE settings SET value='\"http://localhost:%s\"' WHERE key='app.root_url';\n" "$app_port" | docker exec -i "$prefix-db" psql -v ON_ERROR_STOP=1 -U listmonk >/dev/null
docker restart "$prefix-app" >/dev/null
for i in {1..60}; do
  if curl --max-time 3 -fsS "http://localhost:$app_port/health" >/dev/null 2>&1; then break; fi
  sleep 1
done
export LISTMONK_TEST_URL="http://localhost:$app_port" MAILPIT_TEST_URL="http://localhost:$mail_port"
LISTMONK_TEST_TOKEN="$(sed -n 's/.*export LISTMONK_ADMIN_API_TOKEN="\([^"]*\)".*/\1/p' "$task_tmp/install.log")"; export LISTMONK_TEST_TOKEN
python3 tests/automation_e2e.py
printf 'PASS: v5 upgrade preserves existing contact data; repeating upgrade is idempotent\n'
# A fresh fork installation must create the same feature schema without upgrades.
docker exec "$prefix-db" createdb -U listmonk fresh_install
docker run --rm --network "$prefix" -v "$task_tmp/config.toml:/listmonk/config.toml:ro" \
  -e LISTMONK_db__database=fresh_install -e LISTMONK_ADMIN_USER=fresh-admin \
  -e LISTMONK_ADMIN_PASSWORD=local-test-password "$image" ./listmonk --install --yes > "$task_tmp/fresh.log" 2>&1
fresh_count="$(docker exec "$prefix-db" psql -U listmonk -d fresh_install -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('automations','automation_jobs','automation_events','automation_attempts','list_confirmation_templates')")"
test "$fresh_count" = 5
printf 'PASS: fresh installation includes all automation tables\n'
