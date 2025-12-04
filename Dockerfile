# FROM python:alpine3.22
FROM golang:1.25.5-alpine3.22 AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates

WORKDIR /build

COPY go.mod ./
COPY go.sum* ./

RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -a -installsuffix cgo \
    -ldflags='-w -s -extldflags "-static"' \
    -o ns1-dynamic-dns .


FROM alpine:3.22

# Install runtime dependencies
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

COPY *.sh /app
COPY --from=builder /build/ns1-dynamic-dns /app/ns1-dynamic-dns

# Configure DDNS check frequency in minutes
# Can be overriden in the compose.yml file
ARG FREQUENCY=5

COPY crontab /etc/crontabs/root
RUN "/app/prepare-crontab.sh"
CMD ["crond", "-f", "-l", "4"]
# Log levels for crond (the 4th parameter in the `CMD` above)
#LOG_EMERG   0   [* system is unusable *]
#LOG_ALERT   1   [* action must be taken immediately *]
#LOG_CRIT    2   [* critical conditions *]
#LOG_ERR     3   [* error conditions *]
#LOG_WARNING 4   [* warning conditions *]
#LOG_NOTICE  5   [* normal but significant condition *] the default
#LOG_INFO    6   [* informational *]
#LOG_DEBUG   7   [* debug-level messages *] same as -d option 
