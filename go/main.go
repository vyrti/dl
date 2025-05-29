// main.go
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/http" // Required for PreScan http.NewRequest
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time" // Required for PreScan http.Client Timeout
)

// --- Main Application ---
func main() {
	var concurrency int
	var urlsFilePath, hfRepoInput string
	// debugMode is declared in downloader.go, flags will set it.
	flag.BoolVar(&debugMode, "debug", false, "Enable debug logging to log.log")
	flag.IntVar(&concurrency, "c", 3, "Number of concurrent downloads & display lines")
	flag.StringVar(&urlsFilePath, "f", "", "Path to text file containing URLs")
	flag.StringVar(&hfRepoInput, "hf", "", "Hugging Face repository ID (e.g., owner/repo_name) or full URL")
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

	appLogger.Printf("Effective Concurrency: %d. DebugMode: %t, FilePath: '%s', HF Repo Input: '%s'",
		concurrency, debugMode, urlsFilePath, hfRepoInput)

	var urls []string
	var err error
	fmt.Fprintln(os.Stderr, "[INFO] Initializing downloader...")

	if hfRepoInput != "" {
		fmt.Fprintf(os.Stderr, "[INFO] Preparing to fetch from Hugging Face repository: %s\n", hfRepoInput)
		urls, err = fetchHuggingFaceURLs(hfRepoInput) // Call from hf.go
		if err != nil {
			appLogger.Printf("Error fetching from HF '%s': %v", hfRepoInput, err)
			fmt.Fprintf(os.Stderr, "Error fetching from HF '%s': %v\n", hfRepoInput, err)
			os.Exit(1)
		}
	} else {
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
				urls = append(urls, urlStr)
			}
		}
		if serr := scanner.Err(); serr != nil {
			appLogger.Printf("Error reading '%s': %v", urlsFilePath, serr)
			fmt.Fprintf(os.Stderr, "Error reading '%s': %v\n", urlsFilePath, serr)
			os.Exit(1)
		}
		appLogger.Printf("Read %d URLs from '%s'.", len(urls), urlsFilePath)
		fmt.Fprintf(os.Stderr, "[INFO] Read %d URLs from '%s'.\n", len(urls), urlsFilePath)
	}

	if len(urls) == 0 {
		appLogger.Println("No URLs. Exiting.")
		fmt.Fprintln(os.Stderr, "[INFO] No URLs found. Exiting.")
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
			repoSubDir := strings.ReplaceAll(repoOwner+"_"+repoName, "..", "")
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
	fmt.Fprintf(os.Stderr, "[INFO] Pre-scanning %d URLs for sizes (this may take a moment)...\n", len(urls))
	if len(urls) > 0 {
		manager.performActualDraw(false)
	}

	allPWs := make([]*ProgressWriter, len(urls))
	var preScanWG sync.WaitGroup
	preScanSem := make(chan struct{}, 20)
	for i, urlStr := range urls {
		preScanWG.Add(1)
		preScanSem <- struct{}{}
		go func(idx int, u string) {
			defer func() {
				<-preScanSem
				preScanWG.Done()
			}()
			actualFile := generateActualFilename(u) // From downloader.go
			var initialSize int64 = -1
			headReq, _ := http.NewRequest("HEAD", u, nil)
			headReq.Header.Set("User-Agent", "Go-File-Downloader/1.0 (PreScan-HEAD)")
			headClient := http.Client{Timeout: 10 * time.Second}
			headResp, headErr := headClient.Do(headReq)
			if headErr == nil && headResp.StatusCode == http.StatusOK {
				initialSize = headResp.ContentLength
				if headResp.Body != nil {
					headResp.Body.Close()
				}
				appLogger.Printf("[PreScan:%s] HEAD success. Size: %d for %s", u, initialSize, actualFile)
			} else {
				if headErr != nil {
					appLogger.Printf("[PreScan:%s] HEAD error: %v for %s. Size unknown.", u, headErr, actualFile)
				} else if headResp != nil {
					appLogger.Printf("[PreScan:%s] HEAD non-OK status: %s for %s. Size unknown.", u, headResp.Status, actualFile)
					if headResp.Body != nil {
						headResp.Body.Close()
					}
				} else {
					appLogger.Printf("[PreScan:%s] HEAD error (no response): %v for %s. Size unknown.", u, headErr, actualFile)
				}
			}
			allPWs[idx] = newProgressWriter(idx, u, actualFile, initialSize, manager) // From downloader.go
		}(i, urlStr)
	}
	preScanWG.Wait()
	close(preScanSem)
	appLogger.Println("Pre-scan finished.")
	fmt.Fprintln(os.Stderr, "[INFO] Pre-scan complete.")
	manager.AddInitialDownloads(allPWs)

	appLogger.Printf("Downloading %d files to '%s' (concurrency: %d).", len(urls), downloadDir, concurrency)
	fmt.Fprintf(os.Stderr, "[INFO] Starting downloads for %d files to '%s' (concurrency: %d).\n", len(urls), downloadDir, concurrency)

	var dlWG sync.WaitGroup
	dlSem := make(chan struct{}, concurrency)
	for _, pw := range allPWs {
		dlSem <- struct{}{}
		dlWG.Add(1)
		go func(pWriter *ProgressWriter) {
			defer func() { <-dlSem }()
			downloadFile(pWriter, &dlWG, downloadDir, manager) // From downloader.go
		}(pw)
	}
	dlWG.Wait()
	appLogger.Println("All downloads processed.")
	manager.Stop()
	fmt.Fprintf(os.Stderr, "All %d download tasks have been processed.\n", len(urls))
}
