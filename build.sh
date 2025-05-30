// build.sh
#!/bin/bash

# Exit immediately if a command exits with a non-zero status.
set -e

# Define the application version
APP_VERSION="v0.1.4"

# Define the source directory
SOURCE_DIR="./go.stable"

# Define the output directory
OUTPUT_DIR="./build"

# Create the output directory if it doesn't exist
mkdir -p "$OUTPUT_DIR"

echo "Starting build process..."

# macOS Builds
echo "Building for macOS Intel (amd64)..."
GOOS=darwin GOARCH=amd64 go build -ldflags="-X main.CurrentAppVersion=$APP_VERSION -s -w" -o "$OUTPUT_DIR/dl.apple.intel" "$SOURCE_DIR"
echo "Output: $OUTPUT_DIR/dl.apple.intel"

echo "Building for macOS M1 (arm64)..."
GOOS=darwin GOARCH=arm64 go build -ldflags="-X main.CurrentAppVersion=$APP_VERSION -s -w" -o "$OUTPUT_DIR/dl.apple.arm" "$SOURCE_DIR"
echo "Output: $OUTPUT_DIR/dl.apple.arm"

# Windows Builds
echo "Building for Windows x86 (amd64)..."
GOOS=windows GOARCH=amd64 go build -ldflags="-X main.CurrentAppVersion=$APP_VERSION -s -w" -o "$OUTPUT_DIR/dl.win.x64.exe" "$SOURCE_DIR"
echo "Output: $OUTPUT_DIR/dl.win.x64.exe"

echo "Building for Windows ARM (arm64)..."
GOOS=windows GOARCH=arm64 go build -ldflags="-X main.CurrentAppVersion=$APP_VERSION -s -w" -o "$OUTPUT_DIR/dl.win.arm.exe" "$SOURCE_DIR"
echo "Output: $OUTPUT_DIR/dl.win.arm.exe"

# Linux Builds
echo "Building for Linux x86 (amd64)..."
GOOS=linux GOARCH=amd64 go build -ldflags="-X main.CurrentAppVersion=$APP_VERSION -s -w" -o "$OUTPUT_DIR/dl.linux.x64" "$SOURCE_DIR"
echo "Output: $OUTPUT_DIR/dl.linux.x64"

echo "Building for Linux ARM (arm64)..."
GOOS=linux GOARCH=arm64 go build -ldflags="-X main.CurrentAppVersion=$APP_VERSION -s -w" -o "$OUTPUT_DIR/dl.linux.arm" "$SOURCE_DIR"
echo "Output: $OUTPUT_DIR/dl.linux.arm"

echo "Build process completed."
echo "Binaries are located in the '$OUTPUT_DIR' directory."

# List the contents of the build directory
ls -l "$OUTPUT_DIR"