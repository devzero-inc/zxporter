FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY . .

RUN go build -o decrease-load

FROM debian:bullseye-slim

WORKDIR /root/

COPY --from=builder /app/decrease-load ./

EXPOSE 9001

CMD ["./decrease-load"]