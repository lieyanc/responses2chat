FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/responses2chat ./cmd/responses2chat

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && mkdir -p /data && chown nobody:nobody /data
COPY --from=build /out/responses2chat /usr/local/bin/responses2chat
USER nobody
WORKDIR /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/responses2chat"]
