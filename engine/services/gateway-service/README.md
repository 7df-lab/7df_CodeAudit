# Gateway Service

API Gateway for CodeAudit system - handles JWT authentication, request routing to backend gRPC services, and rate limiting.

## Features

- **JWT Authentication**: HS256 token validation with `x-user-id` and `x-user-role` header injection
- **Rate Limiting**: Token bucket algorithm, 50 requests per minute per IP (依据: 07 §7)
- **Request Routing**: REST `/v1/*` → internal gRPC service mapping
- **Health Check**: `/health` endpoint for service monitoring

## Architecture

```
Client Request
    │
    ▼
┌─────────────────────────────────────────┐
│          Gateway Service (:8080)        │
│                                         │
│  1. Logging Middleware                  │
│  2. Rate Limiting (50 req/min/IP)      │
│  3. JWT Authentication                  │
│  4. Request Routing                     │
└─────────────────────────────────────────┘
    │
    ├─→ /v1/projects/* → project-service:50052
    ├─→ /v1/tasks/*    → task-service:50053
    ├─→ /v1/results/*  → result-service:50054
    └─→ /v1/storage/*  → storage-service:50055
```

## Configuration

Environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `CODEAUDIT_JWT_SECRET` | (required) | JWT signing secret |
| `CODEAUDIT_PROJECT_SERVICE_ADDR` | `project-service:50052` | Project service address |
| `CODEAUDIT_TASK_SERVICE_ADDR` | `task-service:50053` | Task service address |
| `CODEAUDIT_RESULT_SERVICE_ADDR` | `result-service:50054` | Result service address |
| `CODEAUDIT_STORAGE_SERVICE_ADDR` | `storage-service:50055` | Storage service address |
| `CODEAUDIT_GATEWAY_PORT` | `8080` | Gateway listen port |

## API Endpoints

### Health Check
```
GET /health
Response: {"status": "ok", "service": "gateway-service"}
```

### Project Service Routes
```
POST   /v1/projects
GET    /v1/projects/{id}
PUT    /v1/projects/{id}
DELETE /v1/projects/{id}
GET    /v1/projects
```

### Task Service Routes
```
POST   /v1/tasks
GET    /v1/tasks/{id}
PUT    /v1/tasks/{id}
DELETE /v1/tasks/{id}
GET    /v1/tasks
```

### Result Service Routes
```
GET    /v1/results/{id}
GET    /v1/results
```

### Storage Service Routes
```
POST   /v1/storage
GET    /v1/storage/{id}
GET    /v1/storage
```

## Authentication

All `/v1/*` endpoints require JWT authentication:

```bash
# Example request with JWT token
curl -X GET http://localhost:8080/v1/projects \
  -H "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
```

The gateway will:
1. Validate the JWT token signature
2. Check token expiration (access tokens: 30min, refresh tokens: 7d)
3. Extract `user_id` and `role` from claims
4. Forward `x-user-id` and `x-user-role` headers to backend services

## Rate Limiting

- **Limit**: 50 requests per minute per IP address
- **Algorithm**: Token bucket
- **Response**: HTTP 429 with `Retry-After: 60` header when exceeded
- **IP Detection**: X-Forwarded-For header or RemoteAddr

## Building

### Local Build (requires Go 1.22+)
```bash
./build.sh local
```

### Docker Build
```bash
./build.sh docker
```

## Running

### Local
```bash
export CODEAUDIT_JWT_SECRET="your-secret-key"
./gateway-service
```

### Docker
```bash
docker run -p 8080:8080 \
  -e CODEAUDIT_JWT_SECRET="your-secret-key" \
  gateway-service:latest
```

## Testing

### Unit Tests
```bash
go test ./internal/middleware/... -v
```

### Rate Limiter Tests
```bash
go test ./internal/middleware/ratelimit_test.go -v
```

## Dependencies

- [github.com/golang-jwt/jwt/v5](https://github.com/golang-jwt/jwt) - JWT authentication
- [google.golang.org/grpc](https://pkg.go.dev/google.golang.org/grpc) - gRPC support (for future direct gRPC routing)

## Design References

- **03 §1.1**: Gateway capabilities (routing, auth, rate limiting)
- **03 §4**: JWT HS256 authentication (access 30min / refresh 7d)
- **07 §7**: Rate limiting specification (50 req/min)
- **ADR-113**: Port 8080 assignment
- **01 §4**: Service architecture (9 services)

## Notes

- The gateway is HTTP-only (not gRPC server) - it proxies HTTP REST to internal gRPC services
- Backend services should validate the `x-user-id` and `x-user-role` headers
- Health endpoint does not require authentication
- Rate limiting is per-IP, not per-user (for simplicity in v1.0)
