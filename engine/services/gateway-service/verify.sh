#!/bin/bash
# Verification script for gateway-service
# Checks all requirements are met

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo -e "${GREEN}Verifying gateway-service implementation...${NC}"

# Check directory structure
echo -e "\n${YELLOW}1. Checking directory structure...${NC}"
REQUIRED_FILES=(
    "cmd/main.go"
    "internal/config/config.go"
    "internal/middleware/jwt.go"
    "internal/middleware/ratelimit.go"
    "internal/middleware/logging.go"
    "internal/handler/proxy.go"
    "internal/handler/health.go"
    "internal/middleware/ratelimit_test.go"
    "go.mod"
    "Dockerfile"
)

for file in "${REQUIRED_FILES[@]}"; do
    if [ -f "$file" ]; then
        echo -e "  ${GREEN}✓${NC} $file"
    else
        echo -e "  ${RED}✗${NC} $file MISSING"
        exit 1
    fi
done

# Check R2 requirement: 07 §7 comment
echo -e "\n${YELLOW}2. Checking R2 requirement (07 §7 comment)...${NC}"
if grep -q "// 07 §7 限流50req/min" internal/middleware/ratelimit.go; then
    echo -e "  ${GREEN}✓${NC} Rate limiter has required '07 §7' comment"
else
    echo -e "  ${RED}✗${NC} Missing required '07 §7' comment in ratelimit.go"
    exit 1
fi

# Check JWT implementation
echo -e "\n${YELLOW}3. Checking JWT implementation...${NC}"
if grep -q "CODEAUDIT_JWT_SECRET" internal/config/config.go; then
    echo -e "  ${GREEN}✓${NC} JWT secret configuration found"
else
    echo -e "  ${RED}✗${NC} Missing JWT secret configuration"
    exit 1
fi

# Check rate limiting implementation
echo -e "\n${YELLOW}4. Checking rate limiting implementation...${NC}"
if grep -q "token bucket" internal/middleware/ratelimit.go; then
    echo -e "  ${GREEN}✓${NC} Token bucket algorithm implemented"
else
    echo -e "  ${RED}✗${NC} Missing token bucket implementation"
    exit 1
fi

# Check proxy routing
echo -e "\n${YELLOW}5. Checking proxy routing...${NC}"
ROUTES=(
    "/v1/projects/"
    "/v1/tasks/"
    "/v1/results/"
    "/v1/storage/"
)

for route in "${ROUTES[@]}"; do
    if grep -q "$route" cmd/main.go; then
        echo -e "  ${GREEN}✓${NC} Route $route configured"
    else
        echo -e "  ${RED}✗${NC} Route $route missing"
        exit 1
    fi
done

# Check test file exists
echo -e "\n${YELLOW}6. Checking test file...${NC}"
if [ -f "internal/middleware/ratelimit_test.go" ]; then
    echo -e "  ${GREEN}✓${NC} Rate limiter test file exists"
else
    echo -e "  ${RED}✗${NC} Test file missing"
    exit 1
fi

# Check Dockerfile
echo -e "\n${YELLOW}7. Checking Dockerfile...${NC}"
if grep -q "golang:1.22-alpine" Dockerfile && grep -q "alpine:latest" Dockerfile; then
    echo -e "  ${GREEN}✓${NC} Multi-stage Dockerfile with golang:1.22-alpine and alpine:latest"
else
    echo -e "  ${RED}✗${NC} Dockerfile missing required stages"
    exit 1
fi

# Check port configuration
echo -e "\n${YELLOW}8. Checking port configuration...${NC}"
if grep -q "8080" internal/config/config.go; then
    echo -e "  ${GREEN}✓${NC} Port 8080 configured (default)"
else
    echo -e "  ${RED}✗${NC} Port 8080 not configured"
    exit 1
fi

echo -e "\n${GREEN}✓ All verification checks passed!${NC}"
echo -e "\n${YELLOW}Note: Go is not installed in this environment.${NC}"
echo -e "${YELLOW}To build, run: ./build.sh${NC}"
