// go.beta/main.go
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// --- Main Application ---
func main() {
	var concurrency int
	var urlsFilePath, hfRepoInput string
	var selectFile bool
	flag.BoolVar(&debugMode, "debug", false, "Enable debug logging to log.log")
	flag.IntVar(&concurrency, "c", 3, "Number of concurrent downloads & display lines")
	flag.StringVar(&urlsFilePath, "f", "", "Path to text file containing URLs")
	flag.StringVar(&hfRepoInput, "hf", "", "Hugging Face repository ID (e.g., owner/repo_name) or full URL")
	flag.BoolVar(&selectFile, "select", false, "Allow selecting a specific .gguf file/series if downloading from a Hugging Face repository that is mainly .gguf files")
	flag.Parse()

	initLogging()
	defer func() {
		if logFile != nil {
			appLogger.Println("--- Logging Finished ---")
			logFile.Close()
		}
	}()
	appLogger.Println("Application starting...")

	if (urlsFilePath == "" && hfRepoInput == "") || (urlsFilePath != "" && hfRepoInput != "") {
		appLogger.Println("Error: Provide -f OR -hf.")
		fmt.Fprintln(os.Stderr, "Error: Provide -f OR -hf.")
		flag.Usage()
		os.Exit(1)
	}

	// Apply concurrency caps
	// ... (concurrency logic remains the same) ...
	if hfRepoInput != "" {
		maxHfConcurrency := 4
		if concurrency <= 0 {
			fmt.Fprintf(os.Stderr, "[INFO] Concurrency must be positive. Defaulting to %d for -hf.\n", maxHfConcurrency)
			concurrency = maxHfConcurrency
		} else if concurrency > maxHfConcurrency {
			fmt.Fprintf(os.Stderr, "[INFO] Concurrency for -hf is capped at %d. Using %d.\n", maxHfConcurrency, maxHfConcurrency)
			appLogger.Printf("User specified concurrency %d for -hf, capped to %d.", concurrency, maxHfConcurrency)
			concurrency = maxHfConcurrency
		}
	} else { // -f is used
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

	appLogger.Printf("Effective Concurrency: %d. DebugMode: %t, FilePath: '%s', HF Repo Input: '%s', SelectMode: %t",
		concurrency, debugMode, urlsFilePath, hfRepoInput, selectFile)

	var finalDownloadItems []DownloadItem
	fmt.Fprintln(os.Stderr, "[INFO] Initializing downloader...")

	var hfFileSizes map[string]int64 // map URL to size, populated by early pre-scan if needed

	if hfRepoInput != "" {
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
					earlyPreScanWG.Wait() // Wait for all early scans to complete

					for _, res := range earlyScanResults {
						if res.URL != "" { // Check if it was populated
							hfFileSizes[res.URL] = res.Size
						}
					}
					appLogger.Println("[Main] Early GGUF size scan complete.")
					fmt.Fprintln(os.Stderr, "[INFO] GGUF size scan complete.")
				}
				// ---- END: Early Pre-scan ----

				seriesMap := make(map[string]*GGUFSeriesInfo)
				var standaloneGGUFs []HFFile

				for _, hfFile := range ggufFilesInRepo { // Process only GGUF files for series/standalone logic
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
							allSizesKnownInSeries = false // Mark if any file size is unknown
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
					selectedHfFiles = allRepoFiles // already the default
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
	}

	if len(finalDownloadItems) == 0 {
		appLogger.Println("No URLs to download. Exiting.")
		fmt.Fprintln(os.Stderr, "[INFO] No URLs to download. Exiting.")
		os.Exit(0)
	}

	downloadDir := "downloads"
	if hfRepoInput != "" {
		// ... (downloadDir logic for HF remains the same) ...
		var repoOwner, repoName string
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
			repoOwner = strings.ReplaceAll(strings.ReplaceAll(parts[0], string(os.PathSeparator), "_"), "..", "")
			repoName = strings.ReplaceAll(strings.ReplaceAll(parts[1], string(os.PathSeparator), "_"), "..", "")
			downloadDir = filepath.Join(downloadDir, fmt.Sprintf("%s_%s", repoOwner, repoName))
			appLogger.Printf("[Main] Using HF download base directory: %s", downloadDir)
		} else {
			appLogger.Printf("[Main] Could not determine owner/repo from HF input '%s' for subdir creation, using base '%s'", hfRepoInput, downloadDir)
		}

	}
	if urlsFilePath != "" {
		if _, statErr := os.Stat(downloadDir); os.IsNotExist(statErr) {
			appLogger.Printf("Creating base download dir for file list: %s", downloadDir)
			if mkDirErr := os.MkdirAll(downloadDir, 0755); mkDirErr != nil {
				appLogger.Printf("Error creating base dir '%s': %v", downloadDir, mkDirErr)
				fmt.Fprintf(os.Stderr, "Error creating base dir '%s': %v\n", downloadDir, mkDirErr)
				os.Exit(1)
			}
		}
	}

	manager := NewProgressManager(concurrency)
	fmt.Fprintf(os.Stderr, "[INFO] Pre-scanning %d file(s) for sizes (this may take a moment)...\n", len(finalDownloadItems))
	if len(finalDownloadItems) > 0 {
		manager.performActualDraw(false)
	}

	allPWs := make([]*ProgressWriter, len(finalDownloadItems))
	var preScanWG sync.WaitGroup
	preScanSem := make(chan struct{}, 20)
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

			if hfFileSizes != nil { // Check if early scan map was populated
				if size, ok := hfFileSizes[dItem.URL]; ok {
					initialSize = size
					if size > -1 { // Only log if a valid size was found
						appLogger.Printf("[PreScan:%s] Using early pre-scanned size: %d for '%s'", dItem.URL, initialSize, actualFile)
					} else {
						appLogger.Printf("[PreScan:%s] Early pre-scan for '%s' yielded unknown size, will attempt HEAD.", dItem.URL, actualFile)
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
					}
					if headErr != nil {
						appLogger.Printf("[PreScan:%s] HEAD error for '%s': %v. Size unknown.", dItem.URL, actualFile, headErr)
					} else { // headErr is nil, but status not OK
						appLogger.Printf("[PreScan:%s] HEAD non-OK status for '%s': %s. Size unknown.", dItem.URL, actualFile, statusStr)
					}
					if headResp != nil && headResp.Body != nil {
						headResp.Body.Close()
					}
				}
			}
			allPWs[idx] = newProgressWriter(idx, dItem.URL, actualFile, initialSize, manager)
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
	dlSem := make(chan struct{}, concurrency)
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
	manager.Stop()
	fmt.Fprintf(os.Stderr, "All %d download tasks have been processed.\n", len(finalDownloadItems))
}
