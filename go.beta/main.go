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
	"regexp" // For GGUF series parsing
	"sort"   // For sorting displayable items
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
	DisplayName     string   // e.g., "Series: BF16/model (30 parts)" or "File: standalone.gguf"
	FilesToDownload []HFFile // All HFFile objects for this selection
}

// Regex to capture GGUF series: (base_name)-(part_num)-of-(total_parts).gguf
// Base_name can include paths and hyphens.
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
			for _, hfFile := range allRepoFiles {
				if strings.HasSuffix(strings.ToLower(hfFile.Filename), ".gguf") {
					ggufCount++
				}
			}

			// Condition for "mainly .gguf": at least one .gguf file, and .gguf files constitute >= 40% of all files.
			isMainlyGGUF := ggufCount > 0 && (float64(ggufCount)/float64(len(allRepoFiles))) >= 0.40

			if isMainlyGGUF {
				appLogger.Printf("[Main] Repo is mainly .gguf (count: %d/%d). -select processing.", ggufCount, len(allRepoFiles))
				seriesMap := make(map[string]*GGUFSeriesInfo)
				var standaloneGGUFs []HFFile

				for _, hfFile := range allRepoFiles {
					if !strings.HasSuffix(strings.ToLower(hfFile.Filename), ".gguf") {
						continue // Only consider GGUF files for selection logic
					}
					// hfFile.Filename can be "path/to/model-00001-of-00010.gguf"
					matches := ggufSeriesRegex.FindStringSubmatch(hfFile.Filename)
					if matches != nil {
						baseName := matches[1]      // e.g., "path/to/model"
						partNumStr := matches[2]    // e.g., "00001"
						totalPartsStr := matches[3] // e.g., "00010"
						partNum, pErr := strconv.Atoi(partNumStr)
						totalPartsVal, tErr := strconv.Atoi(totalPartsStr)

						if pErr != nil || tErr != nil {
							appLogger.Printf("[Main] Warning: Could not parse part/total numbers for '%s'. Treating as standalone. pErr: %v, tErr: %v", hfFile.Filename, pErr, tErr)
							standaloneGGUFs = append(standaloneGGUFs, hfFile)
							continue
						}

						seriesKey := fmt.Sprintf("%s-of-%s", baseName, totalPartsStr) // Unique key for this series

						if _, exists := seriesMap[seriesKey]; !exists {
							seriesMap[seriesKey] = &GGUFSeriesInfo{
								BaseName:      baseName, // This includes the path part from Rfilename
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

				// Sort files within each series by part number
				for _, seriesInfo := range seriesMap {
					sort.Slice(seriesInfo.FilesWithPart, func(i, j int) bool {
						return seriesInfo.FilesWithPart[i].PartNum < seriesInfo.FilesWithPart[j].PartNum
					})
				}

				var displayableItems []SelectableGGUFItem

				// Add series to displayableItems, sorted by series key for consistent order
				sortedSeriesKeys := make([]string, 0, len(seriesMap))
				for k := range seriesMap {
					sortedSeriesKeys = append(sortedSeriesKeys, k)
				}
				sort.Strings(sortedSeriesKeys)

				for _, seriesKey := range sortedSeriesKeys {
					seriesInfo := seriesMap[seriesKey]
					if len(seriesInfo.FilesWithPart) == 0 { // Should not happen if map entry was created
						continue
					}
					filesInSeries := make([]HFFile, len(seriesInfo.FilesWithPart))
					for i, fwp := range seriesInfo.FilesWithPart {
						filesInSeries[i] = fwp.File
					}
					// Display name shows base name (which includes path) and example of first file's base name
					displayName := fmt.Sprintf("Series: %s (%d parts, e.g., %s)", seriesInfo.BaseName, seriesInfo.TotalParts, filepath.Base(seriesInfo.FilesWithPart[0].File.Filename))
					displayableItems = append(displayableItems, SelectableGGUFItem{
						DisplayName:     displayName,
						FilesToDownload: filesInSeries,
					})
				}

				// Add standalone GGUFs, sorted by filename
				sort.Slice(standaloneGGUFs, func(i, j int) bool {
					return standaloneGGUFs[i].Filename < standaloneGGUFs[j].Filename
				})
				for _, hfFile := range standaloneGGUFs {
					displayName := fmt.Sprintf("File: %s", hfFile.Filename) // Full path as in repo
					displayableItems = append(displayableItems, SelectableGGUFItem{
						DisplayName:     displayName,
						FilesToDownload: []HFFile{hfFile},
					})
				}

				if len(displayableItems) == 0 {
					fmt.Fprintf(os.Stderr, "[INFO] No .gguf files or series found for selection, despite repo being mainly GGUF. Downloading all %d files.\n", len(allRepoFiles))
					appLogger.Println("[Main] No displayable GGUF items, downloading all files.")
					// selectedHfFiles remains allRepoFiles by default
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
			} else if ggufCount > 0 { // -select was true, but not mainly GGUF (but some GGUFs exist)
				fmt.Fprintf(os.Stderr, "[INFO] -select flag was provided, but the repository is not detected as mainly .gguf files (GGUF files: %d of %d total). Proceeding with all %d files from repository.\n", ggufCount, len(allRepoFiles), len(allRepoFiles))
				appLogger.Printf("[Main] -select active, but not mainly .gguf (gguf: %d, total: %d). All %d files will be downloaded.", ggufCount, len(allRepoFiles), len(allRepoFiles))
				// selectedHfFiles remains allRepoFiles by default
			} else { // -select was true, but no GGUF files found at all
				fmt.Fprintf(os.Stderr, "[INFO] -select flag was provided, but no .gguf files were found in the repository. Proceeding with all %d files from repository.\n", len(allRepoFiles))
				appLogger.Printf("[Main] -select active, but no .gguf files found. All %d files will be downloaded.", len(allRepoFiles))
				// selectedHfFiles remains allRepoFiles by default
			}
		} // End of if selectFile

		for _, hfFile := range selectedHfFiles {
			// hfFile.Filename here can be "subdir/file.gguf" or just "file.gguf"
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

	// Determine base download directory
	downloadDir := "downloads"
	if hfRepoInput != "" {
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
	// Base directory structure (e.g., "downloads" or "downloads/owner_repo")
	// Individual file subdirectories (like "BF16/") will be handled by generateActualFilename and mkdirAll in downloadFile
	// Ensure the top-level 'downloads' dir exists if it's the root for file list downloads
	if urlsFilePath != "" { // For -f, downloadDir is just "downloads"
		if _, statErr := os.Stat(downloadDir); os.IsNotExist(statErr) {
			appLogger.Printf("Creating base download dir for file list: %s", downloadDir)
			if mkDirErr := os.MkdirAll(downloadDir, 0755); mkDirErr != nil {
				appLogger.Printf("Error creating base dir '%s': %v", downloadDir, mkDirErr)
				fmt.Fprintf(os.Stderr, "Error creating base dir '%s': %v\n", downloadDir, mkDirErr)
				os.Exit(1)
			}
		}
	}
	// For -hf, downloadDir (e.g. "downloads/owner_repo") will be created by MkdirAll in downloadFile if needed.

	manager := NewProgressManager(concurrency)
	fmt.Fprintf(os.Stderr, "[INFO] Pre-scanning %d file(s) for sizes (this may take a moment)...\n", len(finalDownloadItems))
	if len(finalDownloadItems) > 0 {
		manager.performActualDraw(false) // Initial draw before pre-scan starts
	}

	allPWs := make([]*ProgressWriter, len(finalDownloadItems))
	var preScanWG sync.WaitGroup
	preScanSem := make(chan struct{}, 20) // Prescan concurrency
	for i, item := range finalDownloadItems {
		preScanWG.Add(1)
		preScanSem <- struct{}{}
		go func(idx int, dItem DownloadItem) {
			defer func() {
				<-preScanSem
				preScanWG.Done()
			}()
			// dItem.PreferredFilename can be "subdir/file.gguf" or ""
			// generateActualFilename will handle this, potentially preserving subdirs relative to downloadDir
			actualFile := generateActualFilename(dItem.URL, dItem.PreferredFilename)
			var initialSize int64 = -1
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
				if headErr != nil {
					appLogger.Printf("[PreScan:%s] HEAD error: %v for '%s'. Size unknown.", dItem.URL, headErr, actualFile)
				} else if headResp != nil {
					appLogger.Printf("[PreScan:%s] HEAD non-OK status: %s for '%s'. Size unknown.", dItem.URL, headResp.Status, actualFile)
					if headResp.Body != nil {
						headResp.Body.Close()
					}
				} else {
					appLogger.Printf("[PreScan:%s] HEAD error (no response, no error reported): for '%s'. Size unknown.", dItem.URL, actualFile)
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
	dlSem := make(chan struct{}, concurrency) // Download concurrency
	for _, pw := range allPWs {
		dlSem <- struct{}{} // Acquire a slot
		dlWG.Add(1)
		go func(pWriter *ProgressWriter) {
			defer func() { <-dlSem }()                         // Release slot when done
			downloadFile(pWriter, &dlWG, downloadDir, manager) // downloadDir is the base path
		}(pw)
	}
	dlWG.Wait()
	appLogger.Println("All downloads processed.")
	manager.Stop()
	fmt.Fprintf(os.Stderr, "All %d download tasks have been processed.\n", len(finalDownloadItems))
}
