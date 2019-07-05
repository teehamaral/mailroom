FROM golang:1.12

WORKDIR /app

COPY . .

RUN go build ./cmd/... && chmod +x mailroom

EXPOSE 8090
ENTRYPOINT ["./mailroom"]