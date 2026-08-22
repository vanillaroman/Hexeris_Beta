# Deployment (Docker)

Two **independent** configurations:

- `docker-compose.yml` — development (localhost, HTTP, no TLS, domain or
  certificates).
- `docker-compose.prod.yml` — production (HTTPS, your domain, automatic
  certificate renewal, coturn).

The database schema is created by the application; there are no migrations to
apply.

---

## Development, in one command

Docker with the Compose plugin is the only requirement.

```bash
docker compose up -d --build
```

Open **http://localhost**. Registration, sign-in, messages and files all work;
the secrets are fixed development values (unsafe outside localhost), and the
database and files live in volumes.

> **Port 80 taken?** Usually by a system web server. Stop it, or run the
> development stack elsewhere: `HTTP_PORT=8080 docker compose up -d` →
> http://localhost:8080.

Stop with `docker compose down` (data is kept). Wipe data with
`docker compose down -v`. Logs: `docker compose logs -f app`.

---

## Production over HTTPS

**Requirements:** a host, an A record for `APP_DOMAIN` pointing at it, and open
ports **80 and 443/tcp**, **3478 tcp+udp**, **5349/tcp**,
**49152-65535/udp**. Any system web server must be stopped so ports 80 and 443
are free.

**1. Configuration**

```bash
cp .env.example .env      # fill in APP_DOMAIN and the secrets (generator commands are in the file)
```

**2. Certificate (once, with port 80 free)**

```bash
export APP_DOMAIN=chat.example.com
docker run --rm -p 80:80 -v /etc/letsencrypt:/etc/letsencrypt \
  certbot/certbot certonly --standalone -d "$APP_DOMAIN" --agree-tos -m admin@"$APP_DOMAIN" -n
```

**3. coturn** — in `deploy/coturn/turnserver.conf` replace `__DOMAIN__`,
`__EXTERNAL_IP__` (the host's public address) and `__SECRET__` (the
`TURN_SECRET` from `.env`).

**4. Start**

```bash
docker compose -f docker-compose.prod.yml up -d --build
curl -fsS https://$APP_DOMAIN/healthz && echo " OK"
```

Certificate renewal is automatic (the `certbot` service, webroot challenge).
In a browser, verify sign-in, a message, a file and a call (which exercises the
TURN relay), and check an iPhone with Safari separately.

**Updating the code:**
`git pull && docker compose -f docker-compose.prod.yml up -d --build`.
Data in the volumes survives a rebuild.

---

## Backups

In `.env`, set `DB_BACKUP_ENABLED=true`; the sets (`hexeris-*.sql.gz` and
`hexeris-files-*.tar.gz`) are written to the `appdata` volume at
`/data/backups`. A one-off run:

```bash
docker compose -f docker-compose.prod.yml exec app hexeris backup
```

Details and off-site copies are in [BACKUP.md](BACKUP.md). **Keep
`SERVER_ENC_KEY` separately — without it a backup is useless.**

### Turning backups off when disk space runs short

```bash
# 1. In .env
DB_BACKUP_ENABLED=false

# 2. Restart the application
docker compose -f docker-compose.prod.yml up -d app

# 3. Confirm the scheduler is silent: no "DB backup scheduler ENABLED" line
docker compose -f docker-compose.prod.yml logs --tail=50 app | grep -i backup
```

⚠️ The flag stops **future** runs; it does not delete what already exists.
Space is freed by removing the files:

```bash
du -sh /data/backups                  # how much is used
ls -lh /data/backups | tail           # what is there
rm /data/backups/hexeris-*.sql.gz     # delete deliberately: these are your copies
```

If backups are needed but space is tight, lower `DB_BACKUP_KEEP` or raise
`DB_BACKUP_INTERVAL_HOURS` instead of switching them off. The admin panel shows
`backups disabled (DB_BACKUP_ENABLED)` when the scheduler is off, so the choice
is visible rather than looking like a silent failure.

## A basic restore drill

Restore the latest dump into a throwaway database, time it, count rows and drop
it. For the development stack, omit `-f docker-compose.prod.yml`.

```bash
C="docker compose -f docker-compose.prod.yml"
$C exec app sh -lc 'ls -1t /data/backups/hexeris-*.sql.gz | head -1'   # a set must exist
$C exec db psql -U hexeris -d postgres -c 'DROP DATABASE IF EXISTS restore_test; CREATE DATABASE restore_test;'
time $C exec -T app sh -lc 'gunzip -c $(ls -1t /data/backups/hexeris-*.sql.gz | head -1)' \
  | $C exec -T db psql -U hexeris -d restore_test
$C exec db psql -U hexeris -d restore_test -c 'SELECT count(*) FROM users; SELECT count(*) FROM messages;'
$C exec db psql -U hexeris -d postgres -c 'DROP DATABASE restore_test;'
```

Non-zero counts mean the dump is valid, and the elapsed time is your **database
RTO**. `scripts/restore-drill.sh` automates the same thing and additionally
proves the encryption key matches the backup.

## Reliability drill (chaos)

Confirm that restarting the application under traffic **loses no messages** and
that `/healthz` reflects the database state (needs `pip install aiohttp`):

```bash
python3 scripts/chaos-drill.py                 # development (http://localhost)
# production:
python3 scripts/chaos-drill.py \
  --compose "docker compose -f docker-compose.prod.yml" --server https://$APP_DOMAIN
```

Expect "✅ NO LOSS" and healthz 200→503→200.

## The admin panel on its own host

The panel is a static file (`docs/admin-panel/admin-index.html`) uploaded to an
operations host by hand; its backend lives in this repository (the `/admin/*`
endpoints in `server/admin.go` and `server/loginaudit.go`). The panel calls the
**relative** path `/admin-api/...` and knows neither the messenger's address
nor `ADMIN_KEY` — the reverse proxy injects the key server-side.

A ready configuration with placeholders is
**`docs/admin-panel/nginx-admin-panel.conf`**. Without its `/admin-api/`
location every request from the panel hits the static root and gets a 404,
which the panel reports as "Cannot reach the API" even though the endpoint is
alive.

```bash
sudo cp docs/admin-panel/nginx-admin-panel.conf /etc/nginx/sites-available/hexeris-ops
sudo ln -s /etc/nginx/sites-available/hexeris-ops /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
```

What the messenger side needs:

| Variable | Value | Why |
|---|---|---|
| `ADMIN_KEY` | the same value the proxy sends as `X-Admin-Key` | a second boundary beyond the proxy |
| `ADMIN_ALLOWED_IPS` | the operations host's public address | the endpoints stay closed from anywhere else |

⚠️ The proxy **must not** forward a browser's `X-Forwarded-For`: the messenger
reads the client address from that header, so a hand-crafted one would bypass
`ADMIN_ALLOWED_IPS`. The supplied configuration clears it explicitly.

⚠️ If the messenger lives behind a name whose address can change, a static
`proxy_pass` resolves it once at startup and keeps a stale address until
restarted. The supplied configuration puts the host in a variable and sets a
`resolver`.

`ADMIN_ORIGIN` is unnecessary in this arrangement: the panel and `/admin-api/`
share an origin, so CORS is not involved. It is only needed if the panel ever
calls the messenger directly.

## Every sign-in shows 127.0.0.1 in the log

The application sits behind a reverse proxy, so the TCP connection comes from
the proxy rather than the client. Only the proxy knows the real address and it
must pass it on. Without that header the log records the proxy's address —
which looks like a working log while making it impossible to investigate an
incident or spot brute force from one address.

In the `location` blocks **of the server running the messenger** (not the
operations host):

```nginx
proxy_set_header Host              $host;
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;
```

There are usually two blocks, `location /` and `location /ws`; both need the
headers, because `proxy_set_header` is not inherited into a location that
declares one of its own. A ready version is in
`deploy/nginx/default.conf.template`.

```bash
nginx -t && systemctl reload nginx
```

The application trusts these headers **only** from addresses in
`TRUSTED_PROXY_IPS` (loopback is always trusted). If the proxy runs on another
machine, add its address there — without that check any client could forge an
`X-Forwarded-For` and bypass both the rate limits and `ADMIN_ALLOWED_IPS`.

To verify, sign in from a phone and look at the **Sign-in Log**: while the
address is internal the panel flags it and shows a warning above the list.

## Notes

- Security headers (CSP and the rest) are set by the application; the
  messenger's proxy does not duplicate them. The operations host is the
  exception: there the proxy serves its own static files and sets its own
  headers. Note that any `add_header` inside a `location` **replaces** every
  server-level `add_header` rather than adding to them.
- `client_max_body_size 64m` is already set, above the application's own 60 MB
  limit.
- Running the Go binary directly under an init system, without Docker, remains
  supported.
