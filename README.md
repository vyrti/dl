# 100% AI Generated

This software code was generated 100% using AI tools such as Google Gemini 2.5 Pro and Claude Sonnet 4.
Please be aware of potential legal risks associated with using AI-generated code, including but not limited to copyright infringement or lack of ownership.

# DL

DL is a command-line tool written in Go and Rust for downloading multiple files concurrently from a list of URLs. Both versions feature a dynamic progress bar display for each download, showing speed, percentage, and downloaded/total size.

## Features

*   **Concurrent Downloads:** Downloads multiple files at the same time, configurable via a command-line flag.
*   **Dynamic Progress Bars:** Displays a progress bar for each active download, including:
    *   Filename (truncated for display if too long).
    *   Visual progress bar.
    *   Percentage completion.
    *   Downloaded size / Total size (in MB).
    *   Current download speed (B/s, KB/s, or MB/s).
    *   Handles indeterminate progress (when total file size is unknown) with a spinner.
*   **URL Input:** Reads URLs from a specified text file (one URL per line).
*   **Organized Output:** Saves downloaded files into a `downloads/` subdirectory (created if it doesn't exist).
*   **Error Handling:** Reports common download errors (HTTP issues, file creation problems) directly in the progress display.
*   **Filename Derivation:** Attempts to derive a sensible filename from the URL. Handles common patterns like `?download=true` suffixes and generates unique names if necessary.
*   **Clean UI:** Uses ANSI escape codes to update progress bars in place, providing a clean terminal interface.
*   **Cross-Platform:** Works on Windows, macOS, and Linux.

## Prerequisites

*   Go (version 1.18 or newer recommended, as the `go run` command is used in examples)

## Installation / Building

You can run the program directly using `go run` or build an executable.

1.  **Clone the repository (or ensure `main.go` is in your current directory):**
    ```bash
    # If you have it in a git repo:
    # git clone <repository_url>
    # cd <repository_name>
    ```

2.  **Run directly:**
    ```bash
    go run main.go urls.txt
    ```

3.  **Build an executable:**
    ```bash
    go build main.go
    ```
    This will create an executable file (e.g., `main` or `main.exe`). You can then run it:
    ```bash
    ./main urls.txt
    ```

## Usage

1.  **Create a text file** (e.g., `urls.txt`) containing the URLs you want to download, one URL per line:
    ```text
    // urls.txt
    http://example.com/file1.zip
    https://another-example.com/document.pdf
    http://speedtest.tele2.net/1MB.zip
    ```

2.  **Run the downloader:**
    ```bash
    go run main.go -c <concurrency_level> <path_to_urls_file>
    ```

    **Example:**
    ```bash
    go run main.go urls.txt
    ```

    **Example with custom concurrency (e.g., 5 concurrent downloads):**
    ```bash
    go run main.go -c 5 urls.txt
    ```

### Command-Line Arguments

*   `-c <concurrency_level>`: (Optional) Sets the number of concurrent downloads. Defaults to `3`.
*   `<path_to_urls_file>`: (Required) The path to the text file containing the URLs to download.

## How it Works

The program reads URLs from the input file. For each URL, it attempts to:
1.  Fetch the HEAD of the URL to get the `Content-Length` for total file size (if available).
2.  Initiate a GET request to download the file.
3.  Save the file into the `downloads/` directory.
4.  A `ProgressManager` handles the display of multiple `ProgressWriter` instances, each tracking a single download.
5.  Progress bars are updated periodically, showing current status, downloaded amount, percentage, and speed.
6.  A semaphore limits the number of active downloads to the specified concurrency level.

## Rust Version

This is a separate implementation of the downloader tool written in Rust.

### Prerequisites

*   Rust toolchain (install via [rustup](https://rustup.rs/))

### Installation / Building

1.  **Clone the repository (or ensure the `rust/` directory is in your current directory):**
    ```bash
    # If you have it in a git repo:
    # git clone <repository_url>
    # cd <repository_name>
    ```

2.  **Build the executable:**
    ```bash
    cargo build --release
    ```
    This will create an optimized executable for your current platform in `target/release/dl`.

3.  **Build executables for multiple platforms:**
    You can use the provided `build.sh` script to build executables for macOS (Intel and Apple Silicon), Windows, and Linux.
    ```bash
    ./build.sh
    ```
    Ensure you have the necessary Rust toolchains installed (`rustup target add <target>`).

4.  **Run directly:**
    ```bash
    cargo run --release urls.txt
    ```

5.  **Run the built executable (for your current platform):**
    ```bash
    ./target/release/dl urls.txt
    ```

### Usage

1.  **Create a text file** (e.g., `urls.txt`) containing the URLs you want to download, one URL per line:
    ```text
    // urls.txt
    http://example.com/file1.zip
    https://another-example.com/document.pdf
    http://speedtest.tele2.net/1MB.zip
    ```

2.  **Run the downloader:**
    ```bash
    ./target/release/dl -c <concurrency_level> <path_to_urls_file>
    ```

    **Example:**
    ```bash
    ./target/release/dl urls.txt
    ```

    **Example with custom concurrency (e.g., 5 concurrent downloads):**
    ```bash
    ./target/release/dl -c 5 urls.txt
    ```

### Command-Line Arguments

*   `-c <concurrency_level>`: (Optional) Sets the number of concurrent downloads. Defaults to `3`.
*   `<path_to_urls_file>`: (Required) The path to the text file containing the URLs to download.

## Research

The `research/` folder contains experimental or alternative implementations and related analysis. Currently, it includes:

*   `deepseekr1/dl.deepseek.r1.go`: An alternative implementation of a downloader tool written in Go.
*   `deepseekr1/compare.txt`: A comparison document analyzing the `dl.deepseek.r1.go` code against another version, highlighting areas where the other version demonstrates more robust and professional terminal handling, speed calculation, error handling, and overall structure.

### Alternative

```bash
xargs -P4 -n1 curl -O < urls.txt
```

This will work on linux, mac and windows (if git bash installed)

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.


