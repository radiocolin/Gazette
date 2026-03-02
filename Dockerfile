FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o gazette-bridge .

FROM alpine:latest

WORKDIR /app

# Create data directory
RUN mkdir -p /app/data

COPY --from=builder /app/gazette-bridge .
COPY config.yaml .

EXPOSE 8080

CMD ["./gazette-bridge"]
