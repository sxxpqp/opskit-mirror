FROM golang:1.26.2-alpine3.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go config.example.yaml ./
RUN CGO_ENABLED=0 go build -trimpath -o /mirror .
FROM alpine:3.23.3
RUN apk add --no-cache ca-certificates
COPY --from=build /mirror /usr/local/bin/opskit-mirror
USER 65532:65532
ENTRYPOINT ["opskit-mirror"]
CMD []
