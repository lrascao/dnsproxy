ARG GO_VERSION=1.24.4
FROM golang:${GO_VERSION} AS build

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o dnsproxy .

FROM alpine:3.21
COPY --from=build /app/dnsproxy /usr/local/bin/dnsproxy
COPY config.yaml /etc/dnsproxy/config.yaml

EXPOSE 53/udp
EXPOSE 5353/tcp

ENTRYPOINT ["dnsproxy", "-config", "/etc/dnsproxy/config.yaml"]
