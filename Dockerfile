# syntax=docker/dockerfile:1
FROM golang:1.27.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backup-service ./cmd/backup-service

FROM debian:trixie-slim
# PGDG provides parallel-installable clients. Never copy just pg_dump from
# another image: PostgreSQL utilities also require their matching libraries.
# Ref: https://www.postgresql.org/download/linux/ubuntu/
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
    && install -d /usr/share/postgresql-common/pgdg \
    && curl --fail --show-error --silent https://www.postgresql.org/media/keys/ACCC4CF8.asc \
       -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
    && echo 'deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt trixie-pgdg main' > /etc/apt/sources.list.d/pgdg.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends postgresql-client-14 postgresql-client-16 postgresql-client-17 \
    && apt-get purge -y --auto-remove curl gnupg \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 backupsvc \
    && useradd --uid 10001 --gid 10001 --no-create-home --shell /usr/sbin/nologin backupsvc
COPY --from=build /out/backup-service /usr/local/bin/backup-service
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/backup-service"]
CMD ["--help"]
