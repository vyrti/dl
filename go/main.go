package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxFilenameDisplayLength = 20
	progressBarWidth         = 25
	redrawInterval           = 150 * time.Millisecond
	speedUpdateInterval      = 750 * time.Millisecond
)

var stdoutMutex sync.Mutex
var appLogger *log.Logger
var logFile *os.File
var debugMode bool // To be set by command-line flag

func initLogging() {
	if debugMode {
		var err error
		logFile, err = os.OpenFile("log.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			// If debug logging fails, log to stderr and continue without file logging.
			// We don't want to FATAL exit just because debug log file failed.
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
		id:                   id,
		URL:                  url,
		FileName:             displayFileName,
		ActualFileName:       actualFileName,
		Total:                totalSize,
		manager:              manager,
		lastSpeedCalcTime:    time.Now(),
		lastSpeedCalcCurrent: 0,
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

	current := pw.Current
	total := pw.Total
	isFinished := pw.IsFinished
	errorMsg := pw.ErrorMsg
	fileName := pw.FileName
	currentSpeed := pw.currentSpeedBps

	speedStr := formatSpeed(currentSpeed)
	etaStr := "N/A"

	if isFinished {
		if errorMsg == "" {
			speedStr = "Done    "
			etaStr = ""
		} else {
			speedStr = "Error   "
		}
	} else {
		if total <= 0 && current == 0 && !isFinished {
			speedStr = "Pending "
		} else if currentSpeed > 0 && total > 0 && current < total {
			etaStr = calculateETA(currentSpeed, total, current)
		} else if total > 0 && current == 0 && !isFinished {
			speedStr = "Waiting "
		}
	}

	if isFinished {
		if errorMsg != "" {
			maxErrorDisplay := progressBarWidth + 20
			displayError := errorMsg
			runes := []rune(displayError)
			if len(runes) > maxErrorDisplay {
				if maxErrorDisplay <= 3 {
					displayError = string(runes[:maxErrorDisplay])
				} else {
					displayError = string(runes[:maxErrorDisplay-3]) + "..."
				}
			}
			return fmt.Sprintf("%-*s: [ERROR: %s]", maxFilenameDisplayLength, fileName, displayError)
		}
		percentage := 100.0
		bar := strings.Repeat("=", progressBarWidth)
		currentMB := float64(current) / (1024 * 1024)
		return fmt.Sprintf("%-*s: [%s] %6.2f%% (%6.2f MB) @ %s",
			maxFilenameDisplayLength, fileName, bar, percentage, currentMB, speedStr)
	}

	percentage := 0.0
	barFill := ""
	indeterminate := false

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

		barContentRunes := []rune(strings.Repeat(" ", progressBarWidth))
		for i := 0; i < filledWidth; i++ {
			if i < progressBarWidth {
				barContentRunes[i] = '='
			}
		}
		if filledWidth < progressBarWidth && percentage < 100.0 && percentage >= 0.0 {
			idxForArrow := filledWidth
			if idxForArrow >= 0 && idxForArrow < progressBarWidth {
				barContentRunes[idxForArrow] = '>'
			}
		}
		barFill = string(barContentRunes)
	} else {
		if current > 0 {
			indeterminate = true
			spinChars := []string{"|", "/", "-", "\\"}
			spinner := spinChars[int(time.Now().UnixNano()/(int64(redrawInterval)/int64(len(spinChars))))%len(spinChars)]
			mid := progressBarWidth / 2
			barRunes := []rune(strings.Repeat(" ", progressBarWidth))
			idxToPlaceSpinner := maxInt(0, mid-1)
			if progressBarWidth > 0 && idxToPlaceSpinner < progressBarWidth {
				barRunes[idxToPlaceSpinner] = []rune(spinner)[0]
			} else if progressBarWidth > 0 {
				barRunes[0] = []rune(spinner)[0]
			}
			barFill = string(barRunes)
		} else {
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
		return fmt.Sprintf("%-*s: %s (%6.2f MB / unknown) @ %s ETA: %s",
			maxFilenameDisplayLength, fileName, bar, currentMB, speedStr, etaStr)
	}
	return fmt.Sprintf("%-*s: %s %6.2f%% (%6.2f MB / %s) @ %s ETA: %s",
		maxFilenameDisplayLength, fileName, bar, percentage, currentMB, totalMBStr, speedStr, etaStr)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func calculateETA(speedBps float64, totalSize int64, currentSize int64) string {
	if speedBps <= 0 || totalSize <= 0 || currentSize >= totalSize {
		return "N/A"
	}

	remainingBytes := totalSize - currentSize
	remainingSeconds := float64(remainingBytes) / speedBps

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

type ProgressManager struct {
	bars               []*ProgressWriter
	mu                 sync.Mutex
	redrawPending      bool
	stopRedraw         chan struct{}
	wg                 sync.WaitGroup
	displayConcurrency int // Number of bars to display, same as download concurrency
}

func NewProgressManager(displayConcurrency int) *ProgressManager {
	m := &ProgressManager{
		bars:               make([]*ProgressWriter, 0),
		stopRedraw:         make(chan struct{}),
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

func (m *ProgressManager) GetBarCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bars)
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
	stdoutMutex.Unlock()

	defer func() {
		m.performActualDraw(true) // Final draw shows all bars
		stdoutMutex.Lock()
		fmt.Print("\033[?25h")
		fmt.Println()
		stdoutMutex.Unlock()
	}()

	for {
		forceRedrawThisCycle := false
		select {
		case <-m.stopRedraw:
			appLogger.Println("[PM.redrawLoop] Stop signal received. Exiting loop.")
			return
		case <-ticker.C:
			forceRedrawThisCycle = true
		}

		m.mu.Lock()
		for _, bar := range m.bars {
			if !bar.IsFinished && (bar.Current > 0 || bar.Total > 0) {
				bar.UpdateSpeed()
			}
		}

		if m.redrawPending {
			forceRedrawThisCycle = true
			m.redrawPending = false
		}
		if !forceRedrawThisCycle && m.hasIndeterminateOrActiveBarsLocked() {
			forceRedrawThisCycle = true
		}
		m.mu.Unlock()

		if forceRedrawThisCycle {
			m.performActualDraw(false) // Regular draw respects displayConcurrency
		}
	}
}

func (m *ProgressManager) hasIndeterminateOrActiveBarsLocked() bool {
	for _, bar := range m.bars {
		bar.mu.Lock()
		isActive := !bar.IsFinished
		isIndeterminate := isActive && bar.Current > 0 && bar.Total <= 0
		isSimplyActive := isActive && bar.Current > 0
		bar.mu.Unlock()
		if isIndeterminate || isSimplyActive {
			return true
		}
	}
	return false
}

func (m *ProgressManager) getOverallProgressString(barsSnapshot []*ProgressWriter) string {
	var totalCurrentBytes int64
	var totalExpectedBytes int64
	var overallSpeedBps float64
	allPreScannedAndFinished := true
	totalFiles := len(barsSnapshot)
	finishedFiles := 0
	activeDownloads := 0

	for _, bar := range barsSnapshot {
		bar.mu.Lock()
		totalCurrentBytes += bar.Current
		if bar.Total > 0 {
			totalExpectedBytes += bar.Total
		}
		if !bar.IsFinished && bar.Current > 0 {
			overallSpeedBps += bar.currentSpeedBps
			activeDownloads++
		}
		if !bar.IsFinished {
			allPreScannedAndFinished = false
		} else {
			finishedFiles++
		}
		bar.mu.Unlock()
	}

	percentage := 0.0
	if totalExpectedBytes > 0 {
		percentage = (float64(totalCurrentBytes) / float64(totalExpectedBytes)) * 100
		if percentage > 100 {
			percentage = 100
		}
		if percentage < 0 {
			percentage = 0
		}
	} else if allPreScannedAndFinished && totalFiles > 0 {
		percentage = 100.0
	}

	totalCurrentStr := fmt.Sprintf("%.2f MB", float64(totalCurrentBytes)/(1024*1024))
	totalExpectedStr := "???.?? MB"
	if totalExpectedBytes > 0 {
		totalExpectedStr = fmt.Sprintf("%.2f MB", float64(totalExpectedBytes)/(1024*1024))
	} else if allPreScannedAndFinished && totalFiles > 0 {
		totalExpectedStr = totalCurrentStr
	}

	overallSpeedStr := formatSpeed(overallSpeedBps)
	etaStr := "N/A"

	if !allPreScannedAndFinished && overallSpeedBps > 0 && totalExpectedBytes > 0 && totalCurrentBytes < totalExpectedBytes {
		remainingBytes := totalExpectedBytes - totalCurrentBytes
		if remainingBytes > 0 {
			etaStr = calculateETA(overallSpeedBps, totalExpectedBytes, totalCurrentBytes)
		}
	} else if allPreScannedAndFinished && totalFiles > 0 {
		etaStr = "Done"
		overallSpeedStr = "Completed"
	} else if activeDownloads == 0 && !allPreScannedAndFinished && totalFiles > 0 {
		overallSpeedStr = "Pending..."
	}

	barWidth := progressBarWidth + 10
	filledWidth := 0
	if (totalExpectedBytes > 0 || (allPreScannedAndFinished && totalFiles > 0)) && percentage > 0 {
		filledWidth = int(math.Round(float64(barWidth) * percentage / 100.0))
	}
	if filledWidth > barWidth {
		filledWidth = barWidth
	}
	if filledWidth < 0 {
		filledWidth = 0
	}
	overallBar := "[" + strings.Repeat("=", filledWidth) + strings.Repeat(" ", barWidth-filledWidth) + "]"

	return fmt.Sprintf("Overall %-*s %6.2f%% (%s / %s) @ %s ETA: %s (%d/%d files)",
		barWidth+1,
		overallBar,
		percentage,
		totalCurrentStr,
		totalExpectedStr,
		overallSpeedStr,
		etaStr,
		finishedFiles,
		totalFiles,
	)
}

func (m *ProgressManager) performActualDraw(isFinalDraw bool) {
	m.mu.Lock()
	barsSnapshot := make([]*ProgressWriter, len(m.bars))
	copy(barsSnapshot, m.bars)
	m.mu.Unlock()

	appLogger.Printf("[PM.performActualDraw] Drawing %d bars. IsFinalDraw: %t. DisplayConcurrency: %d",
		len(barsSnapshot), isFinalDraw, m.displayConcurrency)

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
		// For final draw, show all bars to give a complete summary
		barsToDisplay = barsSnapshot
		appLogger.Printf("[PM.performActualDraw] Final draw: showing all %d bars.", len(barsToDisplay))
	} else {
		// Select bars based on displayConcurrency for regular updates
		activeBars := make([]*ProgressWriter, 0)
		pendingBars := make([]*ProgressWriter, 0)

		for _, bar := range barsSnapshot {
			bar.mu.Lock()
			isFin := bar.IsFinished
			curr := bar.Current
			bar.mu.Unlock()

			if !isFin {
				if curr > 0 { // Active download
					activeBars = append(activeBars, bar)
				} else { // Pending download
					pendingBars = append(pendingBars, bar)
				}
			}
		}

		// Prioritize active bars
		for _, bar := range activeBars {
			if len(barsToDisplay) < m.displayConcurrency {
				barsToDisplay = append(barsToDisplay, bar)
			} else {
				break
			}
		}

		// Fill with pending bars if space allows
		// This ensures that if active < displayConcurrency, pending tasks are shown
		if len(barsToDisplay) < m.displayConcurrency {
			for _, bar := range pendingBars {
				if len(barsToDisplay) < m.displayConcurrency {
					barsToDisplay = append(barsToDisplay, bar)
				} else {
					break
				}
			}
		}
		appLogger.Printf("[PM.performActualDraw] Regular draw: selected %d active, %d pending. Displaying %d bars (limit %d).",
			len(activeBars), len(pendingBars), len(barsToDisplay), m.displayConcurrency)
	}

	for _, bar := range barsToDisplay {
		fmt.Println(bar.getProgressString())
	}

	if len(barsSnapshot) > 0 {
		fmt.Println(strings.Repeat("-", 80))
		fmt.Println(m.getOverallProgressString(barsSnapshot))
	}

	os.Stdout.Sync()
}

func (m *ProgressManager) Stop() {
	appLogger.Println("[PM.Stop] Stop called. Closing stopRedraw channel.")
	close(m.stopRedraw)
	appLogger.Println("[PM.Stop] Waiting for redrawLoop to finish.")
	m.wg.Wait()
	appLogger.Println("[PM.Stop] RedrawLoop finished. ProgressManager stopped.")
}

func downloadFile(pw *ProgressWriter, wg *sync.WaitGroup, downloadDir string, manager *ProgressManager) {
	logPrefix := fmt.Sprintf("[downloadFile:%s]", pw.URL)
	appLogger.Printf("%s Goroutine started for pre-initialized bar (File: %s).", logPrefix, pw.ActualFileName)
	defer func() {
		appLogger.Printf("%s Goroutine finishing (File: %s).", logPrefix, pw.ActualFileName)
		wg.Done()
	}()

	filePath := filepath.Join(downloadDir, pw.ActualFileName)

	client := http.Client{}
	req, err := http.NewRequest("GET", pw.URL, nil)
	if err != nil {
		errMsg := fmt.Sprintf("Req create: %v", shortenError(err, 25))
		appLogger.Printf("%s Error: %s", logPrefix, errMsg)
		pw.MarkFinished(errMsg)
		return
	}
	req.Header.Set("User-Agent", "Go-File-Downloader/1.0 (GET)")

	resp, getErr := client.Do(req)
	if getErr != nil {
		errMsg := fmt.Sprintf("GET: %v", shortenError(getErr, 25))
		appLogger.Printf("%s Error: %s", logPrefix, errMsg)
		pw.MarkFinished(errMsg)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errMsg := fmt.Sprintf("HTTP %s", resp.Status)
		appLogger.Printf("%s Error: %s", logPrefix, errMsg)
		pw.MarkFinished(errMsg)
		return
	}
	appLogger.Printf("%s GET successful. Status: %s. ContentLength: %d", logPrefix, resp.Status, resp.ContentLength)

	pw.mu.Lock()
	if resp.ContentLength > 0 && (pw.Total <= 0 || pw.Total != resp.ContentLength) {
		appLogger.Printf("%s Updating total size from %d to %d (from GET Content-Length)", logPrefix, pw.Total, resp.ContentLength)
		pw.Total = resp.ContentLength
	} else if pw.Total <= 0 && resp.ContentLength <= 0 {
		appLogger.Printf("%s Total size remains unknown after GET.", logPrefix)
	}
	pw.mu.Unlock()
	manager.requestRedraw()

	out, createErr := os.Create(filePath)
	if createErr != nil {
		errMsg := fmt.Sprintf("Create file: %v", shortenError(createErr, 25))
		appLogger.Printf("%s Error creating file '%s': %s", logPrefix, filePath, errMsg)
		pw.MarkFinished(errMsg)
		return
	}
	defer out.Close()

	appLogger.Printf("%s Starting copy to '%s'", logPrefix, filePath)
	_, copyErr := io.Copy(out, io.TeeReader(resp.Body, pw))
	if copyErr != nil {
		pw.mu.Lock()
		alreadyMarkedFinished := pw.IsFinished
		pw.mu.Unlock()

		if alreadyMarkedFinished && copyErr == io.EOF {
			appLogger.Printf("%s Copy interrupted as progress writer was already marked finished. URL: %s", logPrefix, pw.URL)
		} else {
			errMsg := fmt.Sprintf("Copy: %v", shortenError(copyErr, 25))
			appLogger.Printf("%s Error copying data: %s", logPrefix, errMsg)
			pw.MarkFinished(errMsg)
		}
	} else {
		appLogger.Printf("%s Successfully copied data for '%s'", logPrefix, pw.ActualFileName)
		pw.MarkFinished("")
	}
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
	fileName := filepath.Base(urlStr)
	const suffixToRemove = "?download=true"
	if strings.HasSuffix(fileName, suffixToRemove) {
		fileName = strings.TrimSuffix(fileName, suffixToRemove)
	}

	if fileName == "." || fileName == "/" || fileName == "" {
		base := "download_" + strconv.FormatInt(time.Now().UnixNano(), 16)[:8]
		originalBaseName := filepath.Base(urlStr)
		if strings.HasSuffix(originalBaseName, suffixToRemove) {
			originalBaseName = strings.TrimSuffix(originalBaseName, suffixToRemove)
		}
		ext := filepath.Ext(originalBaseName)
		if ext != "" && len(ext) > 1 && len(ext) < 7 && !strings.ContainsAny(ext, "?&=/:\\*\"<>|") {
			fileName = base + ext
		} else {
			fileName = base + ".file"
		}
	}
	return fileName
}

func main() {
	var concurrency int
	var urlsFilePath string

	flag.BoolVar(&debugMode, "debug", false, "Enable debug logging to log.log")
	flag.IntVar(&concurrency, "c", 3, "Number of concurrent downloads (and lines to display)")
	flag.StringVar(&urlsFilePath, "f", "", "Path to the text file containing URLs (required)")
	flag.Parse()

	initLogging() // Initialize logging based on debugMode
	defer func() {
		if logFile != nil { // Only close if it was opened
			appLogger.Println("---------------- Logging Finished (Debug Mode) -----------------")
			logFile.Close()
		}
	}()
	appLogger.Println("Application starting...")
	appLogger.Printf("Flags parsed. DebugMode: %t, Download Concurrency/Display Lines: %d, FilePath: '%s'",
		debugMode, concurrency, urlsFilePath)

	if urlsFilePath == "" {
		appLogger.Println("Error: -f flag (file path) is required.")
		fmt.Fprintln(os.Stderr, "Error: -f flag (file path) is required.")
		flag.Usage()
		os.Exit(1)
	}

	if concurrency <= 0 {
		appLogger.Printf("Error: concurrency (-c) must be a positive integer. Got: %d", concurrency)
		fmt.Fprintf(os.Stderr, "Error: concurrency (-c) must be a positive integer. Got: %d\n", concurrency)
		os.Exit(1)
	}

	file, err := os.Open(urlsFilePath)
	if err != nil {
		appLogger.Printf("Error opening file '%s': %v", urlsFilePath, err)
		fmt.Fprintf(os.Stderr, "Error opening file '%s': %v\n", urlsFilePath, err)
		os.Exit(1)
	}
	defer file.Close()

	var urls []string
	scanner := bufio.NewScanner(file)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		url := strings.TrimSpace(scanner.Text())
		if url != "" {
			urls = append(urls, url)
		}
	}
	if err := scanner.Err(); err != nil {
		appLogger.Printf("Error reading file '%s' (around line %d): %v", urlsFilePath, lineNum, err)
		if err == bufio.ErrTooLong {
			appLogger.Println("Hint: A line in the URL file might be too long for the scanner's default buffer.")
		}
		fmt.Fprintf(os.Stderr, "Error reading file '%s' (around line %d): %v\n", urlsFilePath, lineNum, err)
		os.Exit(1)
	}

	appLogger.Printf("Read %d URLs from file '%s'.", len(urls), urlsFilePath)
	fmt.Fprintf(os.Stderr, "[INFO] Read %d URLs from file '%s'.\n", len(urls), urlsFilePath)

	if len(urls) == 0 {
		appLogger.Println("No URLs found in the file. Exiting.")
		fmt.Println("No URLs found in the file.")
		os.Exit(0)
	}

	downloadDir := "downloads"
	if _, err := os.Stat(downloadDir); os.IsNotExist(err) {
		appLogger.Printf("Download directory '%s' does not exist. Creating.", downloadDir)
		if mkDirErr := os.Mkdir(downloadDir, 0755); mkDirErr != nil {
			appLogger.Printf("Error creating download directory '%s': %v", downloadDir, mkDirErr)
			fmt.Fprintf(os.Stderr, "Error creating download directory '%s': %v\n", downloadDir, mkDirErr)
			os.Exit(1)
		}
	}

	manager := NewProgressManager(concurrency) // Pass concurrency for display limit

	appLogger.Printf("Starting pre-scan for %d URLs to get initial sizes...", len(urls))
	fmt.Fprintf(os.Stderr, "[INFO] Pre-scanning %d URLs for sizes (this may take a moment)...\n", len(urls))

	allProgressWriters := make([]*ProgressWriter, len(urls))
	var preScanWG sync.WaitGroup
	preScanConcurrency := 20
	preScanSem := make(chan struct{}, preScanConcurrency)

	for i, urlStr := range urls {
		preScanWG.Add(1)
		preScanSem <- struct{}{}
		go func(idx int, u string) {
			defer func() {
				<-preScanSem
				preScanWG.Done()
			}()
			logPrefix := fmt.Sprintf("[PreScan:%s]", u)
			actualFilename := generateActualFilename(u)
			var initialSize int64 = -1

			headReq, _ := http.NewRequest("HEAD", u, nil)
			headReq.Header.Set("User-Agent", "Go-File-Downloader/1.0 (PreScan-HEAD)")
			headClient := http.Client{Timeout: 10 * time.Second}
			headResp, headErr := headClient.Do(headReq)

			if headErr == nil && headResp.StatusCode == http.StatusOK {
				initialSize = headResp.ContentLength
				appLogger.Printf("%s HEAD success. Size: %d for %s", logPrefix, initialSize, actualFilename)
				if headResp.Body != nil {
					headResp.Body.Close()
				}
			} else if headErr != nil {
				appLogger.Printf("%s HEAD error: %v for %s. Size will be unknown.", logPrefix, headErr, actualFilename)
			} else {
				appLogger.Printf("%s HEAD non-OK status: %s for %s. Size will be unknown.", logPrefix, headResp.Status, actualFilename)
				if headResp.Body != nil {
					headResp.Body.Close()
				}
			}
			allProgressWriters[idx] = newProgressWriter(idx, u, actualFilename, initialSize, manager)
		}(i, urlStr)
	}
	preScanWG.Wait()
	close(preScanSem)
	appLogger.Println("Pre-scan finished.")
	fmt.Fprintln(os.Stderr, "[INFO] Pre-scan complete.")

	manager.AddInitialDownloads(allProgressWriters)

	appLogger.Printf("Attempting to download %d files from '%s' into '%s' (download concurrency/display lines: %d).",
		len(urls), urlsFilePath, downloadDir, concurrency)
	fmt.Fprintf(os.Stderr, "[INFO] Starting downloads for %d files (download concurrency/display lines: %d).\n",
		len(urls), concurrency)

	var downloadWG sync.WaitGroup
	downloadSem := make(chan struct{}, concurrency) // Semaphore for actual downloads

	appLogger.Printf("[MainDownloadLoop] Starting to launch %d download goroutines.", len(allProgressWriters))
	for i, pw := range allProgressWriters {
		appLogger.Printf("[MainDownloadLoop] Index %d: Preparing for URL: %s (File: %s)", i, pw.URL, pw.ActualFileName)
		downloadSem <- struct{}{}
		appLogger.Printf("[MainDownloadLoop] Index %d: Acquired download semaphore for URL: %s", i, pw.URL)
		downloadWG.Add(1)
		go func(pWriter *ProgressWriter) {
			defer func() {
				appLogger.Printf("[downloadFile:%s] Releasing download semaphore.", pWriter.URL)
				<-downloadSem
			}()
			downloadFile(pWriter, &downloadWG, downloadDir, manager)
		}(pw)
		appLogger.Printf("[MainDownloadLoop] Index %d: Launched download goroutine for URL: %s", i, pw.URL)
	}
	appLogger.Printf("[MainDownloadLoop] All %d download goroutines launched. Waiting on WaitGroup.", len(allProgressWriters))

	downloadWG.Wait()
	appLogger.Println("[MainDownloadLoop] WaitGroup finished. All download goroutines completed.")
	manager.Stop()

	appLogger.Printf("All %d download tasks have been processed. Application exiting.", len(urls))
	fmt.Printf("All %d download tasks have been processed.\n", len(urls))
}
