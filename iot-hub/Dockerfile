# ~15 MB image: static binary on distroless, dashboard embedded.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /iothub ./cmd/iothub && mkdir /data

FROM gcr.io/distroless/static:nonroot
COPY --from=build /iothub /iothub
# Pre-create /data owned by nonroot so a fresh volume is writable.
COPY --from=build --chown=65532:65532 /data /data
# Soft heap cap: the GC works harder instead of growing past it.
ENV GOMEMLIMIT=64MiB IOTHUB_DATA=/data/iothub.json IOTHUB_DB=/data/readings.db
VOLUME /data
EXPOSE 8080 1883 8883
USER nonroot
ENTRYPOINT ["/iothub"]
