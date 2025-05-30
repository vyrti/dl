// go.beta/main.go
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DownloadItem represents a file to be downloaded.
type DownloadItem struct {
	URL               string
	PreferredFilename string // Optional, from HF's rfilename or similar context. Can include subdirs.
}

// For Hugging Face GGUF selection
type GGUFFileWithPartNum struct {
	File    HFFile
	PartNum int
}
type GGUFSeriesInfo struct {
	BaseName      string // e.g., "BF16/DeepSeek-R1-0528-BF16" (includes path within repo)
	TotalParts    int
	SeriesKey     string // For map key and sorting, e.g., "BF16/DeepSeek-R1-0528-BF16-of-00030"
	FilesWithPart []GGUFFileWithPartNum
}

type SelectableGGUFItem struct {
	DisplayName     string   // e.g., "Series: BF16/model (30 parts, 12.34 GB)" or "File: standalone.gguf, 0.01 GB"
	FilesToDownload []HFFile // All HFFile objects for this selection
}

// Regex to capture GGUF series: (base_name)-(part_num)-of-(total_parts).gguf
var ggufSeriesRegex = regexp.MustCompile(`^(.*?)-(\d{5})-of-(\d{5})\.gguf$`)

// Predefined model registry
var modelRegistry = map[string]string{
	"qwen3-0.6b":    "https://huggingface.co/Qwen/Qwen3-4B-GGUF/resolve/main/Qwen3-4B-Q4_K_M.gguf?download=true",
	"qwen3-1.7b":    "https://huggingface.co/Qwen/Qwen3-8B-GGUF/resolve/main/Qwen3-8B-Q4_K_M.gguf?download=true",
	"qwen3-4b":      "https://huggingface.co/Qwen/Qwen3-4B-GGUF/resolve/main/Qwen3-4B-Q4_K_M.gguf?download=true",
	"qwen3-8b":      "https://huggingface.co/Qwen/Qwen3-8B-GGUF/resolve/main/Qwen3-8B-Q4_K_M.gguf?download=true",
	"qwen3-16b":     "https://huggingface.co/Qwen/Qwen3-16B-GGUF/resolve/main/Qwen3-16B-Q4_K_M.gguf?download=true",
	"qwen3-32b":     "https://huggingface.co/Qwen/Qwen3-32B-GGUF/resolve/main/Qwen3-32B-Q4_K_M.gguf?download=true",
	"qwen3-30b-moe": "https://huggingface.co/Qwen/Qwen3-16B-GGUF/resolve/main/Qwen3-16B-Q4_K_M.gguf?download=true",
	"gemma3-27b":    "https://huggingface.co/unsloth/gemma-3-27b-it-GGUF/resolve/main/gemma-3-27b-it-Q4_0.gguf?download=true",
}

// Package-level variables for global access (e.g., by signal handlers, main defer)
var manager *ProgressManager

func printModelUsage() {
	baseCmd := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, "Usage: %s model <subcommand> [options]\n", baseCmd)
	fmt.Fprintln(os.Stderr, "\nSubcommands:")
	fmt.Fprintln(os.Stderr, "  search <query>   Search for models on Hugging Face.")
	fmt.Fprintln(os.Stderr, "\nExample for model command:")
	fmt.Fprintf(os.Stderr, "  %s model search llama 7b gguf\n", baseCmd)
}

func printModelSearchUsage() {
	baseCmd := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, "Usage: %s model search <query>\n", baseCmd)
	fmt.Fprintln(os.Stderr, "\nArguments:")
	fmt.Fprintln(os.Stderr, "  <query>          The search term for models (e.g., 'bert', 'text generation').")
	fmt.Fprintln(os.Stderr, "\nExample for model search:")
	fmt.Fprintf(os.Stderr, "  %s model search llama 7b gguf\n", baseCmd)
}

func printUsage() {
	baseCmd := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, "Usage: %s [flags] <URL1> <URL2> ...\n", baseCmd)
	fmt.Fprintln(os.Stderr, "Or manage pre-configured applications:")
	fmt.Fprintf(os.Stderr, "  %s install <app_name>\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  %s update <app_name>\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  %s remove <app_name>\n", baseCmd)
	fmt.Fprintln(os.Stderr, "Or search for models:")
	fmt.Fprintf(os.Stderr, "  %s model search <query>\n", baseCmd)
	fmt.Fprintln(os.Stderr, "\n  Available <app_name> for install/update/remove:")
	fmt.Fprintln(os.Stderr, "    llama            (Generic CPU build for your OS/Architecture)")
	fmt.Fprintln(os.Stderr, "    llama-win-cuda   (CUDA-enabled build for Windows x64)")
	fmt.Fprintln(os.Stderr, "    llama-mac-arm    (Metal-enabled build for macOS ARM64)")
	fmt.Fprintln(os.Stderr, "    llama-linux-cuda (CUDA-enabled build for Linux, matching your system's CUDA-compatible architecture)")
	fmt.Fprintln(os.Stderr, "\nFlags for URL/repository downloading (run with -h or --help for details):")
	fmt.Fprintln(os.Stderr, "  Use '"+baseCmd+" -h' for a list of downloader flags and more examples.")

	fmt.Fprintln(os.Stderr, "\nExamples:")
	fmt.Fprintf(os.Stderr, "  %s http://example.com/file.zip\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  %s -f urls.txt -c 5\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  %s -hf TheBloke/Llama-2-7B-GGUF -select\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  %s install llama-linux-cuda\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  %s update llama\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  %s model search text-generation\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  %s --update (for self-updating the application)\n", baseCmd)
}

func main() {
	// Temporarily parse -debug flag early for initLogging
	// This sets the global `debugMode` variable.
	for _, arg := range os.Args {
		if arg == "-debug" {
			debugMode = true // global debugMode from downloader.go
			break
		}
	}
	initLogging() // Initialize logging using global `debugMode`

	var exitCode int

	// This defer is the final gate before os.Exit. It handles panics and ensures cleanup.
	defer func() {
		if r := recover(); r != nil {
			if appLogger != nil {
				appLogger.Printf("PANIC encountered: %+v", r)
			} else {
				// This case should ideally not happen if initLogging is robust
				fmt.Fprintf(os.Stderr, "PANIC encountered before logger initialization: %+v\n", r)
			}
			fmt.Fprintf(os.Stderr, "\n[CRITICAL] Application panicked: %v\n", r)

			// If a panic occurred, try to stop the manager gracefully to restore cursor.
			if manager != nil {
				fmt.Fprintln(os.Stderr, "[INFO] Attempting to restore terminal state due to panic...")
				manager.Stop() // manager.Stop() is responsible for restoring the cursor.
			} else {
				// Fallback if manager was not initialized or already nil.
				fmt.Print("\033[?25h")
			}
			if exitCode == 0 { // If panic happened and no specific exit code was set by runActual
				exitCode = 2 // Use a distinct exit code for panics
			}
		}

		// Close log file if it was opened.
		if logFile != nil { // logFile is global from downloader.go
			if appLogger != nil {
				appLogger.Println("--- Main: Logging Finished (deferred close) ---")
			}
			logFile.Close()
		}
		if appLogger != nil { // Ensure appLogger exists before trying to use it
			appLogger.Printf("Exiting with code %d", exitCode)
		} else {
			fmt.Printf("Exiting with code %d (logger was not available)\n", exitCode)
		}
		os.Exit(exitCode)
	}()

	// Handle signal for graceful shutdown
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-signalChan
		if appLogger != nil {
			appLogger.Printf("Signal received: %s. Initiating shutdown.", sig)
		}
		fmt.Fprintln(os.Stderr, "\n[INFO] Interrupt signal received. Cleaning up and exiting...")
		if manager != nil {
			manager.Stop() // This should restore the cursor.
		} else {
			fmt.Print("\033[?25h") // Fallback if manager is not active.
		}
		if logFile != nil && appLogger != nil { // logFile and appLogger are global
			appLogger.Println("--- Main: Logging Finished (signal handler close) ---")
			// logFile.Close() // logFile will be closed by the main defer
		}
		if appLogger != nil {
			appLogger.Println("Exiting due to signal (code 1).")
		}
		os.Exit(1) // Exit directly after cleanup. This bypasses the main defer's os.Exit.
	}()

	exitCode = runActual()
}

// runActual contains the original core logic of main() and returns an exit code.
func runActual() int {
	// Note: `manager` is a package-level variable.
	// `appLogger`, `logFile`, `debugMode` are also package-level (from downloader.go).

	// Handle install/update/remove/model commands first
	if len(os.Args) > 1 {
		command := os.Args[1]
		var appName string    // For install/update/remove
		var subCommand string // For model

		switch command {
		case "install", "update", "remove":
			if len(os.Args) > 2 {
				appName = os.Args[2]
				if strings.HasPrefix(appName, "-") {
					fmt.Fprintf(os.Stderr, "Error: Invalid <app_name> '%s' for %s command. App name cannot be a flag.\n", appName, command)
					printUsage()
					return 1
				}
			} else {
				fmt.Fprintf(os.Stderr, "Error: Missing <app_name> for %s command.\n", command)
				printUsage()
				return 1
			}

			if command == "install" || command == "update" {
				manager = NewProgressManager(1)
				defer manager.Stop()
			}

			switch command {
			case "install":
				HandleInstallLlamaApp(manager, appName)
			case "update":
				HandleUpdateLlamaApp(manager, appName)
			case "remove":
				HandleRemoveLlamaApp(appName)
			}
			return 0 // Commands handled

		case "model":
			if len(os.Args) > 2 {
				subCommand = os.Args[2]
				if subCommand == "search" {
					if len(os.Args) > 3 {
						searchQuery := strings.Join(os.Args[3:], " ")
						HandleModelSearch(searchQuery) // From functions_search.go
						return 0                       // Command handled
					}
					fmt.Fprintln(os.Stderr, "Error: Missing search query for 'model search' command.")
					printModelSearchUsage()
					return 1
				}
				fmt.Fprintf(os.Stderr, "Error: Unknown subcommand '%s' for 'model' command.\n", subCommand)
				printModelUsage()
				return 1
			}
			fmt.Fprintln(os.Stderr, "Error: Missing subcommand for 'model' command (e.g., 'search').")
			printModelUsage()
			return 1
		}
		// If it was not a recognized management command, proceed to flag parsing.
	}

	// --- Existing Flag-based command processing ---
	var concurrency int
	var urlsFilePath, hfRepoInput, modelName string
	var selectFile bool
	var showSysInfo bool
	var updateAppSelf bool

	// Use a new FlagSet for downloader-specific flags.
	downloaderFlags := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)
	baseCmdName := downloaderFlags.Name() // Store for use in usage messages

	downloaderFlags.BoolVar(&debugMode, "debug", debugMode, "Enable debug logging to log.log")
	downloaderFlags.BoolVar(&showSysInfo, "t", false, "Show system hardware information and exit")
	downloaderFlags.BoolVar(&updateAppSelf, "update", false, "Check for and apply application self-updates (use '--update')")
	downloaderFlags.IntVar(&concurrency, "c", 3, "Number of concurrent downloads & display lines")
	downloaderFlags.StringVar(&urlsFilePath, "f", "", "Path to text file containing URLs to download directly")
	downloaderFlags.StringVar(&hfRepoInput, "hf", "", "Hugging Face repository ID (e.g., owner/repo_name) or full URL")
	downloaderFlags.StringVar(&modelName, "m", "", "Predefined model alias to download")
	downloaderFlags.BoolVar(&selectFile, "select", false, "Allow selecting files if downloading from a Hugging Face repository")

	downloaderFlags.Usage = func() { // Custom usage for this flag set
		fmt.Fprintf(downloaderFlags.Output(), "Usage: %s [flags] <URL1> <URL2> ...\n", baseCmdName)

		fmt.Fprintln(downloaderFlags.Output(), "\nThis tool also supports application and model management commands:")
		fmt.Fprintf(downloaderFlags.Output(), "  %s install <app_name>\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s update <app_name>\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s remove <app_name>\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s model search <query>\n", baseCmdName)

		fmt.Fprintln(downloaderFlags.Output(), "\n  Available <app_name> for install/update/remove:")
		fmt.Fprintln(downloaderFlags.Output(), "    llama            (Generic CPU build for your OS/Architecture)")
		fmt.Fprintln(downloaderFlags.Output(), "    llama-win-cuda   (CUDA-enabled build for Windows x64)")
		fmt.Fprintln(downloaderFlags.Output(), "    llama-mac-arm    (Metal-enabled build for macOS ARM64)")
		fmt.Fprintln(downloaderFlags.Output(), "    llama-linux-cuda (CUDA-enabled build for Linux, matching your system's CUDA-compatible architecture)")
		fmt.Fprintln(downloaderFlags.Output(), "\nNote: The 'install', 'update', 'remove', and 'model' commands are processed before the flags listed below.")

		fmt.Fprintln(downloaderFlags.Output(), "\nFlags for URL/repository downloading:")
		downloaderFlags.PrintDefaults()

		fmt.Fprintln(downloaderFlags.Output(), "\nExamples for URL/repository downloading:")
		fmt.Fprintf(downloaderFlags.Output(), "  %s http://example.com/file.zip\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s -c 5 http://example.com/file1.zip http://example.com/file2.tar.gz\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s -f urls.txt\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s -hf TheBloke/Llama-2-7B-GGUF\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s -hf TheBloke/Llama-2-7B-GGUF -select\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s -m qwen3-8b\n", baseCmdName)

		fmt.Fprintln(downloaderFlags.Output(), "\nExamples for application and model management:")
		fmt.Fprintf(downloaderFlags.Output(), "  %s install llama-linux-cuda\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s update llama\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s remove llama-mac-arm\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s model search \"text to image\"\n", baseCmdName)

		fmt.Fprintln(downloaderFlags.Output(), "\nOther utility commands/flags:")
		fmt.Fprintf(downloaderFlags.Output(), "  %s --update                             (Self-update this application)\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s -t                                   (Show system hardware information)\n", baseCmdName)
	}

	err := downloaderFlags.Parse(os.Args[1:])
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 1
	}

	if updateAppSelf {
		actionFlagsUsed := 0
		downloaderFlags.Visit(func(f *flag.Flag) {
			if f.Name != "update" && f.Name != "debug" {
				actionFlagsUsed++
			}
		})
		if downloaderFlags.NArg() > 0 {
			actionFlagsUsed++
		}
		if actionFlagsUsed > 0 {
			appLogger.Println("Error: --update flag (for self-update) cannot be used with other action flags or direct URLs.")
			fmt.Fprintln(os.Stderr, "Error: --update flag (for self-update) cannot be used with other action flags (-f, -hf, -m, -t) or direct URLs.")
			return 1
		}
		HandleUpdate()
		return 0
	}

	if showSysInfo {
		actionFlagsUsed := 0
		downloaderFlags.Visit(func(f *flag.Flag) {
			if f.Name != "t" && f.Name != "debug" {
				actionFlagsUsed++
			}
		})
		if downloaderFlags.NArg() > 0 {
			actionFlagsUsed++
		}
		if actionFlagsUsed > 0 {
			appLogger.Printf("Error: -t flag cannot be used with other action flags or direct URLs.")
			fmt.Fprintf(os.Stderr, "Error: -t flag cannot be used with other action flags or direct URLs.\n")
			return 1
		}
		appLogger.Println("[Main] System info requested via -t flag. Displaying info and exiting.")
		ShowSystemInfo()
		return 0
	}

	appLogger.Println("Application starting in downloader mode...")

	modesSet := 0
	if urlsFilePath != "" {
		modesSet++
	}
	if hfRepoInput != "" {
		modesSet++
	}
	if modelName != "" {
		modesSet++
	}
	if downloaderFlags.NArg() > 0 && urlsFilePath == "" && hfRepoInput == "" && modelName == "" {
		modesSet++
	}

	if modesSet == 0 {
		appLogger.Println("Error: No download mode specified (-f, -hf, -m, or direct URLs) and no other command given.")
		fmt.Fprintln(os.Stderr, "Error: No download mode specified or direct URLs provided.")
		downloaderFlags.Usage()
		return 1
	}
	if modesSet > 1 {
		appLogger.Println("Error: Flags -f, -hf, -m, and direct URLs are mutually exclusive.")
		fmt.Fprintln(os.Stderr, "Error: Flags -f, -hf, -m, and direct URLs are mutually exclusive. Please use only one.")
		downloaderFlags.Usage()
		return 1
	}

	effectiveConcurrency := concurrency
	if modelName != "" {
		effectiveConcurrency = 1
		appLogger.Printf("Concurrency display overridden to 1 for -m.")
	} else if hfRepoInput != "" {
		maxHfConcurrency := 4
		if effectiveConcurrency <= 0 || effectiveConcurrency > maxHfConcurrency {
			effectiveConcurrency = maxHfConcurrency
		}
		appLogger.Printf("Effective concurrency for -hf: %d", effectiveConcurrency)
	} else {
		maxFileConcurrency := 100
		if effectiveConcurrency <= 0 {
			effectiveConcurrency = 3
		}
		if effectiveConcurrency > maxFileConcurrency {
			effectiveConcurrency = maxFileConcurrency
		}
		appLogger.Printf("Effective concurrency for file list/direct URLs: %d", effectiveConcurrency)
	}
	if effectiveConcurrency <= 0 {
		effectiveConcurrency = 1
	}

	manager = NewProgressManager(effectiveConcurrency)
	defer manager.Stop()

	appLogger.Printf("Effective Display Concurrency: %d. DebugMode: %t, FilePath: '%s', HF Repo Input: '%s', ModelName: '%s', SelectMode: %t, Args: %v",
		effectiveConcurrency, debugMode, urlsFilePath, hfRepoInput, modelName, selectFile, downloaderFlags.Args())

	var finalDownloadItems []DownloadItem
	var downloadDir string
	var hfFileSizes map[string]int64

	fmt.Fprintln(os.Stderr, "[INFO] Initializing downloader...")

	if modelName != "" {
		modelURL, found := modelRegistry[modelName]
		if !found {
			appLogger.Printf("Error: Model alias '%s' not recognized.", modelName)
			fmt.Fprintf(os.Stderr, "Error: Model alias '%s' not recognized.\nAvailable model aliases:\n", modelName)
			for alias := range modelRegistry {
				fmt.Fprintf(os.Stderr, "  - %s\n", alias)
			}
			return 1
		}
		appLogger.Printf("Using model alias '%s' for URL: %s", modelName, modelURL)
		parsedURL, parseErr := url.Parse(modelURL)
		var preferredFilename string
		if parseErr == nil {
			preferredFilename = path.Base(parsedURL.Path)
		} else {
			preferredFilename = "downloaded_model.file"
		}
		finalDownloadItems = append(finalDownloadItems, DownloadItem{URL: modelURL, PreferredFilename: preferredFilename})
		safeModelName := strings.ReplaceAll(strings.ReplaceAll(modelName, string(os.PathSeparator), "_"), "..", "")
		downloadDir = filepath.Join("downloads", safeModelName)
	} else if hfRepoInput != "" {
		fmt.Fprintf(os.Stderr, "[INFO] Preparing to fetch from Hugging Face repository: %s\n", hfRepoInput)
		allRepoFiles, errHf := fetchHuggingFaceURLs(hfRepoInput)
		if errHf != nil {
			appLogger.Printf("Error fetching from HF '%s': %v", hfRepoInput, errHf)
			fmt.Fprintf(os.Stderr, "Error fetching from HF '%s': %v\n", hfRepoInput, errHf)
			return 1
		}
		if len(allRepoFiles) == 0 {
			fmt.Fprintf(os.Stderr, "[INFO] No files found in repository %s. Exiting.\n", hfRepoInput)
			return 0
		}

		selectedHfFiles := allRepoFiles
		if selectFile {
			fmt.Fprintln(os.Stderr, "[INFO] -select specified. File selection logic would run here if implemented.")
		}

		for _, hfFile := range selectedHfFiles {
			finalDownloadItems = append(finalDownloadItems, DownloadItem{URL: hfFile.URL, PreferredFilename: hfFile.Filename})
		}
		var repoOwnerClean, repoNameClean string
		cleanedRepoInput := strings.TrimPrefix(hfRepoInput, "https://huggingface.co/")
		cleanedRepoInput = strings.TrimPrefix(cleanedRepoInput, "http://huggingface.co/")
		parts := strings.Split(cleanedRepoInput, "/")
		if len(parts) >= 2 {
			repoOwnerClean = strings.ReplaceAll(strings.ReplaceAll(parts[0], string(os.PathSeparator), "_"), "..", "")
			repoNameClean = strings.ReplaceAll(strings.ReplaceAll(parts[1], string(os.PathSeparator), "_"), "..", "")
			repoNameClean = strings.Split(repoNameClean, "?")[0]
			repoNameClean = strings.Split(repoNameClean, "#")[0]
			downloadDir = filepath.Join("downloads", fmt.Sprintf("%s_%s", repoOwnerClean, repoNameClean))
		} else {
			safeRepoName := strings.ReplaceAll(strings.ReplaceAll(cleanedRepoInput, string(os.PathSeparator), "_"), "..", "")
			downloadDir = filepath.Join("downloads", fmt.Sprintf("hf_%s", safeRepoName))
			appLogger.Printf("Could not parse owner/repo from hf input '%s', using dir %s", hfRepoInput, downloadDir)
		}
	} else {
		if selectFile {
			fmt.Fprintln(os.Stderr, "[WARN] -select flag is ignored when using -f or direct URLs.")
		}
		inputURLs := downloaderFlags.Args()

		if urlsFilePath != "" {
			fmt.Fprintf(os.Stderr, "[INFO] Reading URLs from file: %s\n", urlsFilePath)
			file, ferr := os.Open(urlsFilePath)
			if ferr != nil {
				appLogger.Printf("Error opening URL file '%s': %v", urlsFilePath, ferr)
				fmt.Fprintf(os.Stderr, "Error opening URL file '%s': %v\n", urlsFilePath, ferr)
				return 1
			}
			defer file.Close()
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				urlStr := strings.TrimSpace(scanner.Text())
				if urlStr != "" && !strings.HasPrefix(urlStr, "#") {
					inputURLs = append(inputURLs, urlStr)
				}
			}
			if serr := scanner.Err(); serr != nil {
				appLogger.Printf("Error reading URL file '%s': %v", urlsFilePath, serr)
				fmt.Fprintf(os.Stderr, "Error reading URL file '%s': %v\n", urlsFilePath, serr)
				return 1
			}
		}

		for _, urlStr := range inputURLs {
			finalDownloadItems = append(finalDownloadItems, DownloadItem{URL: urlStr, PreferredFilename: ""})
		}
		appLogger.Printf("Processed %d URLs for download.", len(finalDownloadItems))
		downloadDir = "downloads"
	}

	if len(finalDownloadItems) == 0 {
		appLogger.Println("No URLs to download. Exiting.")
		fmt.Fprintln(os.Stderr, "[INFO] No URLs to download. Exiting.")
		return 0
	}

	if _, statErr := os.Stat(downloadDir); os.IsNotExist(statErr) {
		if mkDirErr := os.MkdirAll(downloadDir, 0755); mkDirErr != nil {
			appLogger.Printf("Error creating base directory '%s': %v", downloadDir, mkDirErr)
			fmt.Fprintf(os.Stderr, "Error creating base directory '%s': %v\n", downloadDir, mkDirErr)
			return 1
		}
	} else if statErr != nil {
		appLogger.Printf("Error checking base directory '%s': %v", downloadDir, statErr)
		fmt.Fprintf(os.Stderr, "Error checking base directory '%s': %v\n", downloadDir, statErr)
		return 1
	}

	fmt.Fprintf(os.Stderr, "[INFO] Pre-scanning %d file(s) for sizes (this may take a moment)...\n", len(finalDownloadItems))
	allPWs := make([]*ProgressWriter, len(finalDownloadItems))
	var preScanWG sync.WaitGroup
	preScanSem := make(chan struct{}, 20)

	for i, item := range finalDownloadItems {
		preScanWG.Add(1)
		go func(idx int, dItem DownloadItem) {
			defer preScanWG.Done()
			preScanSem <- struct{}{}
			defer func() { <-preScanSem }()

			actualFile := generateActualFilename(dItem.URL, dItem.PreferredFilename)
			var initialSize int64 = -1

			if size, ok := hfFileSizes[dItem.URL]; ok && hfFileSizes != nil {
				initialSize = size
				appLogger.Printf("[PreScan] Using size %d for %s from hfFileSizes map", size, dItem.URL)
			}

			if initialSize == -1 {
				client := http.Client{Timeout: 15 * DefaultClientTimeoutMultiplier * time.Second}
				headResp, headErr := client.Head(dItem.URL)
				if headErr == nil {
					defer headResp.Body.Close()
					if headResp.StatusCode == http.StatusOK {
						initialSize = headResp.ContentLength
					} else {
						appLogger.Printf("[PreScan] HEAD request for %s returned status %s", dItem.URL, headResp.Status)
					}
				} else {
					appLogger.Printf("[PreScan] HEAD request failed for %s: %v", dItem.URL, headErr)
				}
			}
			allPWs[idx] = newProgressWriter(idx, dItem.URL, actualFile, initialSize, manager)
			manager.requestRedraw()
		}(i, item)
	}
	preScanWG.Wait()
	appLogger.Println("Pre-scan finished.")
	fmt.Fprintln(os.Stderr, "[INFO] Pre-scan complete.")

	manager.AddInitialDownloads(allPWs)
	if len(finalDownloadItems) > 0 {
		manager.performActualDraw(false)
	}

	appLogger.Printf("Downloading %d file(s) to '%s' (concurrency: %d).", len(finalDownloadItems), downloadDir, effectiveConcurrency)
	fmt.Fprintf(os.Stderr, "[INFO] Starting downloads for %d file(s) to '%s' (concurrency: %d).\n", len(finalDownloadItems), downloadDir, effectiveConcurrency)

	var dlWG sync.WaitGroup
	dlSem := make(chan struct{}, effectiveConcurrency)
	for _, pw := range allPWs {
		if pw == nil {
			appLogger.Printf("Skipping nil ProgressWriter in download loop (should not happen).")
			continue
		}
		dlSem <- struct{}{}
		dlWG.Add(1)
		go func(pWriter *ProgressWriter) {
			defer func() { <-dlSem }()
			downloadFile(pWriter, &dlWG, downloadDir, manager)
		}(pw)
	}
	dlWG.Wait()
	appLogger.Println("All downloads processed.")

	fmt.Fprintf(os.Stderr, "All %d download tasks have been processed.\n", len(finalDownloadItems))
	return 0
}

const DefaultClientTimeoutMultiplier = 1
