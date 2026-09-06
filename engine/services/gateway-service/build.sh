#!/bin/bash
# Build script for gateway-service
# Usage: ./build.sh [docker|local]

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}Building gateway-service...${NC}"

# Check if Go is available locally
if command -v go &> /dev/null; then
    echo -e "${YELLOW}Go found locally, building with Go...${NC}"
    go mod tidy
    go build -o gateway-service ./cmd/main.go
    echo -e "${GREEN}Build successful! Binary: ./gateway-service${NC}"
    exit 0
fi

# Check if Docker is available
if command -v docker &> /dev/null; then
    echo -e "${YELLOW}Go not found locally, building with Docker...${NC}"
    docker build -t gateway-service:latest .
    echo -e "${GREEN}Docker build successful!${NC}"
    echo -e "${GREEN}Run with: docker run -p 8080:8080 gateway-service:latest${NC}"
    exit 0
fi

echo -e "${RED}Error: Neither Go nor Docker found. Please install one of them.${NC}"
exit 1
