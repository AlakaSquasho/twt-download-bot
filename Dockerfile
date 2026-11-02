FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bot .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/bot /usr/local/bin/bot
WORKDIR /state
ENTRYPOINT ["/usr/local/bin/bot"]
