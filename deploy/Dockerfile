# Builds either binary from the one module: pass CMD=proxy or CMD=api.
#
# CGO is on because the API links mattn/go-sqlite3. That also decides the
# runtime image — the binary is dynamically linked against glibc, so it cannot
# run on scratch, and alpine would need a musl build instead.

FROM golang:1.26 AS build
ARG CMD

WORKDIR /src

# Copy the manifests first so dependency downloads stay cached when only
# source files change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o /out/app ./cmd/${CMD}

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/app /usr/local/bin/app

# Not root: neither binary needs privileged ports (3307 and 8080).
#
# /data is created here so the API's named volume inherits its ownership —
# Docker seeds an empty named volume from the image, and a root-owned mount
# point would leave the non-root process unable to write the SQLite file.
RUN useradd --system --uid 10001 auditproxy \
    && mkdir -p /data \
    && chown 10001:10001 /data
USER 10001

ENTRYPOINT ["/usr/local/bin/app"]
