FROM golang:alpine

WORKDIR /app/phresher

COPY . .

RUN go install

ENTRYPOINT ["phresher"]
