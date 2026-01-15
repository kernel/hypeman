#!/bin/bash
# E2E Build System Test
# Usage: ./scripts/e2e-build-test.sh [--skip-run]
#
# Prerequisites:
#   - API server running (make dev)
#   - Generic builder image imported into Hypeman registry
#   - .env file configured
#
# Options:
#   --skip-run    Skip running the built image (only test build)
#
# Environment variables:
#   API_URL       - API endpoint (default: http://localhost:8083)
#   BUILDER_IMAGE - Builder image to check (default: hypeman/builder:latest)

set -e

# Configuration
API_URL="${API_URL:-http://localhost:8083}"
TIMEOUT_POLLS=60
POLL_INTERVAL=5
SKIP_RUN=false

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --skip-run)
            SKIP_RUN=true
            shift
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log() { echo -e "${GREEN}[INFO]${NC} $1" >&2; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1" >&2; }
error() { echo -e "${RED}[ERROR]${NC} $1" >&2; }

# Check prerequisites
check_prerequisites() {
    log "Checking prerequisites..."
    
    # Check if API is reachable
    if ! curl -s "$API_URL/health" | grep -q "ok"; then
        error "API server not reachable at $API_URL"
        error "Start the server with: make dev"
        exit 1
    fi
    log "✓ API server is running"

    # Check if generic builder image exists
    BUILDER_IMAGE="${BUILDER_IMAGE:-hypeman/builder:latest}"
    if ! docker images "$BUILDER_IMAGE" --format "{{.Repository}}" | grep -q .; then
        warn "Builder image not found locally"
        warn "Build it with: docker build -t hypeman/builder:latest -f lib/builds/images/generic/Dockerfile ."
    else
        log "✓ Builder image available locally"
    fi
}

# Generate JWT token
generate_token() {
    cd "$(dirname "$0")/.."
    
    # Try using make gen-jwt
    if command -v make &> /dev/null && [ -f Makefile ]; then
        TOKEN=$(make gen-jwt 2>/dev/null | tail -1)
        if [ -n "$TOKEN" ] && [ "$TOKEN" != "make:" ]; then
            echo "$TOKEN"
            return
        fi
    fi
    
    # Fallback: run directly
    if [ -f ./bin/godotenv ]; then
        TOKEN=$(./bin/godotenv -f .env go run ./cmd/gen-jwt -user-id e2e-test 2>/dev/null | tail -1)
        echo "$TOKEN"
        return
    fi
    
    echo ""
}

# Create test source with Dockerfile
# The generic builder requires a Dockerfile to be provided
create_test_source() {
    TEST_DIR=$(mktemp -d)
    
    # Application code
    cat > "$TEST_DIR/package.json" << 'EOF'
{
  "name": "e2e-test-app",
  "version": "1.0.0",
  "main": "index.js",
  "scripts": {
    "start": "node index.js"
  },
  "dependencies": {}
}
EOF
    
    cat > "$TEST_DIR/index.js" << 'EOF'
console.log("E2E Build Test - Success!");
console.log("Built at:", new Date().toISOString());
EOF

    # Dockerfile is REQUIRED for the generic builder
    # Users control their runtime version here
    cat > "$TEST_DIR/Dockerfile" << 'EOF'
FROM node:20-alpine
WORKDIR /app
COPY package.json index.js ./
CMD ["node", "index.js"]
EOF

    # Create tarball
    TARBALL=$(mktemp --suffix=.tar.gz)
    tar -czvf "$TARBALL" -C "$TEST_DIR" . > /dev/null 2>&1
    
    rm -rf "$TEST_DIR"
    echo "$TARBALL"
}

# Submit build
submit_build() {
    local token="$1"
    local source="$2"
    
    log "Submitting build..."
    
    # Extract Dockerfile from source tarball
    DOCKERFILE_CONTENT=$(tar -xzf "$source" -O ./Dockerfile 2>/dev/null || echo "")
    
    if [ -n "$DOCKERFILE_CONTENT" ]; then
        # Dockerfile found in source - pass it explicitly for reliability
        RESPONSE=$(curl -s -X POST "$API_URL/builds" \
            -H "Authorization: Bearer $token" \
            -F "source=@$source" \
            -F "dockerfile=$DOCKERFILE_CONTENT" \
            -F "cache_scope=e2e-test" \
            -F "timeout_seconds=300")
    else
        # No Dockerfile in source - will fail if not provided
        error "No Dockerfile found in source tarball"
        exit 1
    fi
    
    BUILD_ID=$(echo "$RESPONSE" | jq -r '.id // empty')
    
    if [ -z "$BUILD_ID" ]; then
        error "Failed to submit build"
        echo "$RESPONSE" | jq .
        exit 1
    fi
    
    log "Build submitted: $BUILD_ID"
    echo "$BUILD_ID"
}

# Get build events/logs
get_logs() {
    local token="$1"
    local build_id="$2"
    
    log "Fetching build events..."
    curl -s "$API_URL/builds/$build_id/events" \
        -H "Authorization: Bearer $token"
}

# Import an image into Hypeman's image store
import_image() {
    local token="$1"
    local image_ref="$2"
    
    log "Importing image into Hypeman..."
    log "  Image: $image_ref"
    
    # Request image import
    RESPONSE=$(curl -s -X POST "$API_URL/images" \
        -H "Authorization: Bearer $token" \
        -H "Content-Type: application/json" \
        -d "{\"name\": \"$image_ref\"}")
    
    IMAGE_NAME=$(echo "$RESPONSE" | jq -r '.name // empty')
    IMAGE_STATUS=$(echo "$RESPONSE" | jq -r '.status // empty')
    
    if [ -z "$IMAGE_NAME" ]; then
        error "Failed to import image"
        echo "$RESPONSE" | jq .
        return 1
    fi
    
    log "Image import started: $IMAGE_NAME (status: $IMAGE_STATUS)"
    
    # Extract the build ID for filtering (last part of the path before the tag)
    # e.g., "10.102.0.1:8083/builds/abc123:latest" -> "abc123"
    BUILD_ID=$(echo "$IMAGE_NAME" | sed -E 's|.*/([^/:]+)(:[^/]*)?$|\1|')
    
    # Wait for image to be ready
    # The API may normalize image names (e.g., 10.102.0.1:8083/builds/xxx -> docker.io/builds/xxx)
    # So we need to check for both the original name and the normalized version
    log "Waiting for image conversion..."
    for i in $(seq 1 60); do
        # Query the list endpoint and filter by build ID (works regardless of registry prefix)
        RESPONSE=$(curl -s "$API_URL/images" \
            -H "Authorization: Bearer $token" | \
            jq --arg buildid "$BUILD_ID" '[.[] | select(.name | contains($buildid))] | .[0] // empty')
        
        if [ -z "$RESPONSE" ] || [ "$RESPONSE" = "null" ]; then
            echo -ne "\r  Waiting for image... (poll $i/60)..." >&2
            sleep 2
            continue
        fi
        
        STATUS=$(echo "$RESPONSE" | jq -r '.status // empty')
        IMAGE_ERROR=$(echo "$RESPONSE" | jq -r '.error // empty')
        FOUND_NAME=$(echo "$RESPONSE" | jq -r '.name // empty')
        
        case "$STATUS" in
            "ready")
                echo "" >&2  # Clear the progress line
                log "✓ Image is ready: $FOUND_NAME"
                # Export the actual image name for use in instance creation (to stdout)
                echo "$FOUND_NAME"
                return 0
                ;;
            "failed")
                echo "" >&2  # Clear the progress line
                error "Image import failed: $IMAGE_ERROR"
                if echo "$IMAGE_ERROR" | grep -q "mediatype"; then
                    error "  Hint: The builder may be pushing Docker-format images instead of OCI format."
                    error "  Ensure the builder image has been updated with oci-mediatypes=true"
                fi
                return 1
                ;;
            "pending"|"pulling"|"converting")
                echo -ne "\r  Status: $STATUS (poll $i/60)..." >&2
                ;;
            *)
                warn "Unknown status: $STATUS"
                ;;
        esac
        
        sleep 2
    done
    echo ""
    
    error "Image import timed out"
    return 1
}

# Create and run an instance from the built image
run_built_image() {
    local token="$1"
    local image_ref="$2"
    
    log "Running built image as VM..."
    log "  Image: $image_ref"
    
    # First, import the image into Hypeman's image store
    IMPORTED_NAME=$(import_image "$token" "$image_ref")
    if [ $? -ne 0 ]; then
        error "Failed to import image"
        error ""
        error "  This typically happens when the builder outputs Docker-format images"
        error "  instead of OCI format. The builder agent needs oci-mediatypes=true"
        error "  in the BuildKit output configuration."
        error ""
        error "  To fix: rebuild the builder image and deploy it:"
        error "    make build-builder"
        error "    docker push <your-registry>/builder:latest"
        return 1
    fi
    
    # Use the imported image name (may differ from the original reference)
    if [ -n "$IMPORTED_NAME" ]; then
        log "Using imported image: $IMPORTED_NAME"
        image_ref="$IMPORTED_NAME"
    fi
    
    # Create instance
    log "Creating instance..."
    RESPONSE=$(curl -s -X POST "$API_URL/instances" \
        -H "Authorization: Bearer $token" \
        -H "Content-Type: application/json" \
        -d "{
            \"image\": \"$image_ref\",
            \"name\": \"e2e-test-instance\",
            \"vcpus\": 1,
            \"memory\": \"256M\"
        }")
    
    INSTANCE_ID=$(echo "$RESPONSE" | jq -r '.id // empty')
    
    if [ -z "$INSTANCE_ID" ]; then
        error "Failed to create instance"
        echo "$RESPONSE" | jq .
        return 1
    fi
    
    log "Instance created: $INSTANCE_ID"
    
    # Wait for instance to be running
    log "Waiting for instance to start..."
    for i in $(seq 1 30); do
        RESPONSE=$(curl -s "$API_URL/instances/$INSTANCE_ID" \
            -H "Authorization: Bearer $token")
        
        STATE=$(echo "$RESPONSE" | jq -r '.state')
        
        # Convert state to lowercase for comparison (API may return "Running" or "running")
        STATE_LOWER=$(echo "$STATE" | tr '[:upper:]' '[:lower:]')
        
        case "$STATE_LOWER" in
            "running")
                log "✓ Instance is running"
                break
                ;;
            "stopped"|"shutdown"|"failed")
                error "Instance failed to start (state: $STATE)"
                echo "$RESPONSE" | jq .
                cleanup_instance "$token" "$INSTANCE_ID"
                return 1
                ;;
            *)
                echo -ne "\r  State: $STATE (poll $i/30)..."
                ;;
        esac
        
        sleep 2
    done
    echo ""
    
    if [ "$STATE_LOWER" != "running" ]; then
        error "Instance did not start in time (final state: $STATE)"
        cleanup_instance "$token" "$INSTANCE_ID"
        return 1
    fi
    
    # Give the container a moment to run
    sleep 2
    
    # Try to exec into the instance and run a simple command
    log "Executing test command in instance..."
    EXEC_RESPONSE=$(curl -s -X POST "$API_URL/instances/$INSTANCE_ID/exec" \
        -H "Authorization: Bearer $token" \
        -H "Content-Type: application/json" \
        -d '{
            "command": ["node", "-e", "console.log(\"E2E VM test passed!\")"],
            "timeout_seconds": 30
        }')
    
    EXEC_EXIT_CODE=$(echo "$EXEC_RESPONSE" | jq -r '.exit_code // -1')
    EXEC_STDOUT=$(echo "$EXEC_RESPONSE" | jq -r '.stdout // ""')
    
    if [ "$EXEC_EXIT_CODE" = "0" ]; then
        log "✅ Instance exec succeeded!"
        log "  Output: $EXEC_STDOUT"
    else
        warn "Instance exec returned exit code: $EXEC_EXIT_CODE"
        echo "$EXEC_RESPONSE" | jq .
    fi
    
    # Cleanup
    cleanup_instance "$token" "$INSTANCE_ID"
    
    return 0
}

# Cleanup instance
cleanup_instance() {
    local token="$1"
    local instance_id="$2"
    
    log "Cleaning up instance: $instance_id"
    
    # Stop the instance
    curl -s -X POST "$API_URL/instances/$instance_id/stop" \
        -H "Authorization: Bearer $token" > /dev/null 2>&1 || true
    
    # Wait a bit for it to stop
    sleep 2
    
    # Delete the instance
    curl -s -X DELETE "$API_URL/instances/$instance_id" \
        -H "Authorization: Bearer $token" > /dev/null 2>&1 || true
    
    log "✓ Instance cleaned up"
}

# Main
main() {
    log "=== E2E Build System Test ==="
    echo ""
    
    # Check prerequisites
    check_prerequisites
    echo ""
    
    # Generate token
    log "Generating JWT token..."
    TOKEN=$(generate_token)
    if [ -z "$TOKEN" ]; then
        error "Failed to generate token"
        error "Run: make gen-jwt"
        exit 1
    fi
    log "✓ Token generated"
    echo ""
    
    # Create test source
    log "Creating test Node.js source..."
    SOURCE=$(create_test_source)
    log "✓ Test source created: $SOURCE"
    echo ""
    
    # Submit build
    BUILD_ID=$(submit_build "$TOKEN" "$SOURCE")
    echo ""
    
    # Wait for completion and capture the response
    BUILD_RESPONSE=""
    log "Waiting for build to complete..."
    
    for i in $(seq 1 $TIMEOUT_POLLS); do
        BUILD_RESPONSE=$(curl -s "$API_URL/builds/$BUILD_ID" \
            -H "Authorization: Bearer $TOKEN")
        
        STATUS=$(echo "$BUILD_RESPONSE" | jq -r '.status')
        
        case "$STATUS" in
            "ready")
                log "✅ Build succeeded!"
                echo "$BUILD_RESPONSE" | jq .
                break
                ;;
            "failed")
                error "❌ Build failed!"
                echo "$BUILD_RESPONSE" | jq .
        echo ""
        log "=== Build Logs ==="
        get_logs "$TOKEN" "$BUILD_ID"
        echo ""
                error "=== E2E Test FAILED ==="
                rm -f "$SOURCE"
                exit 1
                ;;
            "cancelled")
                warn "Build was cancelled"
                rm -f "$SOURCE"
                exit 1
                ;;
            "queued"|"building"|"pushing")
                echo -ne "\r  Status: $STATUS (poll $i/$TIMEOUT_POLLS)..."
                ;;
            *)
                warn "Unknown status: $STATUS"
                ;;
        esac
        
        sleep $POLL_INTERVAL
    done
    echo ""
    
    if [ "$STATUS" != "ready" ]; then
        error "Build timed out after $((TIMEOUT_POLLS * POLL_INTERVAL)) seconds"
        rm -f "$SOURCE"
        exit 1
    fi
    
        echo ""
        log "=== Build Logs ==="
        get_logs "$TOKEN" "$BUILD_ID"
        echo ""
    
    # Run the built image (unless skipped)
    if [ "$SKIP_RUN" = "false" ]; then
        IMAGE_REF=$(echo "$BUILD_RESPONSE" | jq -r '.image_ref // empty')
        
        if [ -n "$IMAGE_REF" ]; then
            echo ""
            log "=== Running Built Image ==="
            if run_built_image "$TOKEN" "$IMAGE_REF"; then
                log "✅ VM run test passed!"
            else
                error "❌ VM run test failed!"
        rm -f "$SOURCE"
        exit 1
    fi
        else
            warn "No image_ref in build response, skipping VM test"
        fi
    else
        log "Skipping VM run test (--skip-run)"
    fi
    
    echo ""
    log "=== E2E Test PASSED ==="
    
    # Cleanup
    rm -f "$SOURCE"
    exit 0
}

main

