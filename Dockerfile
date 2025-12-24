FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o crawler main.go


FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/crawler .
COPY --from=builder /app/.env .

ENTRYPOINT ["./crawler"]