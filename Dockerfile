# syntax=docker/dockerfile:1

FROM golang:1.27-trixie AS builder
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

FROM debian:trixie-slim
RUN apt-get update \
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
