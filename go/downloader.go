// go/downloader.go
package main

import (
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- Constants and Global Variables (moved from main.go) ---
const (
	maxFilenameDisplayLength = 20
	progressBarWidth         = 25
	redrawInterval           = 150 * time.Millisecond
	speedUpdateInterval      = 750 * time.Millisecond
)

var stdoutMutex sync.Mutex
var appLogger *log.Logger
var logFile *os.File
var debugMode bool // This will be set by main.go

// --- Logging ---
func initLogging() {
	if debugMode {
		var err error
		logFile, err = os.OpenFile("log.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: Failed to open log file 'log.log' for debugging: %v. Debug logs will not be written to file.\n", err)
			appLogger = log.New(io.Discard, "", 0)
			return
		}
		appLogger = log.New(logFile, "", log.Ldate|log.Ltime|log.Lmicroseconds)
		appLogger.Println("---------------- Logging Initialized (Debug Mode) ----------------")
	} else {
		appLogger = log.New(io.Discard, "", 0)
	}
}

// --- Formatting and Utility Functions ---
// ... (formatSpeed, maxInt, calculateETA, shortenError, generateActualFilename - unchanged)
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

func calculateETA(speedBps float64, totalSize int64, currentSize int64, showSeconds bool) string {
	if speedBps <= 0 || totalSize <= 0 || currentSize >= totalSize {
		return "N/A"
	}

	remainingBytes := totalSize - currentSize
	remainingSeconds := float64(remainingBytes) / speedBps

	if !showSeconds {
		if remainingSeconds < 60 {
			return "<1 min"
		}
		if remainingSeconds < 3600 {
			minutes := math.Round(remainingSeconds / 60)
			return fmt.Sprintf("%.0f min", minutes)
		}
		hours := math.Floor(remainingSeconds / 3600)
		minutes := math.Round(math.Mod(remainingSeconds, 3600) / 60)
		if minutes == 60 {
			minutes = 0
			hours++
		}
		return fmt.Sprintf("%.0f hr %.0f min", hours, minutes)
	}

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
		fileName = path.Base(parsedURL.Path)
	} else {
		fileName = filepath.Base(urlStr)
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

// --- ProgressWriter ---
// ... (ProgressWriter struct and methods - unchanged)
type ProgressWriter struct {
	id                   int
	URL                  string
	FileName             string
	ActualFileName       string
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
	if elapsed.Seconds() < 0.05 { // Avoid division by zero or tiny intervals
		return
	}
	bytesDownloadedInInterval := pw.Current - pw.lastSpeedCalcCurrent
	if bytesDownloadedInInterval < 0 { // Should not happen, but defensive
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
	pw.currentSpeedBps = 0 // Stop speed calculation
	// If download finished successfully but total size was unknown or slightly off, fix it.
	if errMsg == "" && pw.Total > 0 && pw.Current < pw.Total {
		pw.Current = pw.Total // Ensure it shows 100% if successful and total was known
	} else if errMsg == "" && pw.Total <= 0 && pw.Current > 0 { // Total was unknown, set it to current
		pw.Total = pw.Current
	}
	pw.mu.Unlock()
	pw.manager.requestRedraw() // Request a final redraw for this bar
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
			etaStr = "" // No ETA for completed downloads
		} else {
			speedStr = "Error   "
			// etaStr will be N/A or message
		}
	} else { // Not finished
		if total <= 0 && current == 0 { // Not started, total unknown
			speedStr = "Pending "
		} else if currentSpeed > 0 && total > 0 && current < total { // Downloading with known total
			etaStr = calculateETA(currentSpeed, total, current, true)
		} else if total > 0 && current == 0 { // Queued with known total
			speedStr = "Waiting "
		}
		// If total is unknown but current > 0, speed will be calculated, eta will be N/A
	}

	// Constructing the progress bar string
	if isFinished {
		if errorMsg != "" {
			maxErrDisplay := progressBarWidth + 20 // Allow more space for error message
			displayError := errorMsg
			runes := []rune(displayError)
			if len(runes) > maxErrDisplay {
				if maxErrDisplay <= 3 { // very small maxErrDisplay
					displayError = string(runes[:maxErrDisplay])
				} else {
					displayError = string(runes[:maxErrDisplay-3]) + "..."
				}
			}
			return fmt.Sprintf("%-*s: [ERROR: %s]", maxFilenameDisplayLength, fileName, displayError)
		}
		// Finished successfully
		percentage, bar := 100.0, strings.Repeat("=", progressBarWidth)
		currentMB := float64(current) / (1024 * 1024)
		return fmt.Sprintf("%-*s: [%s] %6.2f%% (%6.2f MB) @ %s", maxFilenameDisplayLength, fileName, bar, percentage, currentMB, speedStr)
	}

	// Not finished, draw progress bar
	percentage, barFill, indeterminate := 0.0, "", false
	if total > 0 {
		percentage = (float64(current) / float64(total)) * 100
		if percentage > 100 {
			percentage = 100
		} // Cap at 100%
		if percentage < 0 {
			percentage = 0
		} // Floor at 0%
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
		// Add '>' for active downloads unless it's full
		if filledWidth < progressBarWidth && percentage < 100.0 && percentage >= 0.0 {
			idxForArrow := filledWidth // Place arrow at the end of filled part
			if idxForArrow >= 0 && idxForArrow < progressBarWidth {
				barRunes[idxForArrow] = '>'
			}
		}
		barFill = string(barRunes)
	} else { // Total size unknown
		if current > 0 { // Indeterminate progress (spinner)
			indeterminate = true
			spinChars := []string{"|", "/", "-", "\\"}
			// Use time to cycle spinner, ensuring it changes with redraws
			spinner := spinChars[int(time.Now().UnixNano()/(int64(redrawInterval)/int64(len(spinChars))))%len(spinChars)]
			mid := progressBarWidth / 2
			barRunes := []rune(strings.Repeat(" ", progressBarWidth))
			idxSpinner := maxInt(0, mid-1) // Ensure index is valid
			if progressBarWidth > 0 && idxSpinner < progressBarWidth {
				barRunes[idxSpinner] = []rune(spinner)[0]
			} else if progressBarWidth > 0 { // Fallback for very small progress bar
				barRunes[0] = []rune(spinner)[0]
			}
			barFill = string(barRunes)
		} else { // Total unknown, nothing downloaded yet
			barFill = strings.Repeat("?", progressBarWidth)
		}
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

// --- ProgressManager ---
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

func (m *ProgressManager) requestRedraw() { m.mu.Lock(); m.redrawPending = true; m.mu.Unlock() }

func (m *ProgressManager) redrawLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(redrawInterval)
	defer ticker.Stop()
	stdoutMutex.Lock()
	fmt.Print("\033[?25l")
	stdoutMutex.Unlock() // Hide cursor
	defer func() {
		m.performActualDraw(true) // Final draw to show all completed/errored
		stdoutMutex.Lock()
		fmt.Print("\033[?25h")
		fmt.Println()
		stdoutMutex.Unlock() // Show cursor and newline
	}()

	for {
		forceRedraw := false
		select {
		case <-m.stopRedraw:
			appLogger.Println("[PM.redrawLoop] Stop signal received.")
			return
		case <-ticker.C:
			forceRedraw = true // Periodically force redraw for spinners, ETA updates
		}

		m.mu.Lock()
		// Update speed for active, non-finished bars
		for _, bar := range m.bars {
			// bar.UpdateSpeed() is internally locked and checks IsFinished
			if !bar.IsFinished && (bar.Current > 0 || bar.Total > 0) { // only update if it makes sense
				bar.UpdateSpeed()
			}
		}
		if m.redrawPending {
			forceRedraw = true
			m.redrawPending = false
		}
		// Also force redraw if there are any indeterminate bars or active downloads for spinner/ETA
		if !forceRedraw && m.hasIndeterminateOrActiveBarsLocked() {
			forceRedraw = true
		}
		m.mu.Unlock()

		if forceRedraw {
			m.performActualDraw(false)
		}
	}
}

func (m *ProgressManager) hasIndeterminateOrActiveBarsLocked() bool {
	// This function assumes m.mu is already locked by the caller (redrawLoop)
	for _, bar := range m.bars {
		bar.mu.Lock() // Lock individual bar
		active := !bar.IsFinished
		indeterminate := active && bar.Current > 0 && bar.Total <= 0
		simplyActive := active && bar.Current > 0 // any bar that has started downloading
		bar.mu.Unlock()
		if indeterminate || simplyActive {
			return true
		}
	}
	return false
}

// ... (getOverallProgressString - unchanged from previous corrected version)
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
		} else if bar.IsFinished && bar.Current > 0 { // If finished and total was unknown, count current as expected
			expectedBytes += bar.Current
		}

		if !bar.IsFinished {
			allDone = false
			if bar.Current > 0 { // Active download if not finished and has progress
				overallSpeed += bar.currentSpeedBps
				activeDownloads++
			}
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
	} else if allDone && totalTasks > 0 { // All tasks done, even if total was 0 (e.g. empty files, or all errored before size known)
		percentage = 100.0
	}

	useGB := false
	// Switch to GB if expected total (or current total if expected is unknown but progress made) is >= 1GB
	effectiveTotalForUnit := expectedBytes
	if effectiveTotalForUnit == 0 && currentBytes > 0 {
		effectiveTotalForUnit = currentBytes // Use current if expected is zero but there's progress
	}
	if effectiveTotalForUnit >= 1024*1024*1024 {
		useGB = true
	}

	var currentStr, expectedStr string
	if useGB {
		currentStr = fmt.Sprintf("%.2f GB", float64(currentBytes)/(1024*1024*1024))
		if expectedBytes > 0 {
			expectedStr = fmt.Sprintf("%.2f GB", float64(expectedBytes)/(1024*1024*1024))
		} else if allDone && totalTasks > 0 {
			expectedStr = currentStr // If all done and total was unknown, use current sum for display
		} else {
			expectedStr = "???.?? GB"
		}
	} else { // Not using GB, use MB
		currentStr = fmt.Sprintf("%.2f MB", float64(currentBytes)/(1024*1024))
		if expectedBytes > 0 {
			expectedStr = fmt.Sprintf("%.2f MB", float64(expectedBytes)/(1024*1024))
		} else if allDone && totalTasks > 0 {
			expectedStr = currentStr // If all done and total was unknown, use current sum for display
		} else {
			expectedStr = "???.?? MB"
		}
	}

	speedStr, etaStr := formatSpeed(overallSpeed), "N/A"
	if !allDone && overallSpeed > 0 && expectedBytes > 0 && currentBytes < expectedBytes {
		remaining := expectedBytes - currentBytes
		if remaining > 0 { // Ensure ETA is calculated only if there's something remaining
			etaStr = calculateETA(overallSpeed, expectedBytes, currentBytes, false)
		}
	} else if allDone && totalTasks > 0 {
		etaStr = "Done"
		speedStr = "Completed " // Padded to align
	} else if activeDownloads == 0 && !allDone && totalTasks > 0 { // No active downloads, but not all done
		speedStr = "Pending... "
	} else if totalTasks == 0 && !allDone { // No tasks added yet or all removed/finished somehow
		speedStr = "Initializing..."
		etaStr = "---"
	}

	barW := progressBarWidth + 10 // Overall progress bar width
	filledW := 0
	if (expectedBytes > 0 || (allDone && totalTasks > 0)) && percentage >= 0 { // Ensure percentage is non-negative
		filledW = int(math.Round(float64(barW) * percentage / 100.0))
	}
	if filledW > barW {
		filledW = barW
	}
	if filledW < 0 {
		filledW = 0
	}

	overallBar := "[" + strings.Repeat("=", filledW) + strings.Repeat(" ", barW-filledW) + "]"
	filesInfo := fmt.Sprintf("  (%d/%d files)", finishedTasks, totalTasks)

	// Ensure consistent padding for speed string
	if len(speedStr) < 10 { // approx "xxx.xx KB/s"
		speedStr = fmt.Sprintf("%-10s", speedStr)
	}

	return fmt.Sprintf("Overall %-*s %6.2f%% (%s / %s) @ %s ETA: %s\n%s",
		barW+1, overallBar, percentage, currentStr, expectedStr, speedStr, etaStr, filesInfo)
}

func (m *ProgressManager) performActualDraw(isFinalDraw bool) {
	m.mu.Lock()
	// Create a snapshot of bars to work with outside the lock, minimizing lock duration
	barsSnapshot := make([]*ProgressWriter, len(m.bars))
	copy(barsSnapshot, m.bars)
	m.mu.Unlock()
	appLogger.Printf("[PM.performActualDraw] Drawing %d bars. Final: %t. DisplayLimit: %d", len(barsSnapshot), isFinalDraw, m.displayConcurrency)

	// If it's the final draw, mark all non-finished, non-errored bars as completed.
	if isFinalDraw {
		for _, b := range barsSnapshot {
			b.mu.Lock()
			if !b.IsFinished { // If it's not already marked (e.g. by an error)
				b.IsFinished = true
				b.currentSpeedBps = 0 // No speed for finished items
				if b.ErrorMsg == "" { // Only adjust if no error
					if b.Total > 0 && b.Current < b.Total {
						b.Current = b.Total // Ensure it shows 100%
					} else if b.Total <= 0 && b.Current > 0 {
						b.Total = b.Current // If total was unknown, set to current
					}
				}
			}
			b.mu.Unlock()
		}
	}

	stdoutMutex.Lock()
	defer stdoutMutex.Unlock()

	fmt.Print("\033[H\033[2J") // Clear screen and move cursor to top-left

	fmt.Println("Download Progress:")
	fmt.Println(strings.Repeat("-", 80)) // Separator line

	// Logic for displaying limited number of bars
	barsToDisplay := make([]*ProgressWriter, 0)
	if isFinalDraw {
		barsToDisplay = barsSnapshot // Show all bars on final draw
	} else {
		// Prioritize active downloads, then pending ones, up to displayConcurrency
		active := make([]*ProgressWriter, 0)
		pending := make([]*ProgressWriter, 0)
		finishedInSnapshot := make([]*ProgressWriter, 0)

		for _, bar := range barsSnapshot {
			bar.mu.Lock()
			// Assign bar.ErrorMsg to blank identifier `_` as local `errMsg` is not used in this block
			isFin, curr, _ := bar.IsFinished, bar.Current, bar.ErrorMsg
			bar.mu.Unlock()

			if !isFin {
				if curr > 0 { // Active download (has progress)
					active = append(active, bar)
				} else { // Pending (no progress yet)
					pending = append(pending, bar)
				}
			} else { // Already finished (completed or errored)
				finishedInSnapshot = append(finishedInSnapshot, bar)
			}
		}

		// Add active downloads first
		for _, bar := range active {
			if len(barsToDisplay) < m.displayConcurrency {
				barsToDisplay = append(barsToDisplay, bar)
			} else {
				break
			}
		}
		// Then add pending downloads if space allows
		if len(barsToDisplay) < m.displayConcurrency {
			for _, bar := range pending {
				if len(barsToDisplay) < m.displayConcurrency {
					barsToDisplay = append(barsToDisplay, bar)
				} else {
					break
				}
			}
		}
		// Then add finished/errored ones if space allows (e.g. if fewer than displayConcurrency items are active/pending)
		if len(barsToDisplay) < m.displayConcurrency {
			for _, bar := range finishedInSnapshot {
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

	// Summary line for non-displayed items if any
	if !isFinalDraw && len(barsSnapshot) > len(barsToDisplay) {
		remainingCount := len(barsSnapshot) - len(barsToDisplay)
		if remainingCount > 0 {
			fmt.Printf("... and %d more downloads ...\n", remainingCount)
		}
	}

	fmt.Println(strings.Repeat("-", 80)) // Separator line
	if len(barsSnapshot) > 0 {
		fmt.Println(m.getOverallProgressString(barsSnapshot))
	} else {
		// Default message if no downloads are active/queued yet
		fmt.Printf("Overall [Processing...........................]   ---.-%% (--- MB / --- MB) @ Initializing... ETA: ---\n  (0/? files)\n")
	}

	os.Stdout.Sync() // Force flush output
}

func (m *ProgressManager) Stop() {
	appLogger.Println("[PM.Stop] Stop method called.")
	close(m.stopRedraw) // Signal redrawLoop to stop
	appLogger.Println("[PM.Stop] Waiting for redrawLoop to finish.")
	m.wg.Wait() // Wait for redrawLoop goroutine to complete
	appLogger.Println("[PM.Stop] RedrawLoop finished.")
	// Cursor is made visible and newline printed in redrawLoop's defer
}

// --- Downloader Function ---
// ... (downloadFile - unchanged from previous corrected version)
func downloadFile(pw *ProgressWriter, wg *sync.WaitGroup, downloadDir string, manager *ProgressManager) {
	logPrefix := fmt.Sprintf("[downloadFile:%s]", pw.URL) // Use URL for logging as filename might not be unique if generated
	appLogger.Printf("%s Download initiated for URL (File: %s).", logPrefix, pw.ActualFileName)
	defer func() {
		appLogger.Printf("%s Goroutine finished (File: %s, Error: '%s').", logPrefix, pw.ActualFileName, pw.ErrorMsg)
		wg.Done()
		manager.requestRedraw() // Ensure a redraw occurs after a download finishes or errors
	}()

	filePath := filepath.Join(downloadDir, pw.ActualFileName)

	// Ensure download directory exists
	err := os.MkdirAll(downloadDir, os.ModePerm)
	if err != nil {
		pw.MarkFinished(fmt.Sprintf("Dir create: %v", shortenError(err, 25)))
		return
	}

	client := http.Client{
		Timeout: 60 * time.Minute, // Add a timeout for the entire download
	}
	req, err := http.NewRequest("GET", pw.URL, nil)
	if err != nil {
		pw.MarkFinished(fmt.Sprintf("Req create: %v", shortenError(err, 25)))
		return
	}
	// Set a common user agent
	req.Header.Set("User-Agent", "Go-File-Downloader/1.1")

	resp, getErr := client.Do(req)
	if getErr != nil {
		pw.MarkFinished(fmt.Sprintf("GET: %v", shortenError(getErr, 25)))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		pw.MarkFinished(fmt.Sprintf("HTTP %s", resp.Status)) // e.g. HTTP 404 Not Found
		return
	}
	appLogger.Printf("%s HTTP GET successful. ContentLength: %d", logPrefix, resp.ContentLength)

	pw.mu.Lock()
	if resp.ContentLength > 0 && (pw.Total <= 0 || pw.Total != resp.ContentLength) {
		// Update total size if header provides it and it's different or was unknown
		appLogger.Printf("%s Updating total size from %d to %d", logPrefix, pw.Total, resp.ContentLength)
		pw.Total = resp.ContentLength
	} else if pw.Total <= 0 && resp.ContentLength <= 0 {
		appLogger.Printf("%s Total size remains unknown from headers.", logPrefix)
		// pw.Total remains 0 or uninitialized, progress bar will be indeterminate
	}
	pw.mu.Unlock()
	manager.requestRedraw() // Request redraw as total size might have changed

	// Create the output file
	out, createErr := os.Create(filePath)
	if createErr != nil {
		pw.MarkFinished(fmt.Sprintf("Create file: %v", shortenError(createErr, 25)))
		return
	}
	defer out.Close()

	appLogger.Printf("%s Starting file copy to '%s'", logPrefix, filePath)
	// io.Copy will write to 'out' and also through 'pw' (ProgressWriter)
	_, copyErr := io.Copy(out, io.TeeReader(resp.Body, pw))

	if copyErr != nil {
		// Check if already marked finished (e.g., by a timeout or manual stop)
		// io.EOF is not an error for Copy if the source is fully read.
		// However, if TeeReader's Write method returns an error (like io.EOF if pw.IsFinished), Copy might propagate it.
		pw.mu.Lock()
		alreadyDone := pw.IsFinished
		pw.mu.Unlock()

		if alreadyDone && (copyErr == io.EOF || strings.Contains(copyErr.Error(), "EOF")) { // If it was externally marked done and copy stopped due to that
			appLogger.Printf("%s Copy interrupted, but already marked done (possibly by stop signal). Error: %v", logPrefix, copyErr)
			// Do not MarkFinished again if it was due to a graceful stop.
			// If pw.ErrorMsg is already set, it will be kept. If not, it implies a successful stop.
		} else {
			pw.MarkFinished(fmt.Sprintf("Copy: %v", shortenError(copyErr, 25)))
		}
	} else {
		// Successfully copied
		pw.MarkFinished("") // Mark as finished with no error
	}
	appLogger.Printf("%s File copy process completed for '%s'. Final status IsFinished: %t, ErrorMsg: '%s'", logPrefix, filePath, pw.IsFinished, pw.ErrorMsg)
}
