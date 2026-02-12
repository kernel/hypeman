#!/bin/bash
# E2E Benchmark: Pre-built erofs optimization
# Submits a build and measures time from submission to "ready" status.
# Run with: ./scripts/e2e-erofs-benchmark.sh [label]
set -e

LABEL="${1:-benchmark}"
API_URL="${API_URL:-http://localhost:8084}"
TOKEN="${TOKEN}"

if [ -z "$TOKEN" ]; then
    cd "$(dirname "$0")/.."
    TOKEN=$(./bin/godotenv -f .env go run ./cmd/gen-jwt -user-id e2e-bench 2>/dev/null | tail -1)
fi

echo "=== E2E Build Benchmark: $LABEL ==="
echo "API: $API_URL"

# Check API is up
if ! curl -s "$API_URL/health" | grep -q "ok"; then
    echo "ERROR: API not reachable at $API_URL"
    exit 1
fi
echo "API server is running"

# Create test source with a unique layer to avoid digest deduplication
UNIQUE_ID="$(date +%s%N)-$$"
TEST_DIR=$(mktemp -d)
cat > "$TEST_DIR/Dockerfile" << DOCKERFILE
FROM node:20-alpine
WORKDIR /app
COPY package.json index.js ./
RUN echo "build-id: $UNIQUE_ID" > /app/.build-id
CMD ["node", "index.js"]
DOCKERFILE

cat > "$TEST_DIR/package.json" << 'EOF'
{"name":"bench-app","version":"1.0.0","main":"index.js"}
EOF

cat > "$TEST_DIR/index.js" << 'EOF'
console.log("Benchmark app running at", new Date().toISOString());
EOF

TARBALL=$(mktemp --suffix=.tar.gz)
tar -czf "$TARBALL" -C "$TEST_DIR" .
rm -rf "$TEST_DIR"

DOCKERFILE_CONTENT=$(tar -xzf "$TARBALL" -O ./Dockerfile 2>/dev/null)

# Submit build
echo ""
echo "Submitting build..."
SUBMIT_TS=$(date +%s%N)

RESPONSE=$(curl -s -X POST "$API_URL/builds" \
    -H "Authorization: Bearer $TOKEN" \
    -F "source=@$TARBALL" \
    -F "dockerfile=$DOCKERFILE_CONTENT" \
    -F "cache_scope=e2e-bench")

BUILD_ID=$(echo "$RESPONSE" | jq -r '.id // empty')
if [ -z "$BUILD_ID" ]; then
    echo "ERROR: Failed to submit build"
    echo "$RESPONSE" | jq .
    rm -f "$TARBALL"
    exit 1
fi
echo "Build ID: $BUILD_ID"

# Poll for completion
echo "Polling for completion..."
LAST_STATUS=""
while true; do
    RESPONSE=$(curl -s "$API_URL/builds/$BUILD_ID" -H "Authorization: Bearer $TOKEN")
    STATUS=$(echo "$RESPONSE" | jq -r '.status')

    if [ "$STATUS" != "$LAST_STATUS" ]; then
        TS=$(date +%s%N)
        ELAPSED_MS=$(( (TS - SUBMIT_TS) / 1000000 ))
        echo "  [${ELAPSED_MS}ms] Status: $STATUS"
        LAST_STATUS="$STATUS"
    fi

    case "$STATUS" in
        "ready")
            READY_TS=$(date +%s%N)
            break
            ;;
        "failed")
            echo "ERROR: Build failed!"
            echo "$RESPONSE" | jq .
            echo ""
            echo "=== Build Logs ==="
            curl -s "$API_URL/builds/$BUILD_ID/events" -H "Authorization: Bearer $TOKEN" | jq -r '.[] | select(.type=="log") | .content' 2>/dev/null || \
            curl -s "$API_URL/builds/$BUILD_ID/logs" -H "Authorization: Bearer $TOKEN"
            rm -f "$TARBALL"
            exit 1
            ;;
        "cancelled")
            echo "Build cancelled!"
            rm -f "$TARBALL"
            exit 1
            ;;
    esac
    sleep 0.5
done

# Calculate timing
TOTAL_MS=$(( (READY_TS - SUBMIT_TS) / 1000000 ))
DURATION_MS=$(echo "$RESPONSE" | jq -r '.duration_ms // "unknown"')
IMAGE_DIGEST=$(echo "$RESPONSE" | jq -r '.image_digest // "none"')

echo ""
echo "=== RESULTS: $LABEL ==="
echo "Build ID:        $BUILD_ID"
echo "Image digest:    $IMAGE_DIGEST"
echo "Agent duration:  ${DURATION_MS}ms (build inside VM)"
echo "Total duration:  ${TOTAL_MS}ms (submit to ready)"
echo "Post-build wait: $(( TOTAL_MS - ${DURATION_MS:-0} ))ms (image conversion)"
echo ""

# Check server logs for erofs path indicators
echo "=== Server Log Indicators ==="
echo "(Looking for pre-built erofs indicators in the build response)"
HAS_EROFS=$(echo "$RESPONSE" | jq -r '.erofs_disk_path // empty')
if [ -n "$HAS_EROFS" ]; then
    echo "Pre-built erofs: YES ($HAS_EROFS)"
else
    echo "Pre-built erofs: NO (used fallback pipeline)"
fi

rm -f "$TARBALL"
echo ""
echo "=== Benchmark Complete ==="
