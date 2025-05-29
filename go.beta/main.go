// go.beta/main.go
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/http" // Required for PreScan http.NewRequest
	"net/url"
	"os"
	"path/filepath"
	"strconv" // Required for Atoi
	"strings"
	"sync"
	"time" // Required for PreScan http.Client Timeout
)

// DownloadItem represents a file to be downloaded.
type DownloadItem struct {
	URL               string
	PreferredFilename string // Optional, from HF's rfilename or similar context
}

// --- Main Application ---
func main() {
	var concurrency int
	var urlsFilePath, hfRepoInput string
	var selectFile bool // New flag for selecting a file
	// debugMode is declared in downloader.go, flags will set it.
	flag.BoolVar(&debugMode, "debug", false, "Enable debug logging to log.log")
	flag.IntVar(&concurrency, "c", 3, "Number of concurrent downloads & display lines")
	flag.StringVar(&urlsFilePath, "f", "", "Path to text file containing URLs")
	flag.StringVar(&hfRepoInput, "hf", "", "Hugging Face repository ID (e.g., owner/repo_name) or full URL")
	flag.BoolVar(&selectFile, "select", false, "Allow selecting a specific .gguf file if downloading from a Hugging Face repository that is mainly .gguf files")
	flag.Parse()

	initLogging() // Call from downloader.go
	defer func() {
		if logFile != nil { // logFile is from downloader.go
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
		hfFileInfos, err := fetchHuggingFaceURLs(hfRepoInput) // Returns []HFFile
		if err != nil {
			appLogger.Printf("Error fetching from HF '%s': %v", hfRepoInput, err)
			fmt.Fprintf(os.Stderr, "Error fetching from HF '%s': %v\n", hfRepoInput, err)
			os.Exit(1)
		}

		if len(hfFileInfos) == 0 {
			appLogger.Println("No files returned from Hugging Face repository.")
			fmt.Fprintln(os.Stderr, "[INFO] No files found in the Hugging Face repository. Exiting.")
			os.Exit(0)
		}

		selectedHfFiles := hfFileInfos // By default, all files

		if selectFile {
			ggufCount := 0
			var ggufSpecificFiles []HFFile
			for _, hfFile := range hfFileInfos {
				if strings.HasSuffix(strings.ToLower(hfFile.Filename), ".gguf") {
					ggufCount++
					ggufSpecificFiles = append(ggufSpecificFiles, hfFile)
				}
			}

			// Condition for "mainly .gguf": at least one .gguf file, and .gguf files constitute >= 40% of all files.
			isMainlyGGUF := ggufCount > 0 && (float64(ggufCount)/float64(len(hfFileInfos))) >= 0.4

			if isMainlyGGUF {
				appLogger.Printf("[Main] Repository detected as mainly .gguf files (gguf: %d of %d total). -select is active.", ggufCount, len(hfFileInfos))
				if len(ggufSpecificFiles) == 1 {
					fmt.Fprintf(os.Stderr, "[INFO] Auto-selecting the only .gguf file: %s\n", ggufSpecificFiles[0].Filename)
					selectedHfFiles = ggufSpecificFiles
					appLogger.Printf("[Main] Auto-selected the only .gguf file: %s", ggufSpecificFiles[0].Filename)
				} else { // Multiple .gguf files to choose from
					fmt.Fprintln(os.Stderr, "[INFO] Multiple .gguf files found. Please select one to download:")
					for i, gf := range ggufSpecificFiles {
						fmt.Fprintf(os.Stderr, "%d: %s\n", i+1, gf.Filename)
					}
					fmt.Fprint(os.Stderr, "Enter the number of the .gguf file to download: ")

					reader := bufio.NewReader(os.Stdin)
					inputStr, readErr := reader.ReadString('\n')
					if readErr != nil {
						fmt.Fprintln(os.Stderr, "[ERROR] Failed to read selection. Aborting.")
						appLogger.Printf("Error reading user selection: %v", readErr)
						os.Exit(1)
					}
					inputStr = strings.TrimSpace(inputStr)
					selectedIndex, convErr := strconv.Atoi(inputStr)

					if convErr != nil || selectedIndex < 1 || selectedIndex > len(ggufSpecificFiles) {
						fmt.Fprintln(os.Stderr, "[ERROR] Invalid selection. Aborting.")
						appLogger.Printf("Invalid user selection: input='%s', parsed_idx=%d (valid range 1-%d)", inputStr, selectedIndex, len(ggufSpecificFiles))
						os.Exit(1)
					}
					selectedHfFiles = []HFFile{ggufSpecificFiles[selectedIndex-1]}
					appLogger.Printf("[Main] User selected file: %s", selectedHfFiles[0].Filename)
					fmt.Fprintf(os.Stderr, "[INFO] Selected for download: %s\n", selectedHfFiles[0].Filename)
				}
			} else if selectFile { // -select was true, but conditions for selection (mainly GGUF) not met
				fmt.Fprintf(os.Stderr, "[INFO] -select flag was provided, but the repository is not detected as mainly .gguf files (GGUF files: %d of %d total). Proceeding with all %d files from repository.\n", ggufCount, len(hfFileInfos), len(hfFileInfos))
				appLogger.Printf("[Main] -select active, but not mainly .gguf (gguf: %d, total: %d). All %d files will be downloaded.", ggufCount, len(hfFileInfos), len(hfFileInfos))
				// selectedHfFiles remains hfFileInfos (all files)
			}
			// If !selectFile, selectedHfFiles remains hfFileInfos (all files) anyway.
		}

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
		lineNum := 0
		for scanner.Scan() {
			lineNum++
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
		var repoOwner, repoName string
		if strings.Contains(hfRepoInput, "/") {
			tempRepoID := hfRepoInput
			if strings.HasPrefix(hfRepoInput, "http") {
				parsedHF, parseErr := url.Parse(hfRepoInput)
				if parseErr == nil && parsedHF != nil {
					repoPath := strings.TrimPrefix(parsedHF.Path, "/")
					pathParts := strings.Split(repoPath, "/")
					if len(pathParts) >= 2 {
						tempRepoID = fmt.Sprintf("%s/%s", pathParts[0], pathParts[1])
					}
				} else if parseErr != nil {
					appLogger.Printf("[Main] Error parsing full HF URL for subdir: %v", parseErr)
				}
			}
			parts := strings.Split(tempRepoID, "/")
			if len(parts) == 2 {
				repoOwner = parts[0]
				repoName = parts[1]
			}
		}

		if repoOwner != "" && repoName != "" {
			repoSubDir := strings.ReplaceAll(repoOwner+"_"+repoName, "..", "") // Basic sanitization
			repoSubDir = strings.ReplaceAll(repoSubDir, string(os.PathSeparator), "_")
			repoSubDir = strings.ReplaceAll(repoSubDir, ":", "_")
			downloadDir = filepath.Join(downloadDir, repoSubDir)
			appLogger.Printf("[Main] Using HF download subdir: %s", downloadDir)
		} else {
			appLogger.Printf("[Main] Could not determine owner/repo from HF input '%s' for subdir creation, using default 'downloads'", hfRepoInput)
		}
	}

	if _, statErr := os.Stat(downloadDir); os.IsNotExist(statErr) {
		appLogger.Printf("Creating download dir: %s", downloadDir)
		if mkDirErr := os.MkdirAll(downloadDir, 0755); mkDirErr != nil {
			appLogger.Printf("Error creating dir '%s': %v", downloadDir, mkDirErr)
			fmt.Fprintf(os.Stderr, "Error creating dir '%s': %v\n", downloadDir, mkDirErr)
			os.Exit(1)
		}
	}

	manager := NewProgressManager(concurrency) // From downloader.go
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
			actualFile := generateActualFilename(dItem.URL, dItem.PreferredFilename)  // Pass PreferredFilename
			var initialSize int64 = -1                                                // Default to unknown size
			headReq, _ := http.NewRequest("HEAD", dItem.URL, nil)                     // Use dItem.URL
			headReq.Header.Set("User-Agent", "Go-File-Downloader/1.1 (PreScan-HEAD)") // Updated User-Agent
			headClient := http.Client{Timeout: 10 * time.Second}
			headResp, headErr := headClient.Do(headReq)
			if headErr == nil && headResp.StatusCode == http.StatusOK {
				initialSize = headResp.ContentLength
				if headResp.Body != nil {
					headResp.Body.Close()
				}
				appLogger.Printf("[PreScan:%s] HEAD success. Size: %d for %s", dItem.URL, initialSize, actualFile)
			} else {
				if headErr != nil {
					appLogger.Printf("[PreScan:%s] HEAD error: %v for %s. Size unknown.", dItem.URL, headErr, actualFile)
				} else if headResp != nil { // headErr is nil, but status is not OK
					appLogger.Printf("[PreScan:%s] HEAD non-OK status: %s for %s. Size unknown.", dItem.URL, headResp.Status, actualFile)
					if headResp.Body != nil {
						headResp.Body.Close()
					}
				} else { // Should ideally not happen if headErr is nil
					appLogger.Printf("[PreScan:%s] HEAD error (no response, but no error reported): for %s. Size unknown.", dItem.URL, actualFile)
				}
			}
			allPWs[idx] = newProgressWriter(idx, dItem.URL, actualFile, initialSize, manager) // From downloader.go
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
			downloadFile(pWriter, &dlWG, downloadDir, manager) // From downloader.go
		}(pw)
	}
	dlWG.Wait() // Wait for all download goroutines to finish
	appLogger.Println("All downloads processed.")
	manager.Stop() // Gracefully stop the progress manager and redraw loop
	fmt.Fprintf(os.Stderr, "All %d download tasks have been processed.\n", len(finalDownloadItems))
}
