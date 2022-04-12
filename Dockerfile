FROM golang AS builder
WORKDIR /usr/src/app
COPY go.mod go.sum .
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go install -v .
RUN mkdir /app && cp -R /go/bin/run-quay ./manifests/ ./pipeline/ ./create-pipeline.sh /app/

FROM quay.io/openshift/origin-cli:latest
WORKDIR /app
COPY --from=builder /app /app
ENTRYPOINT ["/app/run-quay"]
