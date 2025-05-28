#/bin/bash

# This script builds the Rust project for multiple target platforms.

# Exit immediately if a command exits with a non-zero status.
set -e

# Define target platforms
TARGETS=(
    "x86_64-apple-darwin"    # macOS Intel
    "aarch64-apple-darwin"   # macOS Apple Silicon
    "x86_64-pc-windows-gnu"  # Windows (using GNU toolchain)
    "x86_64-unknown-linux-gnu" # Linux x86_64
    "aarch64-unknown-linux-gnu" # Linux ARM64
    "x86_64-unknown-freebsd" # FreeBSD x86_64
    "aarch64-unknown-freebsd" # FreeBSD ARM64
)

# Directory containing the Rust project
RUST_DIR="./rust"

# Check if the Rust directory exists
if [ ! -d "$RUST_DIR" ]; then
    echo "Error: Rust project directory '$RUST_DIR' not found."
    exit 1
fi

# Navigate to the Rust project directory
cd "$RUST_DIR"

# Remove the target directory before building
echo "Cleaning previous build artifacts..."
rm -rf "rust/target"

echo "Building Rust project for multiple targets..."

# Loop through each target and build
for TARGET in "${TARGETS[@]}"; do
    echo ""
    echo "----------------------------------------"
    echo "Building for target: $TARGET"
    echo "----------------------------------------"

    # Check if the target toolchain is installed, install if not.
    # Note: This check is basic. A more robust script might parse `rustup target list`.
    if ! rustup target list | grep -q "$TARGET"; then
        echo "Toolchain for $TARGET not found. Attempting to install..."
        rustup target add "$TARGET"
        if [ $? -ne 0 ]; then
            echo "Error installing toolchain for $TARGET. Please install it manually using 'rustup target add $TARGET'."
            # Continue to the next target instead of exiting, as some targets might succeed.
            continue
        fi
    fi

    # Build the project for the target in release mode
    cargo build --release --target "$TARGET"

    if [ $? -ne 0 ]; then
        echo "Error building for target $TARGET."
        # Continue to the next target instead of exiting.
        continue
    fi

    echo "Successfully built for target: $TARGET"
done

echo ""
echo "----------------------------------------"
echo "Build process finished."
echo "Executables can be found in rust/target/<target>/release/"
echo "----------------------------------------"

cp rust/target/aarch64-apple-darwin/release/dl /rust/release/dl-mac-arm
cp rust/target/x86_64-apple-darwin/release/dl /rust/release/dl-mac-intel
cp rust/target/x86_64-pc-windows-gnu/release/dl.exe /rust/release/dl.exe