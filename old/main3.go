package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log" // Standard log, used by appLogger
	"math"
	"net/http"
	"net/url"
	"os"
	"path"          // For URL path manipulation
	"path/filepath" // For OS path manipulation
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- Constants and Global Variables ---
const (
	maxFilenameDisplayLength = 20
	progressBarWidth         = 25
	redrawInterval           = 150 * time.Millisecond
	speedUpdateInterval      = 750 * time.Millisecond
)

var stdoutMutex sync.Mutex
var appLogger *log.Logger
var logFile *os.File
var debugMode bool

// --- Structs for Hugging Face API ---
type RepoInfo struct {
	Siblings []Sibling `json:"siblings"`
}

type Sibling struct {
	Rfilename string `json:"rfilename"`
}

// --- Logging ---
func initLogging() {
	if debugMode {
		var err error
		logFile, err = os.OpenFile("log.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: Failed to open log file 'log.log' for debugging: %v. Debug logs will not be written to file.\n", err)
			appLogger = log.New(io.Discard, "", 0) // No-op logger
			return
		}
		appLogger = log.New(logFile, "", log.Ldate|log.Ltime|log.Lmicroseconds)
		appLogger.Println("---------------- Logging Initialized (Debug Mode) ----------------")
	} else {
		appLogger = log.New(io.Discard, "", 0) // Discard logs if not in debug mode
	}
}

// --- Formatting and Utility Functions ---
func formatSpeed(bytesPerSecond float64) string {
	if bytesPerSecond < 0 {
		return "--- B/s"
	}
	if bytesPerSecond < 1024 {
		return fmt.Sprintf("%6.2f B/s", bytesPerSecond)
	}
	kbps := bytesPerSecond / 1024
	if kbps < 1024 {
		return fmt.Sprintf("%6.2f KB/s", kbps)
	}
	mbps := kbps / 1024
	return fmt.Sprintf("%6.2f MB/s", mbps)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// calculateETA now takes an additional boolean to control second precision
func calculateETA(speedBps float64, totalSize int64, currentSize int64, showSeconds bool) string {
	if speedBps <= 0 || totalSize <= 0 || currentSize >= totalSize {
		return "N/A"
	}

	remainingBytes := totalSize - currentSize
	remainingSeconds := float64(remainingBytes) / speedBps

	if !showSeconds { // Simplified ETA for overall progress
		if remainingSeconds < 60 { // Less than a minute
			return "<1 min"
		}
		if remainingSeconds < 3600 { // Less than an hour
			minutes := math.Round(remainingSeconds / 60)
			return fmt.Sprintf("%.0f min", minutes)
		}
		// More than or equal to an hour
		hours := math.Floor(remainingSeconds / 3600)
		minutes := math.Round(math.Mod(remainingSeconds, 3600) / 60)
		if minutes == 60 { // Handle rounding up to 60 minutes
			minutes = 0
			hours++
		}
		return fmt.Sprintf("%.0f hr %.0f min", hours, minutes)
	}

	// Original precise ETA for individual bars
	if remainingSeconds < 1 {
		return "<1 sec"
	}
	if remainingSeconds < 60 {
		return fmt.Sprintf("%.0f sec", math.Ceil(remainingSeconds))
	}
	if remainingSeconds < 3600 {
		minutes := math.Floor(remainingSeconds / 60)
		seconds := math.Ceil(math.Mod(remainingSeconds, 60))
		if seconds == 60 {
			seconds = 0
			minutes++
		}
		return fmt.Sprintf("%.0f min %.0f sec", minutes, seconds)
	}
	hours := math.Floor(remainingSeconds / 3600)
	remainderMinutes := math.Mod(remainingSeconds, 3600)
	minutes := math.Floor(remainderMinutes / 60)
	seconds := math.Ceil(math.Mod(remainderMinutes, 60))
	if seconds == 60 {
		seconds = 0
		minutes++
		if minutes == 60 {
			minutes = 0
			hours++
		}
	}
	return fmt.Sprintf("%.0f hr %.0f min %.0f sec", hours, minutes, seconds)
}

func shortenError(err error, maxLen int) string {
	s := err.Error()
	runes := []rune(s)
	if len(runes) > maxLen {
		if maxLen <= 3 {
			if maxLen <= 0 {
				return "..."
			}
			return string(runes[:maxLen])
		}
		return string(runes[:maxLen-3]) + "..."
	}
	return s
}

func generateActualFilename(urlStr string) string {
	var fileName string
	parsedURL, err := url.Parse(urlStr)
	if err == nil {
		fileName = path.Base(parsedURL.Path) // Use path.Base for URL paths
	} else {
		fileName = filepath.Base(urlStr) // Fallback, less ideal for general URLs
		appLogger.Printf("[generateActualFilename] Warning: URL parsing failed for '%s', using filepath.Base as fallback: %v", urlStr, err)
	}

	if fileName == "." || fileName == "/" || fileName == "" {
		base := "download_" + strconv.FormatInt(time.Now().UnixNano(), 16)[:8]
		originalBaseName := ""
		if parsedURL != nil {
			originalBaseName = path.Base(parsedURL.Path)
		} else {
			originalBaseName = filepath.Base(urlStr)
		}
		ext := filepath.Ext(originalBaseName)
		if ext != "" && len(ext) > 1 && len(ext) < 7 && !strings.ContainsAny(ext, "?&=/:\\*\"<>|") {
			fileName = base + ext
		} else {
			fileName = base + ".file"
		}
		appLogger.Printf("[generateActualFilename] Generated filename '%s' for URL '%s'", fileName, urlStr)
	}
	return fileName
}

// --- ProgressWriter (handles individual download progress) ---
type ProgressWriter struct {
	id                   int
	URL                  string
	FileName             string // Display filename
	ActualFileName       string // Actual filename on disk
	Total                int64
	Current              int64
	IsFinished           bool
	ErrorMsg             string
	mu                   sync.Mutex
	manager              *ProgressManager
	lastSpeedCalcTime    time.Time
	lastSpeedCalcCurrent int64
	currentSpeedBps      float64
}

func newProgressWriter(id int, url, actualFileName string, totalSize int64, manager *ProgressManager) *ProgressWriter {
	displayFileName := actualFileName
	if len(actualFileName) > maxFilenameDisplayLength {
		suffixLen := maxFilenameDisplayLength - 3
		if suffixLen < 0 {
			suffixLen = 0
		}
		if len(actualFileName) > suffixLen {
			displayFileName = "..." + actualFileName[len(actualFileName)-suffixLen:]
		}
	}
	return &ProgressWriter{
		id: id, URL: url, FileName: displayFileName, ActualFileName: actualFileName, Total: totalSize,
		manager: manager, lastSpeedCalcTime: time.Now(),
	}
}

func (pw *ProgressWriter) Write(p []byte) (int, error) {
	n := len(p)
	pw.mu.Lock()
	if pw.IsFinished {
		pw.mu.Unlock()
		return n, io.EOF
	}
	pw.Current += int64(n)
	pw.mu.Unlock()
	pw.manager.requestRedraw()
	return n, nil
}

func (pw *ProgressWriter) UpdateSpeed() {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	if pw.IsFinished {
		return
	}
	now := time.Now()
	elapsed := now.Sub(pw.lastSpeedCalcTime)
	if elapsed < speedUpdateInterval && (pw.Total <= 0 || pw.Current < pw.Total) {
		return
	}
	if elapsed.Seconds() < 0.05 {
		return
	}
	bytesDownloadedInInterval := pw.Current - pw.lastSpeedCalcCurrent
	if bytesDownloadedInInterval < 0 {
		bytesDownloadedInInterval = 0
	}
	pw.currentSpeedBps = float64(bytesDownloadedInInterval) / elapsed.Seconds()
	pw.lastSpeedCalcTime = now
	pw.lastSpeedCalcCurrent = pw.Current
}

func (pw *ProgressWriter) MarkFinished(errMsg string) {
	pw.mu.Lock()
	pw.IsFinished = true
	pw.ErrorMsg = errMsg
	pw.currentSpeedBps = 0
	if errMsg == "" && pw.Total > 0 && pw.Current < pw.Total {
		pw.Current = pw.Total
	} else if errMsg == "" && pw.Total <= 0 && pw.Current > 0 {
		pw.Total = pw.Current
	}
	pw.mu.Unlock()
	pw.manager.requestRedraw()
}

func (pw *ProgressWriter) getProgressString() string {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	current, total, isFinished, errorMsg := pw.Current, pw.Total, pw.IsFinished, pw.ErrorMsg
	fileName, currentSpeed := pw.FileName, pw.currentSpeedBps
	speedStr, etaStr := formatSpeed(currentSpeed), "N/A"

	if isFinished {
		if errorMsg == "" {
			speedStr = "Done    "
			etaStr = ""
		} else {
			speedStr = "Error   "
		}
	} else {
		if total <= 0 && current == 0 {
			speedStr = "Pending "
		} else if currentSpeed > 0 && total > 0 && current < total {
			etaStr = calculateETA(currentSpeed, total, current, true) // Show seconds for individual
		} else if total > 0 && current == 0 {
			speedStr = "Waiting "
		}
	}

	if isFinished {
		if errorMsg != "" {
			maxErrDisplay := progressBarWidth + 20
			displayError := errorMsg
			runes := []rune(displayError)
			if len(runes) > maxErrDisplay {
				if maxErrDisplay <= 3 {
					displayError = string(runes[:maxErrDisplay])
				} else {
					displayError = string(runes[:maxErrDisplay-3]) + "..."
				}
			}
			return fmt.Sprintf("%-*s: [ERROR: %s]", maxFilenameDisplayLength, fileName, displayError)
		}
		percentage, bar := 100.0, strings.Repeat("=", progressBarWidth)
		currentMB := float64(current) / (1024 * 1024)
		return fmt.Sprintf("%-*s: [%s] %6.2f%% (%6.2f MB) @ %s", maxFilenameDisplayLength, fileName, bar, percentage, currentMB, speedStr)
	}

	percentage, barFill, indeterminate := 0.0, "", false
	if total > 0 {
		percentage = (float64(current) / float64(total)) * 100
		if percentage > 100 {
			percentage = 100
		}
		if percentage < 0 {
			percentage = 0
		}
		filledWidth := int(math.Round(float64(progressBarWidth) * percentage / 100.0))
		if filledWidth > progressBarWidth {
			filledWidth = progressBarWidth
		}
		if filledWidth < 0 {
			filledWidth = 0
		}
		barRunes := []rune(strings.Repeat(" ", progressBarWidth))
		for i := 0; i < filledWidth; i++ {
			if i < progressBarWidth {
				barRunes[i] = '='
			}
		}
		if filledWidth < progressBarWidth && percentage < 100.0 && percentage >= 0.0 {
			idxForArrow := filledWidth
			if idxForArrow >= 0 && idxForArrow < progressBarWidth {
				barRunes[idxForArrow] = '>'
			}
		}
		barFill = string(barRunes)
	} else { // total <= 0 (unknown size)
		if current > 0 { // Downloading, size unknown
			indeterminate = true
			spinChars := []string{"|", "/", "-", "\\"}
			spinner := spinChars[int(time.Now().UnixNano()/(int64(redrawInterval)/int64(len(spinChars))))%len(spinChars)]
			mid := progressBarWidth / 2
			barRunes := []rune(strings.Repeat(" ", progressBarWidth))
			idxSpinner := maxInt(0, mid-1)
			if progressBarWidth > 0 && idxSpinner < progressBarWidth {
				barRunes[idxSpinner] = []rune(spinner)[0]
			} else if progressBarWidth > 0 {
				barRunes[0] = []rune(spinner)[0]
			}
			barFill = string(barRunes)
		} else {
			barFill = strings.Repeat("?", progressBarWidth)
		} // Not started, size unknown
	}
	bar := "[" + barFill + "]"
	currentMB := float64(current) / (1024 * 1024)
	totalMBStr := "???.?? MB"
	if total > 0 {
		totalMBStr = fmt.Sprintf("%.2f MB", float64(total)/(1024*1024))
	}

	if indeterminate {
		return fmt.Sprintf("%-*s: %s (%6.2f MB / unknown) @ %s ETA: %s", maxFilenameDisplayLength, fileName, bar, currentMB, speedStr, etaStr)
	}
	return fmt.Sprintf("%-*s: %s %6.2f%% (%6.2f MB / %s) @ %s ETA: %s", maxFilenameDisplayLength, fileName, bar, percentage, currentMB, totalMBStr, speedStr, etaStr)
}

// --- ProgressManager (manages all progress bars and redrawing) ---
type ProgressManager struct {
	bars               []*ProgressWriter
	mu                 sync.Mutex
	redrawPending      bool
	stopRedraw         chan struct{}
	wg                 sync.WaitGroup
	displayConcurrency int
}

func NewProgressManager(displayConcurrency int) *ProgressManager {
	m := &ProgressManager{
		bars: make([]*ProgressWriter, 0), stopRedraw: make(chan struct{}),
		displayConcurrency: displayConcurrency,
	}
	m.wg.Add(1)
	go m.redrawLoop()
	return m
}

func (m *ProgressManager) AddInitialDownloads(pws []*ProgressWriter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bars = append(m.bars, pws...)
	appLogger.Printf("[PM.AddInitialDownloads] Added %d initial bars. Total bars: %d.", len(pws), len(m.bars))
	m.redrawPending = true
}

func (m *ProgressManager) requestRedraw() {
	m.mu.Lock()
	m.redrawPending = true
	m.mu.Unlock()
}

func (m *ProgressManager) redrawLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(redrawInterval)
	defer ticker.Stop()
	stdoutMutex.Lock()
	fmt.Print("\033[?25l")
	stdoutMutex.Unlock() // Hide cursor
	defer func() {
		m.performActualDraw(true) // Final draw
		stdoutMutex.Lock()
		fmt.Print("\033[?25h")
		fmt.Println()
		stdoutMutex.Unlock() // Show cursor
	}()

	for {
		forceRedraw := false
		select {
		case <-m.stopRedraw:
			appLogger.Println("[PM.redrawLoop] Stop signal.")
			return
		case <-ticker.C:
			forceRedraw = true
		}
		m.mu.Lock()
		for _, bar := range m.bars {
			if !bar.IsFinished && (bar.Current > 0 || bar.Total > 0) {
				bar.UpdateSpeed()
			}
		}
		if m.redrawPending {
			forceRedraw = true
			m.redrawPending = false
		}
		if !forceRedraw && m.hasIndeterminateOrActiveBarsLocked() {
			forceRedraw = true
		}
		m.mu.Unlock()
		if forceRedraw {
			m.performActualDraw(false)
		}
	}
}

func (m *ProgressManager) hasIndeterminateOrActiveBarsLocked() bool { // Assumes m.mu is locked
	for _, bar := range m.bars {
		bar.mu.Lock()
		active := !bar.IsFinished
		indeterminate := active && bar.Current > 0 && bar.Total <= 0
		simplyActive := active && bar.Current > 0
		bar.mu.Unlock()
		if indeterminate || simplyActive {
			return true
		}
	}
	return false
}

func (m *ProgressManager) getOverallProgressString(barsSnapshot []*ProgressWriter) string {
	var currentBytes, expectedBytes int64
	var overallSpeed float64
	allDone := true
	totalTasks := len(barsSnapshot)
	finishedTasks, activeDownloads := 0, 0

	for _, bar := range barsSnapshot {
		bar.mu.Lock()
		currentBytes += bar.Current
		if bar.Total > 0 {
			expectedBytes += bar.Total
		}
		if !bar.IsFinished && bar.Current > 0 {
			overallSpeed += bar.currentSpeedBps
			activeDownloads++
		}
		if !bar.IsFinished {
			allDone = false
		} else {
			finishedTasks++
		}
		bar.mu.Unlock()
	}

	percentage := 0.0
	if expectedBytes > 0 {
		percentage = (float64(currentBytes) / float64(expectedBytes)) * 100
		if percentage > 100 {
			percentage = 100
		}
		if percentage < 0 {
			percentage = 0
		}
	} else if allDone && totalTasks > 0 {
		percentage = 100.0
	}

	// Determine unit for overall size (MB or GB)
	useGB := false
	if expectedBytes >= 1024*1024*1024 { // If total is 1GB or more
		useGB = true
	}

	var currentStr, expectedStr string
	if useGB {
		currentStr = fmt.Sprintf("%.2f GB", float64(currentBytes)/(1024*1024*1024))
		if expectedBytes > 0 {
			expectedStr = fmt.Sprintf("%.2f GB", float64(expectedBytes)/(1024*1024*1024))
		} else {
			expectedStr = "???.?? GB" // Should be caught by the next if block if allDone
		}
	} else {
		currentStr = fmt.Sprintf("%.2f MB", float64(currentBytes)/(1024*1024))
		if expectedBytes > 0 {
			expectedStr = fmt.Sprintf("%.2f MB", float64(expectedBytes)/(1024*1024))
		} else {
			expectedStr = "???.?? MB" // Should be caught by the next if block if allDone
		}
	}
	// If all tasks are done and the original expected size was unknown (<=0),
	// set the expected string to be the same as the current downloaded string.
	if allDone && totalTasks > 0 && expectedBytes <= 0 {
		expectedStr = currentStr
	}

	speedStr, etaStr := formatSpeed(overallSpeed), "N/A"
	if !allDone && overallSpeed > 0 && expectedBytes > 0 && currentBytes < expectedBytes {
		remaining := expectedBytes - currentBytes
		if remaining > 0 {
			etaStr = calculateETA(overallSpeed, expectedBytes, currentBytes, false) // No seconds for overall
		}
	} else if allDone && totalTasks > 0 {
		etaStr = "Done"
		speedStr = "Completed"
	} else if activeDownloads == 0 && !allDone && totalTasks > 0 {
		speedStr = "Pending..."
	} else if totalTasks == 0 && !allDone { // If no tasks yet and not all done (e.g. initial state)
		speedStr = "Initializing..."
		etaStr = "---"
	}

	barW := progressBarWidth + 10
	filledW := 0
	if (expectedBytes > 0 || (allDone && totalTasks > 0)) && percentage > 0 {
		filledW = int(math.Round(float64(barW) * percentage / 100.0))
	}
	if filledW > barW {
		filledW = barW
	}
	if filledW < 0 {
		filledW = 0
	}
	overallBar := "[" + strings.Repeat("=", filledW) + strings.Repeat(" ", barW-filledW) + "]"

	// Files info on a new line, indented
	filesInfo := fmt.Sprintf("  (%d/%d files)", finishedTasks, totalTasks)

	return fmt.Sprintf("Overall %-*s %6.2f%% (%s / %s) @ %s ETA: %s\n%s",
		barW+1, overallBar, percentage, currentStr, expectedStr, speedStr, etaStr, filesInfo)
}

func (m *ProgressManager) performActualDraw(isFinalDraw bool) {
	m.mu.Lock()
	barsSnapshot := make([]*ProgressWriter, len(m.bars))
	copy(barsSnapshot, m.bars)
	m.mu.Unlock()
	appLogger.Printf("[PM.performActualDraw] Drawing %d bars. Final: %t. DisplayLimit: %d", len(barsSnapshot), isFinalDraw, m.displayConcurrency)

	if isFinalDraw {
		for _, b := range barsSnapshot {
			b.mu.Lock()
			if !b.IsFinished {
				b.IsFinished = true
				b.currentSpeedBps = 0
				if b.ErrorMsg == "" && b.Total > 0 && b.Current < b.Total {
					b.Current = b.Total
				} else if b.ErrorMsg == "" && b.Total <= 0 && b.Current > 0 {
					b.Total = b.Current
				}
			}
			b.mu.Unlock()
		}
	}
	stdoutMutex.Lock()
	defer stdoutMutex.Unlock()
	fmt.Print("\033[H\033[2J")
	fmt.Println("Download Progress:")
	fmt.Println(strings.Repeat("-", 80))

	barsToDisplay := make([]*ProgressWriter, 0)
	if isFinalDraw {
		barsToDisplay = barsSnapshot
	} else {
		active, pending := make([]*ProgressWriter, 0), make([]*ProgressWriter, 0)
		for _, bar := range barsSnapshot {
			bar.mu.Lock()
			isFin, curr := bar.IsFinished, bar.Current
			bar.mu.Unlock()
			if !isFin {
				if curr > 0 {
					active = append(active, bar)
				} else {
					pending = append(pending, bar)
				}
			}
		}
		for _, bar := range active {
			if len(barsToDisplay) < m.displayConcurrency {
				barsToDisplay = append(barsToDisplay, bar)
			} else {
				break
			}
		}
		if len(barsToDisplay) < m.displayConcurrency {
			for _, bar := range pending {
				if len(barsToDisplay) < m.displayConcurrency {
					barsToDisplay = append(barsToDisplay, bar)
				} else {
					break
				}
			}
		}
	}
	for _, bar := range barsToDisplay {
		fmt.Println(bar.getProgressString())
	}

	fmt.Println(strings.Repeat("-", 80)) // Print separator regardless
	if len(barsSnapshot) > 0 {           // Show overall progress if manager has been initialized with bars
		fmt.Println(m.getOverallProgressString(barsSnapshot))
	} else { // Placeholder if called before AddInitialDownloads (e.g., during slow pre-scan)
		// Adjusted placeholder to match new overall format (files on next line)
		fmt.Printf("Overall [Processing...........................]   ---.-%% (--- MB / --- MB) @ Initializing... ETA: ---\n  (0/? files)\n")
	}
	os.Stdout.Sync()
}

func (m *ProgressManager) Stop() {
	appLogger.Println("[PM.Stop] Called.")
	close(m.stopRedraw)
	appLogger.Println("[PM.Stop] Waiting for redrawLoop.")
	m.wg.Wait()
	appLogger.Println("[PM.Stop] RedrawLoop finished.")
}

// --- Downloader Function ---
func downloadFile(pw *ProgressWriter, wg *sync.WaitGroup, downloadDir string, manager *ProgressManager) {
	logPrefix := fmt.Sprintf("[downloadFile:%s]", pw.URL)
	appLogger.Printf("%s Started (File: %s).", logPrefix, pw.ActualFileName)
	defer func() {
		appLogger.Printf("%s Finished (File: %s).", logPrefix, pw.ActualFileName)
		wg.Done()
	}()

	filePath := filepath.Join(downloadDir, pw.ActualFileName)

	client := http.Client{}
	req, err := http.NewRequest("GET", pw.URL, nil)
	if err != nil {
		pw.MarkFinished(fmt.Sprintf("Req create: %v", shortenError(err, 25)))
		return
	}
	req.Header.Set("User-Agent", "Go-File-Downloader/1.0 (GET)")

	resp, getErr := client.Do(req)
	if getErr != nil {
		pw.MarkFinished(fmt.Sprintf("GET: %v", shortenError(getErr, 25)))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		pw.MarkFinished(fmt.Sprintf("HTTP %s", resp.Status))
		return
	}
	appLogger.Printf("%s GET OK. Len: %d", logPrefix, resp.ContentLength)

	pw.mu.Lock()
	if resp.ContentLength > 0 && (pw.Total <= 0 || pw.Total != resp.ContentLength) {
		appLogger.Printf("%s Update total %d -> %d", logPrefix, pw.Total, resp.ContentLength)
		pw.Total = resp.ContentLength
	} else if pw.Total <= 0 && resp.ContentLength <= 0 {
		appLogger.Printf("%s Total still unknown.", logPrefix)
	}
	pw.mu.Unlock()
	manager.requestRedraw()

	out, createErr := os.Create(filePath)
	if createErr != nil {
		pw.MarkFinished(fmt.Sprintf("Create file: %v", shortenError(createErr, 25)))
		return
	}
	defer out.Close()

	appLogger.Printf("%s Copying to '%s'", logPrefix, filePath)
	_, copyErr := io.Copy(out, io.TeeReader(resp.Body, pw))
	if copyErr != nil {
		pw.mu.Lock()
		alreadyDone := pw.IsFinished
		pw.mu.Unlock()
		if alreadyDone && copyErr == io.EOF {
			appLogger.Printf("%s Copy interrupted, already marked done.", logPrefix)
		} else {
			pw.MarkFinished(fmt.Sprintf("Copy: %v", shortenError(copyErr, 25)))
		}
	} else {
		pw.MarkFinished("")
	}
}

// --- Hugging Face URL Fetching Logic ---
func fetchHuggingFaceURLs(repoInput string) ([]string, error) {
	appLogger.Printf("[HF] Processing Hugging Face repository input: %s", repoInput)

	var repoID string
	// var parsedInputURL *url.URL // Keep for potential full URL parsing

	// Check if the input is a full URL or just owner/repo
	if strings.HasPrefix(repoInput, "http://") || strings.HasPrefix(repoInput, "https://") {
		parsedInputURL, err := url.Parse(repoInput)
		if err != nil {
			return nil, fmt.Errorf("error parsing repository URL '%s': %w", repoInput, err)
		}
		if parsedInputURL.Host != "huggingface.co" {
			return nil, fmt.Errorf("expected a huggingface.co URL, got: %s", parsedInputURL.Host)
		}
		repoPath := strings.TrimPrefix(parsedInputURL.Path, "/")
		pathParts := strings.Split(repoPath, "/")
		if len(pathParts) < 2 {
			return nil, fmt.Errorf("invalid repository path in URL. Expected 'owner/repo_name', got: '%s'", repoPath)
		}
		repoID = fmt.Sprintf("%s/%s", pathParts[0], pathParts[1])
	} else if strings.Count(repoInput, "/") == 1 { // Assume owner/repo format
		parts := strings.Split(repoInput, "/")
		if len(parts[0]) > 0 && len(parts[1]) > 0 {
			repoID = repoInput
		} else {
			return nil, fmt.Errorf("invalid repository ID format. Expected 'owner/repo_name', got: '%s'", repoInput)
		}
	} else {
		return nil, fmt.Errorf("invalid -hf input '%s'. Expected 'owner/repo_name' or full https://huggingface.co/owner/repo_name URL", repoInput)
	}

	branch := "main" // Default branch for download URLs

	appLogger.Printf("[HF] Determined RepoID: %s, Branch for download URLs: %s", repoID, branch)
	fmt.Fprintf(os.Stderr, "[INFO] Fetching file list for repository: %s (branch: %s)...\n", repoID, branch)

	apiURL := fmt.Sprintf("https://huggingface.co/api/models/%s", repoID)
	appLogger.Printf("[HF] Using API endpoint for repo files: %s", apiURL)

	httpClient := http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Get(apiURL)
	if err != nil {
		return nil, fmt.Errorf("error fetching data from API '%s': %w", apiURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API request failed with status %s for URL %s", resp.Status, apiURL)
	}

	var repoData RepoInfo
	if err := json.NewDecoder(resp.Body).Decode(&repoData); err != nil {
		return nil, fmt.Errorf("error decoding JSON response: %w", err)
	}

	if len(repoData.Siblings) == 0 {
		appLogger.Printf("[HF] No files found in repository %s via API.", repoID)
		fmt.Fprintf(os.Stderr, "[INFO] No files found in repository %s. The API might have changed or the repo is empty/private.\n", repoID)
		return []string{}, nil
	}

	appLogger.Printf("[HF] Found %d file entries in repository %s.", len(repoData.Siblings), repoID)
	fmt.Fprintf(os.Stderr, "[INFO] Found %d file entries. Generating download URLs...\n", len(repoData.Siblings))

	var downloadURLs []string
	for _, sibling := range repoData.Siblings {
		if sibling.Rfilename == "" {
			appLogger.Printf("[HF] Skipping sibling with empty rfilename.")
			continue
		}

		rfilenameParts := strings.Split(sibling.Rfilename, "/")
		escapedRfilenameParts := make([]string, len(rfilenameParts))
		for i, p := range rfilenameParts {
			escapedRfilenameParts[i] = url.PathEscape(p)
		}
		safeRfilenamePath := strings.Join(escapedRfilenameParts, "/")

		dlURL := fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s?download=true", repoID, branch, safeRfilenamePath)
		downloadURLs = append(downloadURLs, dlURL)
		appLogger.Printf("[HF] Generated download URL: %s for rfilename: %s", dlURL, sibling.Rfilename)
	}
	fmt.Fprintf(os.Stderr, "[INFO] Successfully generated %d download URLs from Hugging Face repository.\n", len(downloadURLs))
	return downloadURLs, nil
}

// --- Main Application ---
func main() {
	var concurrency int
	var urlsFilePath, hfRepoInput string
	flag.BoolVar(&debugMode, "debug", false, "Enable debug logging to log.log")
	flag.IntVar(&concurrency, "c", 3, "Number of concurrent downloads & display lines")
	flag.StringVar(&urlsFilePath, "f", "", "Path to text file containing URLs")
	flag.StringVar(&hfRepoInput, "hf", "", "Hugging Face repository ID (e.g., owner/repo_name) or full URL")
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
	if hfRepoInput != "" {
		maxHfConcurrency := 4
		if concurrency <= 0 { // Ensure initial user input is positive before capping
			fmt.Fprintf(os.Stderr, "[INFO] Concurrency must be positive. Defaulting to %d for -hf.\n", maxHfConcurrency)
			concurrency = maxHfConcurrency
		} else if concurrency > maxHfConcurrency {
			fmt.Fprintf(os.Stderr, "[INFO] Concurrency for -hf is capped at %d. Using %d.\n", maxHfConcurrency, maxHfConcurrency)
			appLogger.Printf("User specified concurrency %d for -hf, capped to %d.", concurrency, maxHfConcurrency)
			concurrency = maxHfConcurrency
		}
	} else { // -f is used
		maxFileConcurrency := 100
		if concurrency <= 0 { // Ensure initial user input is positive
			fmt.Fprintf(os.Stderr, "[INFO] Concurrency must be positive. Defaulting to 3 for -f.\n")
			concurrency = 3 // A sensible default if user gives <=0 for -f
		} else if concurrency > maxFileConcurrency {
			fmt.Fprintf(os.Stderr, "[INFO] Concurrency for -f is capped at %d. Using %d.\n", maxFileConcurrency, maxFileConcurrency)
			appLogger.Printf("User specified concurrency %d for -f, capped to %d.", concurrency, maxFileConcurrency)
			concurrency = maxFileConcurrency
		}
	}
	// Final check, though previous logic should ensure concurrency > 0
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
		urls, err = fetchHuggingFaceURLs(hfRepoInput)
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
			repoSubDir := strings.ReplaceAll(repoOwner+"_"+repoName, "..", "")         // Basic sanitization
			repoSubDir = strings.ReplaceAll(repoSubDir, string(os.PathSeparator), "_") // Replace path separators
			repoSubDir = strings.ReplaceAll(repoSubDir, ":", "_")                      // Replace colons
			// Add more sanitization if needed for other invalid characters
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

	manager := NewProgressManager(concurrency)
	fmt.Fprintf(os.Stderr, "[INFO] Pre-scanning %d URLs for sizes (this may take a moment)...\n", len(urls))
	if len(urls) > 0 {
		manager.performActualDraw(false) // Show initial "Processing..." state
	}

	allPWs := make([]*ProgressWriter, len(urls))
	var preScanWG sync.WaitGroup
	preScanSem := make(chan struct{}, 20) // Concurrency for HEAD requests
	for i, urlStr := range urls {
		preScanWG.Add(1)
		preScanSem <- struct{}{}
		go func(idx int, u string) {
			defer func() {
				<-preScanSem
				preScanWG.Done()
			}()
			actualFile := generateActualFilename(u)
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
			allPWs[idx] = newProgressWriter(idx, u, actualFile, initialSize, manager)
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
			downloadFile(pWriter, &dlWG, downloadDir, manager)
		}(pw)
	}
	dlWG.Wait()
	appLogger.Println("All downloads processed.")
	manager.Stop()
	fmt.Fprintf(os.Stderr, "All %d download tasks have been processed.\n", len(urls))
}
