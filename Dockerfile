# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e

FROM golang:1.27-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS builder
ARG VERSION=dev
ARG COMMIT=unknown
ADD --checksum=sha256:5f4c81d34ad406538c2871ad72dbfceb5d9b412b2f16cbbeb4d712d270846667 \
    https://cpan.metacpan.org/authors/id/E/EX/EXIFTOOL/Image-ExifTool-13.55.tar.gz /tmp/exiftool.tgz
RUN mkdir -p /opt/exiftool \
    && tar -xzf /tmp/exiftool.tgz --strip-components=1 -C /opt/exiftool \
       Image-ExifTool-13.55/exiftool Image-ExifTool-13.55/lib Image-ExifTool-13.55/README

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/exiftool-server ./cmd/exiftool-server

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends curl perl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/exiftool-server /usr/local/bin/exiftool-server
COPY --chmod=0555 healthcheck.sh /usr/local/bin/healthcheck
COPY --from=builder /opt/exiftool /opt/exiftool
WORKDIR /opt
ENV ADDR=:8080 \
    EXIFTOOL_PATH=/opt/exiftool/exiftool
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 \
  CMD ["healthcheck"]
ENTRYPOINT ["/usr/local/bin/exiftool-server"]
