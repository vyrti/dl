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


### Command-Line Arguments

*   `-c <concurrency_level>`: (Optional) Sets the number of concurrent downloads. Defaults to `3`.
*   `<path_to_urls_file>`: (Required) The path to the text file containing the URLs to download.

## Rust Version

This is a separate implementation of the downloader tool written in Rust.

### Command-Line Arguments

*   `-c <concurrency_level>`: (Optional) Sets the number of concurrent downloads. Defaults to `3`.
*   `<path_to_urls_file>`: (Required) The path to the text file containing the URLs to download.

## Research
Main working application using GO lang in this repo was created using base code from Claude Sonnet 4 with Gemini 2.5 Pro review. Two models was working on same code, one after another. Result in this repo is 2 prompts total. One prompt for each model.

Rust version was also created by Claude Sonnet 4 with Gemini 2.5 Pro review with 5 promts.

The `research/` folder contains experimental or alternative implementations and related analysis from various AI models, in a try to create app with one model ONLY. Each subdirectory corresponds to a model's attempt to generate the downloader tool based on a specific prompt. The `info.txt` file in each directory summarizes the outcome of the interaction with the model.

*   `chatgpt-canvas/`:
    *   no working code after 5 additional prompts
*   `chatgpt-think/`:
    *   partial working code
    *   all progress bars on same line
*   `deepseekr1/`:
    *   working code after 2 prompts
*   `deepseekr1-0528/`:
    *   no working code after 8 (!) additional prompts
*   `gemini-2.5-pro/`:
    *   working code after 4 prompts, more functions than in main app in this repo
*   `gemini-flash-2.5/`:
    *   no working code after 5 correcting promts
    *   only model that gave 10 diff code options
*   `grok3/`:
    *   no working code after 3 correcting prompts
*   `qwen3-32b/`:
    *   no working code after 5 correcting promts

*   `deepseekr1/compare.txt`: A comparison document analyzing the `dl.deepseek.r1.go` code against another version, highlighting areas where the other version demonstrates more robust and professional terminal handling, speed calculation, error handling, and overall structure.

### Alternative

```bash
xargs -P4 -n1 curl -O < urls.txt
```

This will work on linux, mac and windows (if git bash installed)

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.