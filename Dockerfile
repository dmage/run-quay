FROM golang AS builder
WORKDIR /usr/src/app
COPY go.mod go.sum .
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go install -ldflags="-X 'main.Commit=$(git rev-parse --short HEAD)'" -v .
RUN mkdir /app && cp -R /go/bin/run-quay /app/

FROM quay.io/openshift/origin-cli:latest
WORKDIR /app
COPY --from=builder /app /app
ENTRYPOINT ["/app/run-quay"]
