# 100% AI Generated

This software code was generated 100% using AI tools such as Google Gemini 2.5 Pro and Claude Sonnet 4.
Please be aware of potential legal risks associated with using AI-generated code, including but not limited to copyright infringement or lack of ownership.


# DL

DL is a command-line tool written in Go for downloading multiple files concurrently from a list of URLs or a Hugging Face repository. It features a dynamic progress bar display for each download, showing speed, percentage, and downloaded/total size. The tool supports advanced Hugging Face repository handling, including interactive selection of specific `.gguf` files or series.
Auto-update is available with -update.

![Screenshot of DL tool](image.png)


## Quick Start

1. **Build the tool for all major platforms:**
    ```bash
    ./build.sh
    ```
    Binaries will be placed in the `build/` directory for macOS (Intel/ARM), Windows (x64/ARM), and Linux (x64/ARM).

2. **Download from a URL list:**
    ```bash
    ./dl -f ../download_links.txt -c 4
    ```

3. **Download from a Hugging Face repo:**
    ```bash
    ./dl -hf "Qwen/Qwen3-30B-A3B"
    ```

4. **Select a GGUF file/series from a Hugging Face repo:**
    ```bash
    ./dl -hf "unsloth/DeepSeek-R1-0528-GGUF" -select
    ```

5. **Download a pre-defined model by alias:**
    ```bash
    ./dl -m qwen3-0.6b
    ```

6. **Search for models on Hugging Face:**
    ```bash
    ./dl model search llama 7b gguf
    ```

7. **Install, update, or remove llama.cpp binaries:**
    ```bash
    ./dl install llama-mac-arm
    ./dl update llama
    ./dl remove llama-win-cuda
    ```

8. **Show system hardware info:**
    ```bash
    ./dl -t
    ```

9. **Self-update the tool:**
    ```bash
    ./dl --update
    ```


### Features

*   **Concurrent Downloads:** Download multiple files at once, with concurrency caps for file lists and Hugging Face downloads.
*   **Multiple Input Sources:** Download from a URL list (`-f`), Hugging Face repo (`-hf`), or direct URLs.
*   **Model Registry:** Use `-m <alias>` to download popular models by shortcut (see below).
*   **Model Search:** Search Hugging Face models from the command line.
*   **Resume:** Resume supported
*   **Llama.cpp App Management:** Install, update, or remove pre-built llama.cpp binaries for your platform.
*   **Hugging Face GGUF Selection:** Use `-select` to interactively choose `.gguf` files or series from Hugging Face repos.
*   **Dynamic Progress Bars:** Per-download progress bars with speed, ETA, and more.
*   **Pre-scanning:** HEAD requests to determine file size before download.
*   **Organized Output:** Downloads go to `downloads/`, with subfolders for Hugging Face repos and models.
*   **Error Handling:** Clear error messages and robust handling of download issues.
*   **Filename Derivation:** Smart filename handling for URLs and Hugging Face files.
*   **Clean UI:** ANSI escape codes for a tidy terminal interface.
*   **Debug Logging:** Enable with `-debug` (logs to `log.log`).
*   **System Info:** Show hardware info with `-t`.
*   **Self-Update:** Update the tool with `-update`.
*   **Cross-Platform:** Windows, macOS, and Linux supported.

### Command-Line Arguments

> **Note:** You must provide only one of the following: `-f`, `-hf`, `-m`, or direct URLs.

*   `-c <concurrency_level>`: (Optional) Number of concurrent downloads. Defaults to `3`. Capped at 4 for Hugging Face, 100 for file lists.
*   `-f <path_to_urls_file>`: Download from a text file of URLs (one per line).
*   `-hf <repo_input>`: Download all files from a Hugging Face repo (`owner/repo_name` or full URL).
*   `-m <model_alias>`: Download a pre-defined model by alias (see Model Registry below).
*   `--token`: Use the `HF_TOKEN` environment variable for Hugging Face API requests and downloads. Necessary for gated or private repositories. The `HF_TOKEN` variable must be set in your environment.
*   `-select`: (Hugging Face only) Interactively select `.gguf` files or series.
*   `-debug`: Enable debug logging to `log.log`.
*   `--update`: Self-update the tool.
*   `-t`: Show system hardware info.
*   `install <app_name>`: Install a pre-built llama.cpp binary (see below).
*   `update <app_name>`: Update a llama.cpp binary.
*   `remove <app_name>`: Remove a llama.cpp binary.
*   `model search <query>`: Search Hugging Face models from the command line. Can be used with `--token`.

---

## Model Registry

You can use the `-m` flag with the following aliases to quickly download popular models:

qwen3-4b, qwen3-8b, qwen3-14b, qwen3-32b, qwen3-30b-moe, gemma3-27b

---

## Model Search

Search for models on Hugging Face directly from the command line:

```bash
./dl model search llama 7b gguf
```

---

## Llama.cpp App Management

Install, update, or remove official pre-built llama.cpp binaries for your platform from github:

```bash
./dl install llama-mac-arm
./dl update llama
./dl remove llama-win-cuda
```

---

## System Info

Show system hardware information:

```bash
./dl -t
```

---

## Self-Update

Update the tool to the latest version:

```bash
./dl --update
```

---

## Build

To build the tool for all supported platforms, run:

```bash
./build.sh
```

This will produce binaries for macOS (Intel/ARM), Windows (x64/ARM), and Linux (x64/ARM) in the `build/` directory.

---

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.