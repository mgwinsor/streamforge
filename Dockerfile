FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/api ./cmd/api && CGO_ENABLED=0 go build -o /out/worker ./cmd/worker
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates curl && rm -rf /var/lib/apt/lists/* && useradd -u 10001 -m streamforge && mkdir /data && chown streamforge /data
COPY --from=build /out/ /usr/local/bin/
USER streamforge
ENV DATA_DIR=/data
EXPOSE 8080
CMD ["api"]
