#!/bin/bash

# Configuration
VERSION="${VERSION:-1.0.0}"
DIST_DIR="dist"
COMMANDS=("multicast_listener" "lsf2csv" "lsf2json" "lsfmerge" "lsfsplit" "logsync")
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

echo "Building imc-go tools v${VERSION}..."

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

# --- .deb packaging (Linux) ---
build_deb() {
    local ARCH="$1"
    local SRC_DIR="$DIST_DIR/linux-$ARCH"
    local PKG_NAME="imc-go-tools"
    local PKG_DIR="$DIST_DIR/${PKG_NAME}_${VERSION}_${ARCH}"

    if [ ! -d "$SRC_DIR" ]; then
        echo "  Skipping .deb for $ARCH: $SRC_DIR not found"
        return
    fi

    echo "  -> Packaging .deb for $ARCH..."

    rm -rf "$PKG_DIR"
    mkdir -p "$PKG_DIR/DEBIAN"
    mkdir -p "$PKG_DIR/usr/bin"
    mkdir -p "$PKG_DIR/usr/share/imc-go"

    for cmd in "${COMMANDS[@]}"; do
        install -m 755 "$SRC_DIR/$cmd" "$PKG_DIR/usr/bin/$cmd"
    done

    if [ -f "$SRC_DIR/IMC.xml" ]; then
        install -m 644 "$SRC_DIR/IMC.xml" "$PKG_DIR/usr/share/imc-go/IMC.xml"
    fi

    cat > "$PKG_DIR/DEBIAN/control" << EOF
Package: $PKG_NAME
Version: $VERSION
Section: science
Priority: optional
Architecture: $ARCH
Maintainer: OceanScan <info@oceanscan-mst.com>
Description: IMC protocol tools for LSTS vehicles
 A collection of tools for working with IMC (Inter-Module Communication)
 protocol logs and data from LSTS underwater vehicles.
 .
 Includes: ${COMMANDS[*]}
EOF

    dpkg-deb --build --root-owner-group "$PKG_DIR" "$DIST_DIR/${PKG_NAME}_${VERSION}_${ARCH}.deb"

    if [ $? -eq 0 ]; then
        rm -rf "$PKG_DIR"
        echo "  -> Created ${PKG_NAME}_${VERSION}_${ARCH}.deb"
    else
        echo "  FAILED to build .deb for $ARCH"
    fi
}

if command -v dpkg-deb &> /dev/null; then
    echo ""
    echo "Building .deb packages..."
    build_deb "amd64"
    build_deb "arm64"
else
    echo ""
    echo "dpkg-deb not found, skipping .deb packages."
    echo "Install dpkg-dev to enable: sudo apt install dpkg-dev"
fi

# --- .msi packaging (Windows only — use packaging/wix/build.bat on Windows/CI) ---
if [ -d "$DIST_DIR/windows-amd64" ]; then
    echo ""
    echo "To build Windows .msi installer, run on Windows:"
    echo "  packaging\\wix\\build.bat $VERSION"
fi

# --- .tar.gz archives (macOS + Linux) ---
echo ""
echo "Building .tar.gz archives..."
for platform_dir in "$DIST_DIR"/darwin-* "$DIST_DIR"/linux-*; do
    if [ ! -d "$platform_dir" ]; then
        continue
    fi
    name="$(basename "$platform_dir")"
    archive="$DIST_DIR/imc-go-tools_${VERSION}_${name}.tar.gz"
    tar -czf "$archive" -C "$DIST_DIR" "$name"
    echo "  -> Created $(basename "$archive")"
done
