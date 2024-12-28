# Upstash Redis Proxy

> ⚠️ **Note**: This project is currently a work in progress. Features and documentation may be incomplete or subject to change.

A Go-based HTTP proxy that forwards Upstash Redis REST API requests to a local Redis instance.


**It works for basic Redis commands i havent tested everything so far**

## Features

- Supports Upstash Redis REST API commands
- Authentication via Bearer token
- JSON response format
- Support for transactions and pipelining
- Base64 encoded responses (optional)

## Getting Started

1. Install dependencies:
```bash
go mod tidy
```

2. Run the proxy:
```bash
go run main.go
```

By default, the proxy will:
- Listen on port 8080
- Connect to Redis at localhost:6379
- Require authentication via Bearer token

## API Usage

The proxy follows the same API conventions as Upstash Redis REST API. Send HTTP requests with:

- Command in URL path: `/set/key/value`
- Authentication header: `Authorization: Bearer <token>`
- Optional base64 encoding: `Upstash-Encoding: base64`

Example:
```bash
curl http://localhost:8080/set/foo/bar \
  -H "Authorization: Bearer your-token-here"
```
