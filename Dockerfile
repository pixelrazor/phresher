FROM golang:alpine

RUN apk add --no-cache git

WORKDIR /app/phesher

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

RUN go build -o ./phesher .

EXPOSE 8080

CMD ["./phesher"]