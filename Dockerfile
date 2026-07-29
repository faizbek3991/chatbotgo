# --- Build stage ---
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o server .

# --- Runtime stage ---
FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/server ./server
COPY templates ./templates
COPY static ./static

# Azure App Service for Containers needs to know which port the container
# listens on (set the WEBSITES_PORT app setting to match, if you change this).
EXPOSE 8080

ENTRYPOINT ["./server"]
