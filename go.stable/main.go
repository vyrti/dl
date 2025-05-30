// go.beta/main.go
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	File    HFFile // Contains URL and Original Filename (Sibling.Rfilename)
	PartNum int
	Size    int64 // Size of this specific part
}
type GGUFSeriesInfo struct {
	BaseName              string // e.g., "BF16/DeepSeek-R1-0528-BF16" (includes path within repo)
	TotalParts            int    // As indicated by the first encountered part's filename
	SeriesKey             string // For map key, e.g., "BF16/DeepSeek-R1-0528-BF16-of-00030"
	FilesWithPart         []GGUFFileWithPartNum
	ActualTotalPartsFound int // Counter for parts actually found
	TotalSize             int64
}

type SelectableGGUFItem struct {
	DisplayName     string   // e.g., "Series: BF16/model (30 parts, 12.34 GB)" or "File: standalone.gguf, 0.01 GB"
	FilesToDownload []HFFile // All HFFile objects for this selection (URL + Original Filename)
	IsSeries        bool
	IsComplete      bool // For series, indicates if all parts were found
}

// Regex to capture GGUF series: (base_name)-(part_num)-of-(total_parts).gguf
// It captures: 1: base_name_with_path, 2: part_num, 3: total_parts
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
var manager *ProgressManager      // Initialized only if downloads are confirmed
var activeHuggingFaceToken string // Stores HF_TOKEN if --token is used

func printUsage() {
	baseCmd := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, "Usage: %s [flags] <URL1> <URL2> ...\n", baseCmd) // For direct downloads

	fmt.Fprintln(os.Stderr, "\nAlternative command structures:")
	// Llama.cpp App Management
	fmt.Fprintln(os.Stderr, "  Manage pre-configured applications (e.g., llama.cpp binaries):")
	fmt.Fprintf(os.Stderr, "    %s install <app_name>\n", baseCmd)
	fmt.Fprintf(os.Stderr, "    %s update <app_name>\n", baseCmd)
	fmt.Fprintf(os.Stderr, "    %s remove <app_name>\n", baseCmd)
	fmt.Fprintln(os.Stderr, "      Available <app_name>:")
	fmt.Fprintln(os.Stderr, "        llama            (Generic CPU build for your OS/Architecture)")
	fmt.Fprintln(os.Stderr, "        llama-win-cuda   (CUDA-enabled build for Windows x64)")
	fmt.Fprintln(os.Stderr, "        llama-mac-arm    (Metal-enabled build for macOS ARM64)")
	fmt.Fprintln(os.Stderr, "        llama-linux-cuda (CUDA-enabled build for Linux, matching your system's CUDA-compatible architecture)")

	// Model Management (Search)
	fmt.Fprintln(os.Stderr, "\n  Manage Hugging Face models:")
	fmt.Fprintf(os.Stderr, "    %s model <subcommand> [options]\n", baseCmd)
	fmt.Fprintln(os.Stderr, "      Subcommands for 'model':")
	fmt.Fprintln(os.Stderr, "        search <query>   Search for models on Hugging Face.")
	fmt.Fprintln(os.Stderr, "          Arguments for 'search':")
	fmt.Fprintln(os.Stderr, "            <query>      The search term for models (e.g., 'bert', 'llama 7b gguf').")

	// Flags
	fmt.Fprintln(os.Stderr, "\nFlags:")
	fmt.Fprintln(os.Stderr, "  For downloader-specific flags (when providing URLs or using -f, -hf, -m):")
	fmt.Fprintf(os.Stderr, "    Run '%s -h' or '%s --help' for a detailed list (e.g., -c, -f, -hf, -m, -select).\n", baseCmd, baseCmd)
	fmt.Fprintln(os.Stderr, "  Common flags applicable in various contexts:")
	fmt.Fprintln(os.Stderr, "    --token          Use HF_TOKEN environment variable for Hugging Face requests.")
	fmt.Fprintln(os.Stderr, "                     (Applies to -hf, -m if model is from HF, and 'model search').")
	fmt.Fprintln(os.Stderr, "    -debug           Enable debug logging to log.log.")
	fmt.Fprintln(os.Stderr, "  Other top-level flags/commands:")
	fmt.Fprintln(os.Stderr, "    --update         Check for and apply application self-updates (use standalone).")
	fmt.Fprintln(os.Stderr, "    -t               Show system hardware information and exit (use standalone).")

	fmt.Fprintln(os.Stderr, "\nExamples:")
	fmt.Fprintf(os.Stderr, "  Download a file directly:\n    %s http://example.com/file.zip\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  Download from a list in a file with concurrency 5:\n    %s -f urls.txt -c 5\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  Download (and select files) from a Hugging Face repo using token:\n    %s -hf TheBloke/Llama-2-7B-GGUF -select --token\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  Install a llama.cpp application:\n    %s install llama-linux-cuda\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  Update an installed llama.cpp application:\n    %s update llama\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  Search for Hugging Face models using a token:\n    %s model search \"llama 7b gguf\" --token\n", baseCmd)
	fmt.Fprintf(os.Stderr, "  Self-update the application:\n    %s --update\n", baseCmd)
}

func main() {
	for _, arg := range os.Args {
		if arg == "-debug" {
			debugMode = true
			break
		}
	}
	initLogging()

	var exitCode int
	defer func() {
		if r := recover(); r != nil {
			if appLogger != nil {
				appLogger.Printf("PANIC encountered: %+v", r)
			} else {
				fmt.Fprintf(os.Stderr, "PANIC encountered before logger initialization: %+v\n", r)
			}
			fmt.Fprintf(os.Stderr, "\n[CRITICAL] Application panicked: %v\n", r)
			if manager != nil { // manager is global
				fmt.Fprintln(os.Stderr, "[INFO] Attempting to restore terminal state due to panic...")
				manager.Stop()
			} else {
				fmt.Print("\033[?25h") // Fallback cursor restoration
			}
			if exitCode == 0 {
				exitCode = 2
			}
		}
		if logFile != nil {
			if appLogger != nil {
				appLogger.Println("--- Main: Logging Finished (deferred close) ---")
			}
			logFile.Close()
		}
		if appLogger != nil {
			appLogger.Printf("Exiting with code %d", exitCode)
		} else {
			fmt.Printf("Exiting with code %d (logger was not available)\n", exitCode)
		}
		os.Exit(exitCode)
	}()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-signalChan
		if appLogger != nil {
			appLogger.Printf("Signal received: %s. Initiating shutdown.", sig)
		}
		fmt.Fprintln(os.Stderr, "\n[INFO] Interrupt signal received. Cleaning up and exiting...")
		if manager != nil { // manager is global
			manager.Stop()
		} else {
			fmt.Print("\033[?25h") // Fallback
		}
		// logFile closed by main defer
		if appLogger != nil {
			appLogger.Println("Exiting due to signal (code 1).")
		}
		os.Exit(1)
	}()

	exitCode = runActual()
}

func fetchSingleFileSize(fileURL string, hfToken string) (int64, error) {
	appLogger.Printf("[fetchSingleFileSize] Getting size for: %s", fileURL)
	client := http.Client{Timeout: 20 * DefaultClientTimeoutMultiplier * time.Second}
	req, err := http.NewRequest("HEAD", fileURL, nil)
	if err != nil {
		return -1, fmt.Errorf("creating HEAD request for %s: %w", fileURL, err)
	}
	if hfToken != "" && strings.Contains(fileURL, "huggingface.co") {
		req.Header.Set("Authorization", "Bearer "+hfToken)
	}
	req.Header.Set("User-Agent", "Go-File-Downloader/1.1 (size-fetch)")

	resp, err := client.Do(req)
	if err != nil {
		// Try GET as fallback for HEAD errors (e.g. timeout, connection refused)
		appLogger.Printf("[fetchSingleFileSize] HEAD request for %s failed (%v), trying GET fallback.", fileURL, err)
		return fetchSingleFileSizeWithGET(fileURL, hfToken)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		appLogger.Printf("[fetchSingleFileSize] HEAD for %s successful, ContentLength: %d", fileURL, resp.ContentLength)
		return resp.ContentLength, nil
	}

	// If HEAD status is not OK (e.g. 403, 404, 302), try GET as it might resolve redirects or work where HEAD doesn't
	appLogger.Printf("[fetchSingleFileSize] HEAD request for %s returned status %s, trying GET fallback.", fileURL, resp.Status)
	return fetchSingleFileSizeWithGET(fileURL, hfToken)
}

func fetchSingleFileSizeWithGET(fileURL string, hfToken string) (int64, error) {
	client := http.Client{Timeout: 20 * DefaultClientTimeoutMultiplier * time.Second}
	getReq, getErr := http.NewRequest("GET", fileURL, nil)
	if getErr != nil {
		return -1, fmt.Errorf("creating GET request for %s (fallback for size): %w", fileURL, getErr)
	}
	if hfToken != "" && strings.Contains(fileURL, "huggingface.co") {
		getReq.Header.Set("Authorization", "Bearer "+hfToken)
	}
	getReq.Header.Set("User-Agent", "Go-File-Downloader/1.1 (size-fetch-get)")
	// Important: We don't want to download the whole file, just get headers.
	// Some servers might not stream without Range, but for ContentLength this should be fine.
	// For very large files, if server streams immediately, this is inefficient.
	// However, client.Do will only read headers first unless we read from Body.
	getResp, getRespErr := client.Do(getReq)
	if getRespErr != nil {
		return -1, fmt.Errorf("GET request for %s (fallback for size) failed: %w", fileURL, getRespErr)
	}
	defer getResp.Body.Close() // Essential to close the body to free resources

	if getResp.StatusCode == http.StatusOK {
		appLogger.Printf("[fetchSingleFileSizeWithGET] GET fallback for %s successful, ContentLength: %d", fileURL, getResp.ContentLength)
		return getResp.ContentLength, nil
	}
	appLogger.Printf("[fetchSingleFileSizeWithGET] GET fallback for %s also failed status %s", fileURL, getResp.Status)
	return -1, fmt.Errorf("GET fallback status %s for %s", getResp.Status, fileURL)
}

func runActual() int {
	generalFlags := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	var useHuggingFaceToken bool
	var localDebugMode bool

	generalFlags.BoolVar(&localDebugMode, "debug", debugMode, "Enable debug logging to log.log")
	generalFlags.BoolVar(&useHuggingFaceToken, "token", false, "Use HF_TOKEN environment variable for Hugging Face requests")
	generalFlags.SetOutput(io.Discard)
	_ = generalFlags.Parse(os.Args[1:])

	if localDebugMode {
		debugMode = true      // This should re-trigger logging if it was off
		if !logFileIsOpen() { // Check if log file needs to be (re)opened
			initLogging() // Re-initialize if debug was enabled late
		}
	}
	if useHuggingFaceToken {
		activeHuggingFaceToken = os.Getenv("HF_TOKEN")
		if activeHuggingFaceToken == "" {
			fmt.Fprintln(os.Stderr, "[WARN] --token specified, but HF_TOKEN environment variable is not set or is empty.")
			appLogger.Println("[Main] --token specified, but HF_TOKEN environment variable not found or empty.")
		} else {
			appLogger.Println("[Main] HF_TOKEN found and will be used for Hugging Face requests.")
		}
	}

	// Handle non-downloader commands first
	if len(os.Args) > 1 {
		command := os.Args[1]
		if !strings.HasPrefix(command, "-") { // Is a command word
			var tempManager *ProgressManager // For install/update commands that need a simple progress bar
			argsWithoutFlags := []string{}
			for _, arg := range os.Args[1:] {
				if arg == "--token" || arg == "-debug" {
					continue
				}
				argsWithoutFlags = append(argsWithoutFlags, arg)
			}

			if len(argsWithoutFlags) > 0 {
				command = argsWithoutFlags[0]
				switch command {
				case "install", "update", "remove":
					var appName string
					if len(argsWithoutFlags) > 1 {
						appName = argsWithoutFlags[1]
					}
					if appName == "" || strings.HasPrefix(appName, "-") {
						fmt.Fprintf(os.Stderr, "Error: Missing or invalid <app_name> for %s command.\n", command)
						printUsage()
						return 1
					}
					if command == "install" || command == "update" {
						tempManager = NewProgressManager(1) // Simple manager for single task
						// Note: tempManager.Stop() will be called if the command completes.
						// If the command panics, the main defer handles cursor restoration.
						if tempManager != nil {
							defer tempManager.Stop()
						}
					}
					switch command {
					case "install":
						HandleInstallLlamaApp(tempManager, appName)
					case "update":
						HandleUpdateLlamaApp(tempManager, appName)
					case "remove":
						HandleRemoveLlamaApp(appName)
					}
					return 0
				case "model":
					if len(argsWithoutFlags) > 1 && argsWithoutFlags[1] == "search" {
						if len(argsWithoutFlags) > 2 {
							HandleModelSearch(strings.Join(argsWithoutFlags[2:], " "), activeHuggingFaceToken)
							return 0
						}
						fmt.Fprintln(os.Stderr, "Error: Missing search query for 'model search'.")
						printUsage()
						return 1
					}
					fmt.Fprintln(os.Stderr, "Error: Invalid subcommand for 'model'.")
					printUsage()
					return 1
				}
			}
		}
	}

	// Downloader-specific flags
	var concurrency int
	var urlsFilePath, hfRepoInput, modelName string
	var selectFile bool
	var showSysInfo bool
	var updateAppSelf bool

	downloaderFlags := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)
	baseCmdName := downloaderFlags.Name() // Store for usage message

	// Re-declare common flags for help message, their values are already processed
	downloaderFlags.BoolVar(&debugMode, "debug", debugMode, "Enable debug logging to log.log")
	downloaderFlags.BoolVar(&useHuggingFaceToken, "token", useHuggingFaceToken, "Use HF_TOKEN environment variable")

	downloaderFlags.BoolVar(&showSysInfo, "t", false, "Show system hardware information and exit")
	downloaderFlags.BoolVar(&updateAppSelf, "update", false, "Check for and apply application self-updates")
	downloaderFlags.IntVar(&concurrency, "c", 3, "Number of concurrent downloads & display lines")
	downloaderFlags.StringVar(&urlsFilePath, "f", "", "Path to text file containing URLs")
	downloaderFlags.StringVar(&hfRepoInput, "hf", "", "Hugging Face repository ID or URL")
	downloaderFlags.StringVar(&modelName, "m", "", "Predefined model alias")
	downloaderFlags.BoolVar(&selectFile, "select", false, "Interactively select GGUF files from Hugging Face repository")

	downloaderFlags.Usage = func() {
		fmt.Fprintf(downloaderFlags.Output(), "Usage: %s [flags] <URL1> <URL2> ...\n", baseCmdName)
		fmt.Fprintln(downloaderFlags.Output(), "\nThis tool supports direct URL downloads, file-based downloads (-f), Hugging Face repository downloads (-hf), and predefined model alias downloads (-m).")
		fmt.Fprintln(downloaderFlags.Output(), "It also provides application and model management commands (e.g., 'install', 'model search'). See general help by running without actionable flags or with incorrect main command structure.")
		fmt.Fprintln(downloaderFlags.Output(), "\nFlags for URL/repository downloading:")
		downloaderFlags.PrintDefaults()
		fmt.Fprintln(downloaderFlags.Output(), "\nExamples for downloader flags:")
		fmt.Fprintf(downloaderFlags.Output(), "  %s http://example.com/file.zip another.com/file.iso\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s -f urls.txt -c 5\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s -hf TheBloke/Llama-2-7B-GGUF\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s -hf Org/Model-Name -select --token\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  %s -m qwen3-8b --token\n", baseCmdName)
		fmt.Fprintln(downloaderFlags.Output(), "\nFor help on application management ('install', 'update', 'remove', 'model search') or general commands ('--update', '-t'):")
		fmt.Fprintf(downloaderFlags.Output(), "  Run '%s' with an invalid command or no command to see the general usage structure.\n", baseCmdName)
		fmt.Fprintf(downloaderFlags.Output(), "  Example: %s model search \"your query\" --token\n", baseCmdName)
	}

	err := downloaderFlags.Parse(os.Args[1:])
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		fmt.Fprintf(os.Stderr, "Error parsing flags: %v\n\n", err)
		downloaderFlags.Usage()
		return 1
	}

	if updateAppSelf {
		HandleUpdate()
		return 0
	} // Simplified for brevity
	if showSysInfo {
		ShowSystemInfo()
		return 0
	} // Simplified

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
		fmt.Fprintln(os.Stderr, "Error: No download mode or URLs provided.")
		printUsage()
		return 1
	}
	if modesSet > 1 {
		fmt.Fprintln(os.Stderr, "Error: Flags -f, -hf, -m, and direct URLs are mutually exclusive.")
		downloaderFlags.Usage()
		return 1
	}

	effectiveConcurrency := concurrency
	if modelName != "" {
		effectiveConcurrency = 1
		appLogger.Printf("Concurrency display overridden to 1 for -m.")
	} else if hfRepoInput != "" {
		maxHfConcurrency := 4
		if selectFile {
			maxHfConcurrency = 10
		} // Allow more for size pre-fetching phase
		if effectiveConcurrency <= 0 || effectiveConcurrency > maxHfConcurrency {
			effectiveConcurrency = maxHfConcurrency
		}
		appLogger.Printf("Effective concurrency for -hf: %d", effectiveConcurrency)
	} else { // File list or direct URLs
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

	appLogger.Printf("Effective Display Concurrency: %d. DebugMode: %t, UseHFToken: %t, FilePath: '%s', HF Repo Input: '%s', ModelName: '%s', SelectMode: %t, Args: %v",
		effectiveConcurrency, debugMode, useHuggingFaceToken, urlsFilePath, hfRepoInput, modelName, selectFile, downloaderFlags.Args())

	var finalDownloadItems []DownloadItem
	var downloadDir string
	hfFileSizes := make(map[string]int64)

	fmt.Fprintln(os.Stderr, "[INFO] Initializing downloader...")

	if modelName != "" {
		modelURL, found := modelRegistry[modelName]
		if !found {
			fmt.Fprintf(os.Stderr, "Error: Model alias '%s' not recognized.\n", modelName)
			return 1
		}
		var preferredFilename string
		if pu, pe := url.Parse(modelURL); pe == nil {
			preferredFilename = path.Base(pu.Path)
		} else {
			preferredFilename = "download.file"
		}
		finalDownloadItems = append(finalDownloadItems, DownloadItem{URL: modelURL, PreferredFilename: preferredFilename})
		safeModelName := strings.ReplaceAll(strings.ReplaceAll(modelName, string(os.PathSeparator), "_"), "..", "")
		downloadDir = filepath.Join("downloads", safeModelName)
	} else if hfRepoInput != "" {
		fmt.Fprintf(os.Stderr, "[INFO] Preparing to fetch from Hugging Face repository: %s\n", hfRepoInput)
		allRepoFilesFromAPI, errHf := fetchHuggingFaceURLs(hfRepoInput, activeHuggingFaceToken)
		if errHf != nil {
			fmt.Fprintf(os.Stderr, "Error fetching from HF '%s': %v\n", hfRepoInput, errHf)
			return 1
		}
		if len(allRepoFilesFromAPI) == 0 {
			return 0
		}

		selectedHfFiles := []HFFile{}
		if selectFile {
			appLogger.Println("[Main] Select mode enabled. Processing GGUF files.")
			fmt.Fprintln(os.Stderr, "[INFO] Identifying GGUF files and series for selection...")

			ggufSeriesMap := make(map[string]*GGUFSeriesInfo)
			standaloneGGUFs := []HFFile{}
			var filesToGetSize []HFFile

			for _, hfFile := range allRepoFilesFromAPI {
				if strings.HasSuffix(strings.ToLower(hfFile.Filename), ".gguf") {
					filesToGetSize = append(filesToGetSize, hfFile)
					matches := ggufSeriesRegex.FindStringSubmatch(hfFile.Filename)
					if len(matches) == 4 {
						baseName := matches[1]
						partNumStr := matches[2]
						totalPartsStr := matches[3]
						partNum, _ := strconv.Atoi(partNumStr)
						totalPartsInName, _ := strconv.Atoi(totalPartsStr)
						seriesKey := fmt.Sprintf("%s-of-%s", baseName, totalPartsStr)
						if _, ok := ggufSeriesMap[seriesKey]; !ok {
							ggufSeriesMap[seriesKey] = &GGUFSeriesInfo{BaseName: baseName, TotalParts: totalPartsInName, SeriesKey: seriesKey}
						}
						seriesInfo := ggufSeriesMap[seriesKey]
						seriesInfo.FilesWithPart = append(seriesInfo.FilesWithPart, GGUFFileWithPartNum{File: hfFile, PartNum: partNum})
					} else {
						standaloneGGUFs = append(standaloneGGUFs, hfFile)
					}
				}
			}
			appLogger.Printf("[Main] Found %d GGUF series groups and %d standalone GGUF files.", len(ggufSeriesMap), len(standaloneGGUFs))

			if len(filesToGetSize) > 0 {
				fmt.Fprintf(os.Stderr, "[INFO] Fetching sizes for %d GGUF file(s) (this may take a moment)...\n", len(filesToGetSize))
				var sizeWG sync.WaitGroup
				sizeSem := make(chan struct{}, effectiveConcurrency) // Use effectiveConcurrency for HEAD/GET requests
				processedCount := 0
				totalToProcess := len(filesToGetSize)
				var mu sync.Mutex

				for _, hfFileToSize := range filesToGetSize {
					sizeWG.Add(1)
					go func(file HFFile) {
						defer sizeWG.Done()
						sizeSem <- struct{}{}
						defer func() { <-sizeSem }()
						size, errSize := fetchSingleFileSize(file.URL, activeHuggingFaceToken) // Changed variable name
						mu.Lock()
						if errSize != nil {
							appLogger.Printf("[SelectSizeFetch] Error getting size for %s: %v", file.Filename, errSize)
							hfFileSizes[file.URL] = -1
						} else {
							hfFileSizes[file.URL] = size
						}
						processedCount++
						fmt.Fprintf(os.Stderr, "\rFetching GGUF sizes: %d/%d complete...", processedCount, totalToProcess)
						mu.Unlock()
					}(hfFileToSize)
				}
				sizeWG.Wait()
				fmt.Fprintln(os.Stderr, "\rFetching GGUF sizes: All complete.               ")
			}

			selectableDisplayItems := []SelectableGGUFItem{}
			for _, seriesInfo := range ggufSeriesMap {
				seriesInfo.TotalSize = 0
				seriesInfo.ActualTotalPartsFound = 0
				validParts := []GGUFFileWithPartNum{}
				for _, partFile := range seriesInfo.FilesWithPart {
					partSize, ok := hfFileSizes[partFile.File.URL]
					if ok && partSize > -1 {
						seriesInfo.TotalSize += partSize
						partFile.Size = partSize
						seriesInfo.ActualTotalPartsFound++
						validParts = append(validParts, partFile)
					} else {
						appLogger.Printf("[SelectBuild] Part %s of series %s has unknown size or fetch error, excluding from total.", partFile.File.Filename, seriesInfo.BaseName)
					}
				}
				seriesInfo.FilesWithPart = validParts
				sort.Slice(seriesInfo.FilesWithPart, func(i, j int) bool { return seriesInfo.FilesWithPart[i].PartNum < seriesInfo.FilesWithPart[j].PartNum })
				filesForThisSeries := []HFFile{}
				for _, p := range seriesInfo.FilesWithPart {
					filesForThisSeries = append(filesForThisSeries, p.File)
				}
				isComplete := seriesInfo.ActualTotalPartsFound == seriesInfo.TotalParts && seriesInfo.TotalParts > 0
				completenessMark := ""
				if seriesInfo.TotalParts > 0 && !isComplete {
					completenessMark = fmt.Sprintf(" (INCOMPLETE: %d/%d parts found)", seriesInfo.ActualTotalPartsFound, seriesInfo.TotalParts)
				} else if seriesInfo.TotalParts == 0 && seriesInfo.ActualTotalPartsFound > 0 {
					completenessMark = " (WARNING: total parts in name is 0)"
				}
				displayName := fmt.Sprintf("Series: %s (%d parts, %s)%s", seriesInfo.BaseName, seriesInfo.ActualTotalPartsFound, formatBytes(seriesInfo.TotalSize), completenessMark)
				selectableDisplayItems = append(selectableDisplayItems, SelectableGGUFItem{DisplayName: displayName, FilesToDownload: filesForThisSeries, IsSeries: true, IsComplete: isComplete || (seriesInfo.TotalParts == 0 && seriesInfo.ActualTotalPartsFound > 0)})
			}
			for _, standaloneFile := range standaloneGGUFs {
				size, ok := hfFileSizes[standaloneFile.URL]
				if !ok || size == -1 {
					size = 0
					appLogger.Printf("[SelectBuild] Standalone GGUF %s has unknown size.", standaloneFile.Filename)
				}
				displayName := fmt.Sprintf("File: %s (%s)", standaloneFile.Filename, formatBytes(size))
				selectableDisplayItems = append(selectableDisplayItems, SelectableGGUFItem{DisplayName: displayName, FilesToDownload: []HFFile{standaloneFile}, IsSeries: false, IsComplete: true})
			}
			sort.Slice(selectableDisplayItems, func(i, j int) bool {
				return selectableDisplayItems[i].DisplayName < selectableDisplayItems[j].DisplayName
			})

			if len(selectableDisplayItems) == 0 {
				fmt.Fprintln(os.Stderr, "[INFO] No GGUF files found in the repository for selection.")
				appLogger.Println("[MainSelect] No GGUF files found for selection. Downloading all files as fallback.")
				selectedHfFiles = allRepoFilesFromAPI
			} else {
				fmt.Fprintln(os.Stderr, "\nAvailable GGUF files/series for download:")
				for i, item := range selectableDisplayItems {
					fmt.Fprintf(os.Stderr, "%3d. %s\n", i+1, item.DisplayName)
				}
				fmt.Fprintln(os.Stderr, "---")
				for {
					fmt.Fprint(os.Stderr, "Enter numbers (e.g., 1,3), 'all' (listed GGUFs), or 'none': ")
					inputReader := bufio.NewReader(os.Stdin)
					userInput, _ := inputReader.ReadString('\n')
					userInput = strings.TrimSpace(strings.ToLower(userInput))
					if userInput == "all" {
						for _, item := range selectableDisplayItems {
							if item.IsSeries && !item.IsComplete {
								fmt.Fprintf(os.Stderr, "[WARN] Skipping incomplete series: %s\n", item.DisplayName)
								appLogger.Printf("[MainSelect] User chose 'all', skipping incomplete series: %s", item.DisplayName)
								continue
							}
							selectedHfFiles = append(selectedHfFiles, item.FilesToDownload...)
						}
						appLogger.Printf("[MainSelect] User chose 'all', selected %d files.", len(selectedHfFiles))
						break
					}
					if userInput == "none" {
						fmt.Fprintln(os.Stderr, "[INFO] No files selected for download.")
						appLogger.Println("[MainSelect] User chose 'none'.")
						break
					}
					parts := strings.Split(userInput, ",")
					tempSelectedFiles := []HFFile{}
					validSelection := true
					if len(parts) == 0 && userInput != "" {
						validSelection = false
					}
					for _, p := range parts {
						trimmedPart := strings.TrimSpace(p)
						if trimmedPart == "" {
							continue
						}
						idx, errConv := strconv.Atoi(trimmedPart)
						if errConv != nil || idx < 1 || idx > len(selectableDisplayItems) {
							fmt.Fprintf(os.Stderr, "[ERROR] Invalid input: '%s'. Please enter numbers from 1 to %d, 'all', or 'none'.\n", p, len(selectableDisplayItems))
							validSelection = false
							break
						}
						selectedItem := selectableDisplayItems[idx-1]
						if selectedItem.IsSeries && !selectedItem.IsComplete {
							fmt.Fprintf(os.Stderr, "[WARN] Selected series '%s' is incomplete. Skipping this item.\n", selectedItem.DisplayName)
							appLogger.Printf("[MainSelect] User selected item %d ('%s') which is an incomplete series. Skipping.", idx, selectedItem.DisplayName)
							continue
						}
						tempSelectedFiles = append(tempSelectedFiles, selectedItem.FilesToDownload...)
					}
					if validSelection {
						selectedHfFiles = tempSelectedFiles
						appLogger.Printf("[MainSelect] User selected items, resulting in %d files for download.", len(selectedHfFiles))
						break
					}
				}
			}
		} else {
			selectedHfFiles = allRepoFilesFromAPI
			appLogger.Println("[Main] Select mode not enabled. Preparing to download all files from HF repo.")
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
			file, ferr := os.Open(urlsFilePath)
			if ferr != nil {
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

	manager = NewProgressManager(effectiveConcurrency)
	defer manager.Stop()

	if _, statErr := os.Stat(downloadDir); os.IsNotExist(statErr) {
		if mkDirErr := os.MkdirAll(downloadDir, 0755); mkDirErr != nil {
			fmt.Fprintf(os.Stderr, "Error creating base directory '%s': %v\n", downloadDir, mkDirErr)
			return 1
		}
	} else if statErr != nil {
		fmt.Fprintf(os.Stderr, "Error checking base directory '%s': %v\n", downloadDir, statErr)
		return 1
	}

	fmt.Fprintf(os.Stderr, "[INFO] Pre-scanning %d file(s) for sizes (this may take a moment)...\n", len(finalDownloadItems))
	allPWs := make([]*ProgressWriter, len(finalDownloadItems))
	var preScanWG sync.WaitGroup
	preScanSem := make(chan struct{}, 10)

	for i, item := range finalDownloadItems {
		preScanWG.Add(1)
		go func(idx int, dItem DownloadItem) {
			defer preScanWG.Done()
			preScanSem <- struct{}{}
			defer func() { <-preScanSem }()
			actualFile := generateActualFilename(dItem.URL, dItem.PreferredFilename)
			initialSize, ok := hfFileSizes[dItem.URL]
			if !ok || initialSize == -1 {
				var fetchErr error
				initialSize, fetchErr = fetchSingleFileSize(dItem.URL, activeHuggingFaceToken)
				if fetchErr != nil {
					appLogger.Printf("[PreScan] Error fetching size for %s: %v. Size will be unknown.", dItem.URL, fetchErr)
					initialSize = -1
				}
			}
			allPWs[idx] = newProgressWriter(idx, dItem.URL, actualFile, initialSize, manager)
		}(i, item)
	}
	preScanWG.Wait()
	fmt.Fprintln(os.Stderr, "[INFO] Pre-scan complete.")

	manager.AddInitialDownloads(allPWs)

	appLogger.Printf("Downloading %d file(s) to '%s' (concurrency: %d).", len(finalDownloadItems), downloadDir, effectiveConcurrency)

	var dlWG sync.WaitGroup
	dlSem := make(chan struct{}, effectiveConcurrency)
	for _, pw := range allPWs {
		if pw == nil {
			continue
		}
		dlSem <- struct{}{}
		dlWG.Add(1)
		go func(pWriter *ProgressWriter) {
			defer func() { <-dlSem }()
			downloadFile(pWriter, &dlWG, downloadDir, manager, activeHuggingFaceToken)
		}(pw)
	}
	dlWG.Wait()
	appLogger.Println("All downloads processed.")
	return 0
}

func logFileIsOpen() bool {
	return logFile != nil
}

const DefaultClientTimeoutMultiplier = 1
