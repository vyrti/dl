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

func main() {
	var concurrency int
	var urlsFilePath, hfRepoInput, modelName string
	var selectFile bool
	var showSysInfo bool // Flag for -t
	var getLlama bool    // Flag for -getllama
	var updateApp bool   // New flag for --update

	flag.BoolVar(&debugMode, "debug", false, "Enable debug logging to log.log")
	flag.BoolVar(&showSysInfo, "t", false, "Show system hardware information and exit")
	flag.BoolVar(&getLlama, "getllama", false, "Download latest llama.cpp binaries from ggml-org/llama.cpp")
	flag.BoolVar(&updateApp, "update", false, "Check for and apply application updates") // New --update flag
	flag.IntVar(&concurrency, "c", 3, "Number of concurrent downloads & display lines (forced to 1 if -m or -getllama is used, ignored for --update)")
	flag.StringVar(&urlsFilePath, "f", "", "Path to text file containing URLs")
	flag.StringVar(&hfRepoInput, "hf", "", "Hugging Face repository ID (e.g., owner/repo_name) or full URL")
	flag.StringVar(&modelName, "m", "", "Predefined model alias to download (list: qwen3-0.6b, qwen3-1.6b, qwen3-4b, qwen3-16b, qwen3-32b, qwen3-30b-moe, gemma3-27b )")
	flag.BoolVar(&selectFile, "select", false, "Allow selecting a specific .gguf file/series if downloading from a Hugging Face repository that is mainly .gguf files")
	flag.Parse()

	var manager *ProgressManager // Keep for potential cleanup in panic, though updater doesn't use it.
	defer func() {
		if r := recover(); r != nil {
			if appLogger != nil {
				appLogger.Printf("PANIC encountered: %+v", r)
			} else {
				fmt.Fprintf(os.Stderr, "PANIC encountered before logger initialization: %+v\n", r)
			}
			fmt.Fprintf(os.Stderr, "\n[CRITICAL] Application panicked: %v\n", r)
			if manager != nil && !updateApp { // Only stop manager if not in update mode (manager not used by updater)
				fmt.Fprintln(os.Stderr, "[INFO] Attempting to restore terminal state due to panic...")
				manager.Stop()
			}
		}
		if logFile != nil {
			if appLogger != nil {
				appLogger.Println("--- Main: Logging Finished (deferred close) ---")
			}
			logFile.Close()
		}
	}()

	initLogging() // Initialize logging first

	if updateApp {
		// Ensure --update is exclusive or handled first.
		// For simplicity, let's make it exclusive of other primary actions.
		actionFlagsUsed := 0
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "f", "hf", "m", "getllama", "t":
				actionFlagsUsed++
			}
		})
		if actionFlagsUsed > 0 {
			appLogger.Println("Error: --update flag cannot be used with other action flags like -f, -hf, -m, -getllama, or -t.")
			fmt.Fprintln(os.Stderr, "Error: --update flag cannot be used with other action flags (-f, -hf, -m, -getllama, -t).")
			os.Exit(1)
		}
		HandleUpdate() // This function will os.Exit()
		return         // Should not be reached if HandleUpdate exits.
	}

	if showSysInfo {
		// ... (existing -t logic)
		// ...
		if flag.NFlag() > 1 {
			var otherFlags []string
			flag.Visit(func(f *flag.Flag) {
				if f.Name != "t" && f.Name != "debug" && f.Name != "update" {
					otherFlags = append(otherFlags, "-"+f.Name)
				}
			})
			if len(otherFlags) > 0 {
				appLogger.Printf("Error: -t flag cannot be used with other action flags: %s", strings.Join(otherFlags, " "))
				fmt.Fprintf(os.Stderr, "Error: -t flag cannot be used with other action flags: %s\n", strings.Join(otherFlags, " "))
				os.Exit(1)
			}
		}
		appLogger.Println("[Main] System info requested via -t flag. Displaying info and exiting.")
		ShowSystemInfo()
		os.Exit(0)
	}
	appLogger.Println("Application starting...")

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-signalChan
		appLogger.Printf("Signal received: %s. Initiating shutdown.", sig)
		fmt.Fprintln(os.Stderr, "\n[INFO] Interrupt signal received. Cleaning up and exiting...")
		if manager != nil { // manager might not be initialized if signal is very early
			manager.Stop()
		}
		if logFile != nil {
			appLogger.Println("--- Main: Logging Finished (signal handler close) ---")
			logFile.Close()
		}
		appLogger.Println("Exiting due to signal.")
		os.Exit(1)
	}()

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
	if getLlama {
		modesSet++
	}
	// --update and -t are handled above and exit, so they don't count towards these operational modes.

	if modesSet == 0 {
		appLogger.Println("Error: No download mode specified. Provide -f, -hf, -m, or -getllama. Or use --update or -t.")
		fmt.Fprintln(os.Stderr, "Error: No download mode specified. Provide one of -f, -hf, -m, -getllama. Or use --update for self-update, or -t for system info.")
		flag.Usage() // This will now show the --update flag as well.
		os.Exit(1)
	}
	if modesSet > 1 {
		appLogger.Println("Error: Flags -f, -hf, -m, and -getllama are mutually exclusive.")
		fmt.Fprintln(os.Stderr, "Error: Flags -f, -hf, -m, and -getllama are mutually exclusive. Please use only one.")
		flag.Usage()
		os.Exit(1)
	}

	// ... (rest of the concurrency logic, download preparation, and execution)
	// ... (This part is unchanged as --update will have exited already if used)

	if modelName != "" || getLlama {
		concurrency = 1
		if modelName != "" {
			appLogger.Printf("Concurrency overridden to 1 for -m (single model download).")
		} else {
			appLogger.Printf("Concurrency overridden to 1 for -getllama (single llama.cpp binary download).")
		}
	} else if hfRepoInput != "" {
		maxHfConcurrency := 4
		if concurrency <= 0 {
			fmt.Fprintf(os.Stderr, "[INFO] Concurrency must be positive. Defaulting to %d for -hf.\n", maxHfConcurrency)
			concurrency = maxHfConcurrency
		} else if concurrency > maxHfConcurrency {
			fmt.Fprintf(os.Stderr, "[INFO] Concurrency for -hf is capped at %d. Using %d.\n", maxHfConcurrency, maxHfConcurrency)
			appLogger.Printf("User specified concurrency %d for -hf, capped to %d.", concurrency, maxHfConcurrency)
			concurrency = maxHfConcurrency
		}
	} else { // urlsFilePath is used
		maxFileConcurrency := 100
		if concurrency <= 0 {
			fmt.Fprintf(os.Stderr, "[INFO] Concurrency must be positive. Defaulting to 3 for -f.\n")
			concurrency = 3 // Default for -f
		} else if concurrency > maxFileConcurrency {
			fmt.Fprintf(os.Stderr, "[INFO] Concurrency for -f is capped at %d. Using %d.\n", maxFileConcurrency, maxFileConcurrency)
			appLogger.Printf("User specified concurrency %d for -f, capped to %d.", concurrency, maxFileConcurrency)
			concurrency = maxFileConcurrency
		}
	}
	if concurrency <= 0 {
		appLogger.Printf("Error: Concurrency ended up <= 0 (%d). Defaulting to 1.", concurrency)
		fmt.Fprintf(os.Stderr, "Internal Error: Concurrency value invalid (%d). Defaulting to 1.\n", concurrency)
		concurrency = 1
	}

	appLogger.Printf("Effective Concurrency: %d. DebugMode: %t, FilePath: '%s', HF Repo Input: '%s', ModelName: '%s', SelectMode: %t, GetLlama: %t",
		concurrency, debugMode, urlsFilePath, hfRepoInput, modelName, selectFile, getLlama)

	var finalDownloadItems []DownloadItem
	var downloadDir string
	var hfFileSizes map[string]int64 // Used for -hf with -select

	fmt.Fprintln(os.Stderr, "[INFO] Initializing downloader...")

	// ... (The existing logic for -getllama, -m, -hf, -f to populate finalDownloadItems and downloadDir)
	// ... (This part is large and remains mostly unchanged, as it's only reached if --update is not used)
	// ...
	if getLlama {
		appLogger.Println("[Main] -getllama mode selected.")
		fmt.Fprintln(os.Stderr, "[INFO] Fetching llama.cpp release information...")
		selectedItem, tagName, err := HandleGetLlama()
		if err != nil {
			appLogger.Printf("Error in HandleGetLlama: %v", err)
			fmt.Fprintf(os.Stderr, "[ERROR] Could not get llama.cpp release information: %v\n", err)
			os.Exit(1)
		}
		if selectedItem.URL == "" {
			appLogger.Println("[Main] No file selected from llama.cpp releases. Exiting.")
			fmt.Fprintln(os.Stderr, "[INFO] No file selected for download. Exiting.")
			os.Exit(0)
		}

		finalDownloadItems = append(finalDownloadItems, selectedItem)
		safeTagName := strings.ReplaceAll(strings.ReplaceAll(tagName, string(os.PathSeparator), "_"), "..", "")
		safeTagName = strings.ReplaceAll(safeTagName, ":", "_")
		downloadDir = filepath.Join("downloads", "llama.cpp_"+safeTagName)
		appLogger.Printf("[Main] Download directory for llama.cpp set to: %s", downloadDir)

		if selectFile {
			fmt.Fprintln(os.Stderr, "[WARN] -select flag is ignored when using -getllama.")
			appLogger.Printf("[Main] -select flag ignored with -getllama option.")
		}
	} else if modelName != "" {
		modelURL, found := modelRegistry[modelName]
		if !found {
			appLogger.Printf("Error: Model alias '%s' not recognized.", modelName)
			fmt.Fprintf(os.Stderr, "Error: Model alias '%s' not recognized.\nAvailable model aliases:\n", modelName)
			var availableAliases []string
			for k := range modelRegistry {
				availableAliases = append(availableAliases, k)
			}
			sort.Strings(availableAliases)
			for _, alias := range availableAliases {
				fmt.Fprintf(os.Stderr, "  - %s\n", alias)
			}
			os.Exit(1)
		}
		appLogger.Printf("Using model alias '%s' for URL: %s", modelName, modelURL)
		fmt.Fprintf(os.Stderr, "[INFO] Preparing to download model alias: %s\n", modelName)
		fmt.Fprintf(os.Stderr, "[INFO] Model URL: %s\n", modelURL)

		parsedURL, err := url.Parse(modelURL)
		var preferredFilename string
		if err == nil {
			preferredFilename = path.Base(parsedURL.Path)
		} else {
			preferredFilename = "downloaded_model.file"
			appLogger.Printf("Warning: Could not parse model URL '%s' for filename: %v. Using fallback '%s'", modelURL, err, preferredFilename)
		}

		finalDownloadItems = append(finalDownloadItems, DownloadItem{URL: modelURL, PreferredFilename: preferredFilename})

		safeModelName := strings.ReplaceAll(strings.ReplaceAll(modelName, string(os.PathSeparator), "_"), "..", "")
		downloadDir = filepath.Join("downloads", safeModelName)
		appLogger.Printf("[Main] Download directory set to: %s for model '%s'", downloadDir, modelName)

		if selectFile {
			fmt.Fprintln(os.Stderr, "[WARN] -select flag is ignored when using -m to specify a model alias.")
			appLogger.Printf("[Main] -select flag ignored with -m option.")
		}
	} else if hfRepoInput != "" {
		fmt.Fprintf(os.Stderr, "[INFO] Preparing to fetch from Hugging Face repository: %s\n", hfRepoInput)
		allRepoFiles, err := fetchHuggingFaceURLs(hfRepoInput)
		if err != nil {
			appLogger.Printf("Error fetching from HF '%s': %v", hfRepoInput, err)
			fmt.Fprintf(os.Stderr, "Error fetching from HF '%s': %v\n", hfRepoInput, err)
			os.Exit(1)
		}

		if len(allRepoFiles) == 0 {
			appLogger.Println("No files returned from Hugging Face repository.")
			fmt.Fprintln(os.Stderr, "[INFO] No files found in the Hugging Face repository. Exiting.")
			os.Exit(0)
		}

		selectedHfFiles := allRepoFiles

		if selectFile {
			ggufCount := 0
			var ggufFilesInRepo []HFFile
			for _, hfFile := range allRepoFiles {
				if strings.HasSuffix(strings.ToLower(hfFile.Filename), ".gguf") {
					ggufCount++
					ggufFilesInRepo = append(ggufFilesInRepo, hfFile)
				}
			}

			isMainlyGGUF := ggufCount > 0 && (float64(ggufCount)/float64(len(allRepoFiles))) >= 0.40

			if isMainlyGGUF {
				appLogger.Printf("[Main] Repo is mainly .gguf (count: %d/%d). -select processing.", ggufCount, len(allRepoFiles))
				hfFileSizes = make(map[string]int64) // Initialize map

				if len(ggufFilesInRepo) > 0 {
					fmt.Fprintf(os.Stderr, "[INFO] Scanning %d GGUF file sizes for selection display (this may take a moment)...\n", len(ggufFilesInRepo))
					appLogger.Printf("[Main] Early scanning %d GGUF files for size.", len(ggufFilesInRepo))

					type EarlyScanResult struct {
						URL  string
						Size int64
					}
					earlyScanResults := make(chan EarlyScanResult, len(ggufFilesInRepo)) // Buffered channel
					var earlyPreScanWG sync.WaitGroup
					earlyPreScanSem := make(chan struct{}, 20) // Concurrency for HEAD requests

					for _, hfFileToScan := range ggufFilesInRepo {
						earlyPreScanWG.Add(1)
						go func(fileToScan HFFile) {
							defer earlyPreScanWG.Done()
							earlyPreScanSem <- struct{}{}
							defer func() { <-earlyPreScanSem }()

							var size int64 = -1
							headReq, _ := http.NewRequest("HEAD", fileToScan.URL, nil)
							headReq.Header.Set("User-Agent", "Go-File-Downloader/1.1 (EarlyPreScan-HEAD)")
							headClient := http.Client{Timeout: 10 * time.Second}
							headResp, headErr := headClient.Do(headReq)
							if headErr == nil && headResp.StatusCode == http.StatusOK {
								size = headResp.ContentLength
								if headResp.Body != nil {
									headResp.Body.Close()
								}
							} else {
								statusStr := "N/A"
								if headResp != nil {
									statusStr = headResp.Status
									if headResp.Body != nil {
										headResp.Body.Close()
									}
								}
								appLogger.Printf("[EarlyPreScan:%s] HEAD failed for '%s'. Error: %v, Status: %s", fileToScan.URL, fileToScan.Filename, headErr, statusStr)
							}
							earlyScanResults <- EarlyScanResult{URL: fileToScan.URL, Size: size}
						}(hfFileToScan)
					}
					earlyPreScanWG.Wait()
					close(earlyScanResults)

					for res := range earlyScanResults {
						if res.URL != "" { // Ensure URL is not empty before map assignment
							hfFileSizes[res.URL] = res.Size
						}
					}
					appLogger.Println("[Main] Early GGUF size scan complete.")
					fmt.Fprintln(os.Stderr, "[INFO] GGUF size scan complete.")
				}
				// ... (rest of GGUF selection logic, using hfFileSizes)
				seriesMap := make(map[string]*GGUFSeriesInfo)
				var standaloneGGUFs []HFFile

				for _, hfFile := range ggufFilesInRepo {
					matches := ggufSeriesRegex.FindStringSubmatch(hfFile.Filename)
					if matches != nil {
						baseName := matches[1]
						partNumStr := matches[2]
						totalPartsStr := matches[3]
						partNum, pErr := strconv.Atoi(partNumStr)
						totalPartsVal, tErr := strconv.Atoi(totalPartsStr)

						if pErr != nil || tErr != nil {
							appLogger.Printf("[Main] Warning: Could not parse part/total numbers for '%s'. Treating as standalone. pErr: %v, tErr: %v", hfFile.Filename, pErr, tErr)
							standaloneGGUFs = append(standaloneGGUFs, hfFile)
							continue
						}
						seriesKey := fmt.Sprintf("%s-of-%s", baseName, totalPartsStr)
						if _, exists := seriesMap[seriesKey]; !exists {
							seriesMap[seriesKey] = &GGUFSeriesInfo{
								BaseName:      baseName,
								TotalParts:    totalPartsVal,
								SeriesKey:     seriesKey,
								FilesWithPart: []GGUFFileWithPartNum{},
							}
						}
						seriesMap[seriesKey].FilesWithPart = append(seriesMap[seriesKey].FilesWithPart, GGUFFileWithPartNum{File: hfFile, PartNum: partNum})
					} else {
						standaloneGGUFs = append(standaloneGGUFs, hfFile)
					}
				}

				for _, seriesInfo := range seriesMap {
					sort.Slice(seriesInfo.FilesWithPart, func(i, j int) bool {
						return seriesInfo.FilesWithPart[i].PartNum < seriesInfo.FilesWithPart[j].PartNum
					})
				}

				var displayableItems []SelectableGGUFItem
				sortedSeriesKeys := make([]string, 0, len(seriesMap))
				for k := range seriesMap {
					sortedSeriesKeys = append(sortedSeriesKeys, k)
				}
				sort.Strings(sortedSeriesKeys)

				for _, seriesKey := range sortedSeriesKeys {
					seriesInfo := seriesMap[seriesKey]
					if len(seriesInfo.FilesWithPart) == 0 {
						continue
					}
					filesInSeries := make([]HFFile, len(seriesInfo.FilesWithPart))
					var totalSeriesSizeBytes int64 = 0
					allSizesKnownInSeries := true
					for i, fwp := range seriesInfo.FilesWithPart {
						filesInSeries[i] = fwp.File
						size, ok := hfFileSizes[fwp.File.URL]
						if ok && size > -1 {
							totalSeriesSizeBytes += size
						} else {
							allSizesKnownInSeries = false
						}
					}

					sizeStr := ", size unknown"
					if allSizesKnownInSeries && totalSeriesSizeBytes > 0 {
						sizeGB := float64(totalSeriesSizeBytes) / (1024 * 1024 * 1024)
						sizeStr = fmt.Sprintf(", %.2f GB", sizeGB)
					} else if allSizesKnownInSeries && totalSeriesSizeBytes == 0 {
						sizeStr = ", 0.00 GB (or scan failed)"
					}

					displayName := fmt.Sprintf("Series: %s (%d parts%s, e.g., %s)", seriesInfo.BaseName, seriesInfo.TotalParts, sizeStr, filepath.Base(seriesInfo.FilesWithPart[0].File.Filename))
					displayableItems = append(displayableItems, SelectableGGUFItem{
						DisplayName:     displayName,
						FilesToDownload: filesInSeries,
					})
				}

				sort.Slice(standaloneGGUFs, func(i, j int) bool { return standaloneGGUFs[i].Filename < standaloneGGUFs[j].Filename })
				for _, hfFile := range standaloneGGUFs {
					size, ok := hfFileSizes[hfFile.URL]
					sizeStr := ", size unknown"
					if ok && size > -1 {
						sizeGB := float64(size) / (1024 * 1024 * 1024)
						sizeStr = fmt.Sprintf(", %.2f GB", sizeGB)
					}
					displayName := fmt.Sprintf("File: %s%s", hfFile.Filename, sizeStr)
					displayableItems = append(displayableItems, SelectableGGUFItem{
						DisplayName:     displayName,
						FilesToDownload: []HFFile{hfFile},
					})
				}
				// ... (rest of GGUF selection input from user)
				if len(displayableItems) == 0 {
					fmt.Fprintf(os.Stderr, "[INFO] No .gguf files or series found for selection, despite repo being mainly GGUF. Downloading all %d files.\n", len(allRepoFiles))
					appLogger.Println("[Main] No displayable GGUF items, downloading all files.")
				} else if len(displayableItems) == 1 {
					selectedHfFiles = displayableItems[0].FilesToDownload
					fmt.Fprintf(os.Stderr, "[INFO] Auto-selecting the only available GGUF item: %s (%d file(s))\n", displayableItems[0].DisplayName, len(selectedHfFiles))
					appLogger.Printf("[Main] Auto-selected GGUF item: %s, %d files", displayableItems[0].DisplayName, len(selectedHfFiles))
				} else {
					fmt.Fprintln(os.Stderr, "[INFO] Multiple .gguf files/series found. Please select one to download:")
					for i, item := range displayableItems {
						fmt.Fprintf(os.Stderr, "%d: %s (%d file(s))\n", i+1, item.DisplayName, len(item.FilesToDownload))
					}
					fmt.Fprint(os.Stderr, "Enter the number of the item to download: ")

					reader := bufio.NewReader(os.Stdin)
					inputStr, readErr := reader.ReadString('\n')
					if readErr != nil {
						fmt.Fprintln(os.Stderr, "[ERROR] Failed to read selection. Aborting.")
						appLogger.Printf("Error reading user selection: %v", readErr)
						os.Exit(1)
					}
					inputStr = strings.TrimSpace(inputStr)
					selectedIndex, convErr := strconv.Atoi(inputStr)

					if convErr != nil || selectedIndex < 1 || selectedIndex > len(displayableItems) {
						fmt.Fprintln(os.Stderr, "[ERROR] Invalid selection. Aborting.")
						appLogger.Printf("Invalid user selection: input='%s', parsed_idx=%d (valid range 1-%d)", inputStr, selectedIndex, len(displayableItems))
						os.Exit(1)
					}
					selectedHfFiles = displayableItems[selectedIndex-1].FilesToDownload
					appLogger.Printf("[Main] User selected item %d: %s, %d files", selectedIndex, displayableItems[selectedIndex-1].DisplayName, len(selectedHfFiles))
					fmt.Fprintf(os.Stderr, "[INFO] Selected for download: %s (%d file(s))\n", displayableItems[selectedIndex-1].DisplayName, len(selectedHfFiles))
				}

			} else if ggufCount > 0 { // Not mainly GGUF, but some GGUF files exist
				fmt.Fprintf(os.Stderr, "[INFO] -select flag was provided, but the repository is not detected as mainly .gguf files (GGUF files: %d of %d total). Proceeding with all %d files from repository.\n", ggufCount, len(allRepoFiles), len(allRepoFiles))
				appLogger.Printf("[Main] -select active, but not mainly .gguf (gguf: %d, total: %d). All %d files will be downloaded.", ggufCount, len(allRepoFiles), len(allRepoFiles))
			} else { // No GGUF files found
				fmt.Fprintf(os.Stderr, "[INFO] -select flag was provided, but no .gguf files were found in the repository. Proceeding with all %d files from repository.\n", len(allRepoFiles))
				appLogger.Printf("[Main] -select active, but no .gguf files found. All %d files will be downloaded.", len(allRepoFiles))
			}
		} // End of if selectFile

		finalDownloadItems = make([]DownloadItem, 0, len(selectedHfFiles))
		for _, hfFile := range selectedHfFiles {
			finalDownloadItems = append(finalDownloadItems, DownloadItem{URL: hfFile.URL, PreferredFilename: hfFile.Filename})
		}

		var repoOwnerClean, repoNameClean string
		tempRepoID := hfRepoInput
		if strings.HasPrefix(hfRepoInput, "http") {
			parsedHF, parseErr := url.Parse(hfRepoInput)
			if parseErr == nil && parsedHF != nil && parsedHF.Host == "huggingface.co" {
				repoPath := strings.TrimPrefix(parsedHF.Path, "/")
				pathParts := strings.Split(repoPath, "/")
				if len(pathParts) >= 2 {
					tempRepoID = fmt.Sprintf("%s/%s", pathParts[0], pathParts[1])
				}
			} else if parseErr != nil {
				appLogger.Printf("[Main] Error parsing full HF URL '%s' for subdir: %v. Using original input for dir name.", hfRepoInput, parseErr)
			}
		}
		parts := strings.Split(tempRepoID, "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			repoOwnerClean = strings.ReplaceAll(strings.ReplaceAll(parts[0], string(os.PathSeparator), "_"), "..", "")
			repoNameClean = strings.ReplaceAll(strings.ReplaceAll(parts[1], string(os.PathSeparator), "_"), "..", "")
			downloadDir = filepath.Join("downloads", fmt.Sprintf("%s_%s", repoOwnerClean, repoNameClean))
		} else {
			sanitizedInput := strings.ReplaceAll(strings.ReplaceAll(hfRepoInput, "/", "_"), string(os.PathSeparator), "_")
			sanitizedInput = strings.ReplaceAll(sanitizedInput, "..", "")
			downloadDir = filepath.Join("downloads", "hf_"+sanitizedInput)
			appLogger.Printf("[Main] Could not determine owner/repo from HF input '%s' for subdir creation, using fallback dir name based on input: '%s'", hfRepoInput, downloadDir)
		}
		appLogger.Printf("[Main] Download directory set to: %s for HF repo '%s'", downloadDir, hfRepoInput)

	} else { // urlsFilePath is used
		if selectFile {
			fmt.Fprintln(os.Stderr, "[WARN] -select flag is ignored when using -f to specify a URL file.")
			appLogger.Printf("[Main] -select flag ignored with -f option.")
		}
		fmt.Fprintf(os.Stderr, "[INFO] Reading URLs from file: %s\n", urlsFilePath)
		file, ferr := os.Open(urlsFilePath)
		if ferr != nil {
			appLogger.Printf("Error opening '%s': %v", urlsFilePath, ferr)
			fmt.Fprintf(os.Stderr, "Error opening '%s': %v\n", urlsFilePath, ferr)
			os.Exit(1)
		}
		defer file.Close() // file will be closed when main exits
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			urlStr := strings.TrimSpace(scanner.Text())
			if urlStr != "" {
				finalDownloadItems = append(finalDownloadItems, DownloadItem{URL: urlStr, PreferredFilename: ""})
			}
		}
		if serr := scanner.Err(); serr != nil {
			appLogger.Printf("Error reading '%s': %v", urlsFilePath, serr)
			fmt.Fprintf(os.Stderr, "Error reading '%s': %v\n", urlsFilePath, serr)
			os.Exit(1)
		}
		appLogger.Printf("Read %d URLs from '%s'.", len(finalDownloadItems), urlsFilePath)
		fmt.Fprintf(os.Stderr, "[INFO] Read %d URLs from '%s'.\n", len(finalDownloadItems), urlsFilePath)

		downloadDir = "downloads" // Default download directory for -f mode
		appLogger.Printf("[Main] Download directory set to: %s for file list '%s'", downloadDir, urlsFilePath)
	}

	if len(finalDownloadItems) == 0 {
		appLogger.Println("No URLs to download. Exiting.")
		fmt.Fprintln(os.Stderr, "[INFO] No URLs to download. Exiting.")
		os.Exit(0)
	}

	if _, statErr := os.Stat(downloadDir); os.IsNotExist(statErr) {
		appLogger.Printf("Creating base download directory: %s", downloadDir)
		if mkDirErr := os.MkdirAll(downloadDir, 0755); mkDirErr != nil {
			appLogger.Printf("Error creating base directory '%s': %v", downloadDir, mkDirErr)
			fmt.Fprintf(os.Stderr, "Error creating base directory '%s': %v\n", downloadDir, mkDirErr)
			os.Exit(1)
		}
	} else if statErr != nil {
		appLogger.Printf("Error stating base directory '%s': %v", downloadDir, statErr)
		fmt.Fprintf(os.Stderr, "Error accessing base directory '%s': %v\n", downloadDir, statErr)
		os.Exit(1)
	}

	// Initialize ProgressManager only if not in update mode.
	manager = NewProgressManager(concurrency)

	fmt.Fprintf(os.Stderr, "[INFO] Pre-scanning %d file(s) for sizes (this may take a moment)...\n", len(finalDownloadItems))
	allPWs := make([]*ProgressWriter, len(finalDownloadItems))
	var preScanWG sync.WaitGroup
	// Use a buffered channel for preScanSem for HEAD requests, similar to the GGUF early scan
	preScanSem := make(chan struct{}, 20)

	for i, item := range finalDownloadItems {
		preScanWG.Add(1)
		go func(idx int, dItem DownloadItem) { // Pass item by value
			defer preScanWG.Done()
			preScanSem <- struct{}{}
			defer func() { <-preScanSem }()

			actualFile := generateActualFilename(dItem.URL, dItem.PreferredFilename)
			var initialSize int64 = -1

			// Check if size was pre-fetched by -hf -select logic
			if hfFileSizes != nil { // Check if map is initialized
				if size, ok := hfFileSizes[dItem.URL]; ok {
					initialSize = size
					if size > -1 {
						appLogger.Printf("[PreScan:%s] Using early pre-scanned size: %d for '%s'", dItem.URL, initialSize, actualFile)
					} else {
						appLogger.Printf("[PreScan:%s] Early pre-scan for '%s' yielded unknown size (-1), will attempt HEAD.", dItem.URL, actualFile)
					}
				}
			}

			// If not pre-scanned or pre-scan failed, try HEAD request
			if initialSize == -1 {
				headReq, _ := http.NewRequest("HEAD", dItem.URL, nil)
				headReq.Header.Set("User-Agent", "Go-File-Downloader/1.1 (PreScan-HEAD)")
				headClient := http.Client{Timeout: 10 * time.Second}
				headResp, headErr := headClient.Do(headReq)
				if headErr == nil && headResp.StatusCode == http.StatusOK {
					initialSize = headResp.ContentLength
					if headResp.Body != nil {
						headResp.Body.Close()
					}
					appLogger.Printf("[PreScan:%s] HEAD success. Size: %d for '%s'", dItem.URL, initialSize, actualFile)
				} else {
					statusStr := "N/A"
					if headResp != nil {
						statusStr = headResp.Status
						if headResp.Body != nil {
							headResp.Body.Close()
						}
					}
					if headErr != nil {
						appLogger.Printf("[PreScan:%s] HEAD error for '%s': %v. Size unknown.", dItem.URL, actualFile, headErr)
					} else {
						appLogger.Printf("[PreScan:%s] HEAD non-OK status for '%s': %s. Size unknown.", dItem.URL, actualFile, statusStr)
					}
				}
			}
			// Ensure manager is not nil before creating ProgressWriter
			if manager != nil {
				allPWs[idx] = newProgressWriter(idx, dItem.URL, actualFile, initialSize, manager)
				manager.requestRedraw() // Request redraw after each prescan result potentially
			} else {
				// This case should ideally not happen if --update is handled correctly and exits.
				// Or, if manager initialization failed, but that would be a different problem.
				appLogger.Printf("[PreScan:%s] Error: ProgressManager is nil. Cannot create progress writer for %s.", dItem.URL, actualFile)
			}
		}(i, item)
	}
	preScanWG.Wait()
	appLogger.Println("Pre-scan finished.")
	fmt.Fprintln(os.Stderr, "[INFO] Pre-scan complete.")

	if manager != nil {
		manager.AddInitialDownloads(allPWs) // Add all prescan-populated PWs at once
		if len(finalDownloadItems) > 0 {    // Perform an initial draw if there are items
			manager.performActualDraw(false)
		}
	}

	appLogger.Printf("Downloading %d file(s) to '%s' (concurrency: %d).", len(finalDownloadItems), downloadDir, concurrency)
	fmt.Fprintf(os.Stderr, "[INFO] Starting downloads for %d file(s) to '%s' (concurrency: %d).\n", len(finalDownloadItems), downloadDir, concurrency)

	var dlWG sync.WaitGroup
	dlSem := make(chan struct{}, concurrency)
	for _, pw := range allPWs {
		if pw == nil { // Safety check if any preScan failed to create a PW
			appLogger.Printf("[MainLoop] Skipping nil ProgressWriter.")
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

	if manager != nil {
		manager.Stop()
	}
	fmt.Fprintf(os.Stderr, "All %d download tasks have been processed.\n", len(finalDownloadItems))
}
