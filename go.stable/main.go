// go.beta/main.go
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal" // Added for signal handling
	"path"      // For path.Base with URLs
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall" // Added for signal types (SIGINT, SIGTERM)
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

// --- Main Application ---
func main() {
	var concurrency int
	var urlsFilePath, hfRepoInput, modelName string // Added modelName
	var selectFile bool

	flag.BoolVar(&debugMode, "debug", false, "Enable debug logging to log.log")
	flag.IntVar(&concurrency, "c", 3, "Number of concurrent downloads & display lines (forced to 1 if -m is used)")
	flag.StringVar(&urlsFilePath, "f", "", "Path to text file containing URLs")
	flag.StringVar(&hfRepoInput, "hf", "", "Hugging Face repository ID (e.g., owner/repo_name) or full URL")
	flag.StringVar(&modelName, "m", "", "Predefined model alias to download (list: qwen3-0.6b, qwen3-1.6b, qwen3-4b, qwen3-16b, qwen3-32b, qwen3-30b-moe, gemma3-27b )") // New flag
	flag.BoolVar(&selectFile, "select", false, "Allow selecting a specific .gguf file/series if downloading from a Hugging Face repository that is mainly .gguf files")
	flag.Parse()

	// Declare manager here so it's in scope for the defer and signal handler
	var manager *ProgressManager

	// Deferred function for panic recovery and final cleanup (like closing log file).
	defer func() {
		if r := recover(); r != nil {
			appLogger.Printf("PANIC encountered: %+v", r) // Log the panic details
			fmt.Fprintf(os.Stderr, "\n[CRITICAL] Application panicked: %v\n", r)
			if manager != nil {
				fmt.Fprintln(os.Stderr, "[INFO] Attempting to restore terminal state due to panic...")
				manager.Stop() // Attempt to stop manager gracefully, which should restore cursor
			}
		}
		if logFile != nil {
			appLogger.Println("--- Main: Logging Finished (deferred close) ---")
			logFile.Close()
		}
	}()

	initLogging()
	appLogger.Println("Application starting...")

	// Setup signal handling for SIGINT (Ctrl+C) and SIGTERM.
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-signalChan
		appLogger.Printf("Signal received: %s. Initiating shutdown.", sig)
		fmt.Fprintln(os.Stderr, "\n[INFO] Interrupt signal received. Cleaning up and exiting...")
		if manager != nil {
			manager.Stop()
		}
		if logFile != nil {
			appLogger.Println("--- Main: Logging Finished (signal handler close) ---")
			logFile.Close()
		}
		appLogger.Println("Exiting due to signal.")
		os.Exit(1)
	}()

	// --- Mode Validation (ensure only one of -f, -hf, -m is used) ---
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

	if modesSet == 0 {
		appLogger.Println("Error: No download mode specified. Provide -f, -hf, or -m.")
		fmt.Fprintln(os.Stderr, "Error: No download mode specified. Provide one of -f (file), -hf (Hugging Face repo), or -m (model alias).")
		flag.Usage()
		os.Exit(1)
	}
	if modesSet > 1 {
		appLogger.Println("Error: Flags -f, -hf, and -m are mutually exclusive.")
		fmt.Fprintln(os.Stderr, "Error: Flags -f, -hf, and -m are mutually exclusive. Please use only one.")
		flag.Usage()
		os.Exit(1)
	}

	// --- Concurrency Settings ---
	if modelName != "" {
		concurrency = 1 // Force concurrency to 1 for single model download
		appLogger.Printf("Concurrency overridden to 1 for -m (single model download). Display lines also effectively 1.")
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
			concurrency = 3
		} else if concurrency > maxFileConcurrency {
			fmt.Fprintf(os.Stderr, "[INFO] Concurrency for -f is capped at %d. Using %d.\n", maxFileConcurrency, maxFileConcurrency)
			appLogger.Printf("User specified concurrency %d for -f, capped to %d.", concurrency, maxFileConcurrency)
			concurrency = maxFileConcurrency
		}
	}
	if concurrency <= 0 {
		appLogger.Printf("Error: Concurrency ended up <= 0 (%d). This shouldn't happen.", concurrency)
		fmt.Fprintf(os.Stderr, "Internal Error: Concurrency value invalid (%d).\n", concurrency)
		os.Exit(1)
	}

	appLogger.Printf("Effective Concurrency (for downloads and display lines): %d. DebugMode: %t, FilePath: '%s', HF Repo Input: '%s', ModelName: '%s', SelectMode: %t",
		concurrency, debugMode, urlsFilePath, hfRepoInput, modelName, selectFile)

	// --- Prepare Download Items and Download Directory ---
	var finalDownloadItems []DownloadItem
	var downloadDir string
	var hfFileSizes map[string]int64 // For -hf with -select, populated by its early pre-scan

	fmt.Fprintln(os.Stderr, "[INFO] Initializing downloader...")

	if modelName != "" {
		modelURL, found := modelRegistry[modelName]
		if !found {
			appLogger.Printf("Error: Model alias '%s' not recognized.", modelName)
			fmt.Fprintf(os.Stderr, "Error: Model alias '%s' not recognized.\nAvailable model aliases:\n", modelName)
			var availableAliases []string
			for k := range modelRegistry {
				availableAliases = append(availableAliases, k)
			}
			sort.Strings(availableAliases) // Sort for consistent output
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
			preferredFilename = path.Base(parsedURL.Path) // e.g., Qwen3-4B-Q4_K_M.gguf
		} else {
			preferredFilename = "downloaded_model.file" // Fallback
			appLogger.Printf("Warning: Could not parse model URL '%s' for filename: %v. Using fallback '%s'", modelURL, err, preferredFilename)
		}

		finalDownloadItems = append(finalDownloadItems, DownloadItem{URL: modelURL, PreferredFilename: preferredFilename})

		// Sanitize modelName for directory creation
		safeModelName := strings.ReplaceAll(strings.ReplaceAll(modelName, string(os.PathSeparator), "_"), "..", "")
		downloadDir = filepath.Join("downloads", safeModelName) // e.g., downloads/qwen3-4b
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

		selectedHfFiles := allRepoFiles // Default to all files

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
				hfFileSizes = make(map[string]int64) // Initialize map for storing sizes

				// ---- START: Early Pre-scan for HF GGUF selection display ----
				if len(ggufFilesInRepo) > 0 {
					fmt.Fprintf(os.Stderr, "[INFO] Scanning %d GGUF file sizes for selection display (this may take a moment)...\n", len(ggufFilesInRepo))
					appLogger.Printf("[Main] Early scanning %d GGUF files for size.", len(ggufFilesInRepo))

					type EarlyScanResult struct {
						URL  string
						Size int64
					}
					earlyScanResults := make([]EarlyScanResult, len(ggufFilesInRepo))
					var earlyPreScanWG sync.WaitGroup
					earlyPreScanSem := make(chan struct{}, 20) // Concurrency for this early scan

					for i, hfFileToScan := range ggufFilesInRepo {
						earlyPreScanWG.Add(1)
						earlyPreScanSem <- struct{}{}
						go func(idx int, fileToScan HFFile) {
							defer func() {
								<-earlyPreScanSem
								earlyPreScanWG.Done()
							}()
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
								}
								appLogger.Printf("[EarlyPreScan:%s] HEAD failed for '%s'. Error: %v, Status: %s", fileToScan.URL, fileToScan.Filename, headErr, statusStr)
							}
							earlyScanResults[idx] = EarlyScanResult{URL: fileToScan.URL, Size: size}
						}(i, hfFileToScan)
					}
					earlyPreScanWG.Wait()

					for _, res := range earlyScanResults {
						if res.URL != "" {
							hfFileSizes[res.URL] = res.Size
						}
					}
					appLogger.Println("[Main] Early GGUF size scan complete.")
					fmt.Fprintln(os.Stderr, "[INFO] GGUF size scan complete.")
				}
				// ---- END: Early Pre-scan ----

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
					if allSizesKnownInSeries {
						sizeGB := float64(totalSeriesSizeBytes) / (1024 * 1024 * 1024)
						sizeStr = fmt.Sprintf(", %.2f GB", sizeGB)
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

				if len(displayableItems) == 0 {
					fmt.Fprintf(os.Stderr, "[INFO] No .gguf files or series found for selection, despite repo being mainly GGUF. Downloading all %d files.\n", len(allRepoFiles))
					appLogger.Println("[Main] No displayable GGUF items, downloading all files.")
					// selectedHfFiles is already allRepoFiles by default
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
			} else if ggufCount > 0 {
				fmt.Fprintf(os.Stderr, "[INFO] -select flag was provided, but the repository is not detected as mainly .gguf files (GGUF files: %d of %d total). Proceeding with all %d files from repository.\n", ggufCount, len(allRepoFiles), len(allRepoFiles))
				appLogger.Printf("[Main] -select active, but not mainly .gguf (gguf: %d, total: %d). All %d files will be downloaded.", ggufCount, len(allRepoFiles), len(allRepoFiles))
			} else {
				fmt.Fprintf(os.Stderr, "[INFO] -select flag was provided, but no .gguf files were found in the repository. Proceeding with all %d files from repository.\n", len(allRepoFiles))
				appLogger.Printf("[Main] -select active, but no .gguf files found. All %d files will be downloaded.", len(allRepoFiles))
			}
		} // End of if selectFile

		for _, hfFile := range selectedHfFiles {
			finalDownloadItems = append(finalDownloadItems, DownloadItem{URL: hfFile.URL, PreferredFilename: hfFile.Filename})
		}

		// Determine downloadDir for -hf
		var repoOwnerClean, repoNameClean string // CORRECTED DECLARATION
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
				appLogger.Printf("[Main] Error parsing full HF URL '%s' for subdir: %v", hfRepoInput, parseErr)
			}
		}
		parts := strings.Split(tempRepoID, "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			repoOwnerClean = strings.ReplaceAll(strings.ReplaceAll(parts[0], string(os.PathSeparator), "_"), "..", "")
			repoNameClean = strings.ReplaceAll(strings.ReplaceAll(parts[1], string(os.PathSeparator), "_"), "..", "")
			downloadDir = filepath.Join("downloads", fmt.Sprintf("%s_%s", repoOwnerClean, repoNameClean))
		} else {
			downloadDir = filepath.Join("downloads", "hf_download") // Fallback if parsing fails
			appLogger.Printf("[Main] Could not determine owner/repo from HF input '%s' for subdir creation, using fallback '%s'", hfRepoInput, downloadDir)
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
		defer file.Close()
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

		downloadDir = "downloads" // Default for -f
		appLogger.Printf("[Main] Download directory set to: %s for file list '%s'", downloadDir, urlsFilePath)
	}

	if len(finalDownloadItems) == 0 {
		appLogger.Println("No URLs to download. Exiting.")
		fmt.Fprintln(os.Stderr, "[INFO] No URLs to download. Exiting.")
		os.Exit(0)
	}

	// --- Create Base Download Directory (applies to all modes) ---
	// downloadFile will create subdirectories specified in PreferredFilename relative to this downloadDir
	if _, statErr := os.Stat(downloadDir); os.IsNotExist(statErr) {
		appLogger.Printf("Creating base download directory: %s", downloadDir)
		if mkDirErr := os.MkdirAll(downloadDir, 0755); mkDirErr != nil {
			appLogger.Printf("Error creating base directory '%s': %v", downloadDir, mkDirErr)
			fmt.Fprintf(os.Stderr, "Error creating base directory '%s': %v\n", downloadDir, mkDirErr)
			os.Exit(1)
		}
	} else if statErr != nil { // Other error stating directory (permissions etc)
		appLogger.Printf("Error stating base directory '%s': %v", downloadDir, statErr)
		fmt.Fprintf(os.Stderr, "Error accessing base directory '%s': %v\n", downloadDir, statErr)
		os.Exit(1)
	}

	// --- Initialize Progress Manager and Start Downloads ---
	manager = NewProgressManager(concurrency) // Uses the finalized concurrency

	fmt.Fprintf(os.Stderr, "[INFO] Pre-scanning %d file(s) for sizes (this may take a moment)...\n", len(finalDownloadItems))
	if len(finalDownloadItems) > 0 {
		manager.performActualDraw(false)
	}

	allPWs := make([]*ProgressWriter, len(finalDownloadItems))
	var preScanWG sync.WaitGroup
	preScanSem := make(chan struct{}, 20) // Concurrency for pre-scan HEAD requests
	for i, item := range finalDownloadItems {
		preScanWG.Add(1)
		preScanSem <- struct{}{}
		go func(idx int, dItem DownloadItem) {
			defer func() {
				<-preScanSem
				preScanWG.Done()
			}()
			actualFile := generateActualFilename(dItem.URL, dItem.PreferredFilename)
			var initialSize int64 = -1

			// Use hfFileSizes if available (populated by -hf with -select's early scan)
			if hfFileSizes != nil {
				if size, ok := hfFileSizes[dItem.URL]; ok {
					initialSize = size
					if size > -1 {
						appLogger.Printf("[PreScan:%s] Using early pre-scanned size: %d for '%s'", dItem.URL, initialSize, actualFile)
					} else {
						appLogger.Printf("[PreScan:%s] Early pre-scan for '%s' yielded unknown size (-1), will attempt HEAD.", dItem.URL, actualFile)
					}
				}
			}

			if initialSize == -1 { // Size not pre-fetched or pre-fetch failed/returned -1
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
						if headResp.Body != nil { // Ensure body is closed on error too
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
			allPWs[idx] = newProgressWriter(idx, dItem.URL, actualFile, initialSize, manager)
			manager.requestRedraw()
		}(i, item)
	}
	preScanWG.Wait()
	close(preScanSem)
	appLogger.Println("Pre-scan finished.")
	fmt.Fprintln(os.Stderr, "[INFO] Pre-scan complete.")
	manager.AddInitialDownloads(allPWs)

	appLogger.Printf("Downloading %d file(s) to '%s' (concurrency: %d).", len(finalDownloadItems), downloadDir, concurrency)
	fmt.Fprintf(os.Stderr, "[INFO] Starting downloads for %d file(s) to '%s' (concurrency: %d).\n", len(finalDownloadItems), downloadDir, concurrency)

	var dlWG sync.WaitGroup
	dlSem := make(chan struct{}, concurrency) // Use the finalized concurrency
	for _, pw := range allPWs {
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
