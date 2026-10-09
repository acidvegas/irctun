FROM golang:1.21-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o tunnel .

FROM alpine:latest
COPY --from=builder /build/tunnel /tunnel
EXPOSE 6667
CMD ["/tunnel"]
