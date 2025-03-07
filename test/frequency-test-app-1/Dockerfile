FROM golang:1.23-alpine AS builder

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY . .

RUN go build -o resource-consumer

FROM debian:bullseye-slim

WORKDIR /root/

COPY --from=builder /app/resource-consumer ./

CMD ["./resource-consumer"]