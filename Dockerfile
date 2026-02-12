# Stage 1: Build
FROM golang:1.23-alpine AS build

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY . .
RUN go build -o backend-server .

# Stage 2: Run
FROM alpine:latest

WORKDIR /app

COPY --from=build /app/backend-server .

EXPOSE 8080

CMD ["./backend-server"]
