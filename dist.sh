#!/bin/bash

# Configuration
DIST_DIR="dist"
COMMANDS=("multicast_listener" "lsf2csv")
PLATFORMS=(
    "linux/amd64"
    "linux/arm64"
    "windows/amd64"
    "darwin/amd64"
    "darwin/arm64"
)

# Clean and create dist directory
rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

echo "Building imc-go tools..."

for platform in "${PLATFORMS[@]}"; do
    # Split platform into GOOS and GOARCH
    IFS="/" read -r -a parts <<< "$platform"
    GOOS="${parts[0]}"
    GOARCH="${parts[1]}"
    
    PLATFORM_DIR="$DIST_DIR/$GOOS-$GOARCH"
    mkdir -p "$PLATFORM_DIR"
    
    echo "  -> Building for $GOOS/$GOARCH..."
    
    for cmd in "${COMMANDS[@]}"; do
        OUT_FILE="$PLATFORM_DIR/$cmd"
        if [ "$GOOS" == "windows" ]; then
            OUT_FILE="$OUT_FILE.exe"
        fi
        
        GOOS=$GOOS GOARCH=$GOARCH go build -o "$OUT_FILE" "cmd/$cmd/main.go"
        
        if [ $? -ne 0 ]; then
            echo "FAILED to build $cmd for $platform"
        fi
    done
    
    # Copy IMC.xml to each platform folder as it's a runtime dependency
    if [ -f "IMC.xml" ]; then
        cp "IMC.xml" "$PLATFORM_DIR/"
    fi
done

echo "Build complete! Artifacts are in $DIST_DIR/"
