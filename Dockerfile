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
# ca-certificates for outbound HTTPS (push, OAuth, link previews),
# postgresql-client and tar for backups, tzdata for correct local time.
RUN apk add --no-cache ca-certificates tzdata postgresql-client tar
WORKDIR /app
COPY --from=build /out/hexeris /usr/local/bin/hexeris
# The front-end lives in web/. Its contents are copied into STATIC_DIR (/app),
# so the URLs stay /js/…, /css/app.css, /assets/… and the server serves them
# from /app.
COPY web/ ./
# Container defaults, overridable through compose or the environment. Uploads
# and backups live on the /data volume; TLS is terminated by the reverse proxy,
# so the app itself speaks plain HTTP.
ENV STATIC_DIR=/app \
    UPLOAD_DIR=/data/uploads \
    DB_BACKUP_DIR=/data/backups \
    TLS_MODE=http \
    LISTEN_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["hexeris"]
