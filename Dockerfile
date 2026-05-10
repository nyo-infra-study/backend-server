# Stage 1: Build
FROM golang:1.25-alpine AS build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go mod tidy
RUN go build -o backend-server .

# Stage 2: Run
FROM alpine:latest

WORKDIR /app

COPY --from=build /app/backend-server .

EXPOSE 9000

CMD ["./backend-server"]
