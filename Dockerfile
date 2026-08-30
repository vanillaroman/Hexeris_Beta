# ─────────────────────────── build stage ───────────────────────────
FROM golang:1.25-alpine AS build
WORKDIR /src
# Cache dependencies separately from sources.
COPY go.mod go.sum ./
RUN go mod download
COPY server ./server
# Static build (CGO off — lib/pq is pure Go) for a small image.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hexeris ./server

# ─────────────────────────── runtime stage ─────────────────────────
FROM alpine:3.20
# ca-certificates for outbound HTTPS (push, OAuth, link previews), tar for the
# file backups, tzdata for correct local time.
#
# The Postgres client is pinned to a MAJOR version on purpose. The unversioned
# `postgresql-client` follows whatever the base image happens to carry, and the
# day Alpine moves to 17 while the stack still runs `postgres:16-alpine`,
# pg_dump refuses to work at all ("server version mismatch") — backups stop, and
# a floating dependency is the worst thing to discover at that moment. Bump this
# together with the database image, deliberately.
#
# The fallback is not decoration: the versioned package name is what changes
# between Alpine releases, and a base-image bump should degrade to the
# unversioned client rather than break the build outright. If the fallback is
# what ran, `hexeris backup` will say so the first time the versions disagree.
RUN apk add --no-cache ca-certificates tzdata tar && \
    (apk add --no-cache postgresql16-client || apk add --no-cache postgresql-client)

# The application runs as a non-root user: a messenger is reachable from the
# internet, and a container process there has no business being root.
RUN addgroup -S hexeris && adduser -S -G hexeris -h /app hexeris

WORKDIR /app
COPY --from=build /out/hexeris /usr/local/bin/hexeris
# The front-end lives in web/. Its contents are copied into STATIC_DIR (/app),
# so the URLs stay /js/…, /css/app.css, /assets/… and the server serves them
# from /app.
COPY web/ ./

# These directories must exist in the IMAGE, not only in the volume. The server
# checks UPLOAD_DIR at start-up and exits if it is missing — deliberately, so a
# mistyped path is named once instead of turning into a 404 on every picture.
# A freshly created named volume, however, starts out empty, so without this
# line the very first `docker compose up` ends in a crash loop on a clean
# machine. Docker seeds a new named volume from the image's content, which is
# exactly what makes creating them here work.
#
# A bind mount is NOT seeded that way: if you mount a host directory at /data,
# create `uploads` and `backups` inside it yourself and give this user access.
RUN mkdir -p /data/uploads /data/backups && chown -R hexeris:hexeris /data /app

# Container defaults, overridable through compose or the environment. Uploads
# and backups live on the /data volume; TLS is terminated by the reverse proxy,
# so the app itself speaks plain HTTP.
ENV STATIC_DIR=/app \
    UPLOAD_DIR=/data/uploads \
    DB_BACKUP_DIR=/data/backups \
    TLS_MODE=http \
    LISTEN_ADDR=:8080

USER hexeris
EXPOSE 8080

# `docker compose ps` should say whether the application actually answers, not
# merely that a process is alive. /healthz pings the database, so a stuck pool
# shows up here rather than in a user's complaint.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["hexeris"]
