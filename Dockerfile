FROM golang:1.11

RUN wget -q -O /usr/bin/dep https://github.com/golang/dep/releases/download/v0.5.0/dep-linux-amd64
RUN chmod +x /usr/bin/dep

WORKDIR /go/src/github.com/flexoid/gitlab-mr-coverage

COPY Gopkg.toml Gopkg.lock ./
RUN dep ensure --vendor-only

COPY . ./
RUN go build

CMD ./gitlab-mr-coverage
