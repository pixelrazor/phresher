FROM golang:alpine

WORKDIR /app/phesher

COPY . .

RUN go install

ENTRYPOINT ["phesher"]
