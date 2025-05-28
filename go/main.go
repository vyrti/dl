package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
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
	// Adjusted for speed display
	maxFilenameDisplayLength = 20
	progressBarWidth         = 25
	redrawInterval           = 150 * time.Millisecond
	speedUpdateInterval      = 750 * time.Millisecond // How often to recalculate speed for a bar
)

// --- Global Variables ---
var stdoutMutex sync.Mutex

// --- Helper Function ---
func formatSpeed(bytesPerSecond float64) string {
	if bytesPerSecond < 0 { // Can happen if current somehow decreases or time is weird
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

// --- ProgressWriter ---
type ProgressWriter struct {
	id                   int
	FileName             string
	Total                int64
	Current              int64
	IsFinished           bool
	ErrorMsg             string
	mu                   sync.Mutex
	manager              *ProgressManager
	lastSpeedCalcTime    time.Time
	lastSpeedCalcCurrent int64
	currentSpeedBps      float64 // Bytes per second
}

func newProgressWriter(id int, fileName string, totalSize int64, manager *ProgressManager) *ProgressWriter {
	displayFileName := fileName
	if len(fileName) > maxFilenameDisplayLength {
		displayFileName = "..." + fileName[len(fileName)-maxFilenameDisplayLength+3:]
	}
	return &ProgressWriter{
		id:                   id,
		FileName:             displayFileName,
		Total:                totalSize,
		manager:              manager,
		lastSpeedCalcTime:    time.Now(), // Initialize for first speed calc
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
	// Speed is updated by ProgressManager's redrawLoop periodically
	pw.mu.Unlock()
	pw.manager.requestRedraw()
	return n, nil
}

func (pw *ProgressWriter) UpdateSpeed() {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	if pw.IsFinished {
		// pw.currentSpeedBps = 0 // Already handled in MarkFinished
		return
	}

	now := time.Now()
	elapsed := now.Sub(pw.lastSpeedCalcTime)

	// Only update if enough time has passed or it's near completion (for a final accurate speed)
	if elapsed < speedUpdateInterval && (pw.Total <= 0 || pw.Current < pw.Total) {
		return
	}

	if elapsed.Seconds() < 0.05 { // Avoid division by zero or extremely small intervals if called rapidly
		return
	}

	bytesDownloadedInInterval := pw.Current - pw.lastSpeedCalcCurrent
	if bytesDownloadedInInterval < 0 {
		bytesDownloadedInInterval = 0
	} // Safety

	pw.currentSpeedBps = float64(bytesDownloadedInInterval) / elapsed.Seconds()

	pw.lastSpeedCalcTime = now
	pw.lastSpeedCalcCurrent = pw.Current
}

func (pw *ProgressWriter) MarkFinished(errMsg string) {
	pw.mu.Lock()
	pw.IsFinished = true
	pw.ErrorMsg = errMsg
	pw.currentSpeedBps = 0 // Explicitly set speed to 0 on finish

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
	if isFinished && errorMsg == "" {
		speedStr = "Done    " // Pad to align with speed format
	} else if isFinished && errorMsg != "" {
		speedStr = "Error   "
	}

	// Calculate ETA if not finished
	var etaStr string
	if !isFinished {
		etaStr = calculateETA(currentSpeed, total, current)
	} else {
		etaStr = "N/A"
	}

	if isFinished {
		if errorMsg != "" {
			maxErrorDisplay := progressBarWidth + 20 // Approximate
			displayError := errorMsg
			if len(displayError) > maxErrorDisplay {
				displayError = displayError[:maxErrorDisplay-3] + "..."
			}
			return fmt.Sprintf("%-*s: [ERROR: %s] ETA: %s", maxFilenameDisplayLength, fileName, displayError, etaStr)
		}
		percentage := 100.0
		bar := strings.Repeat("=", progressBarWidth)
		currentMB := float64(current) / (1024 * 1024)
		return fmt.Sprintf("%-*s: [%s] %6.2f%% (%6.2f MB) @ %s ETA: %s",
			maxFilenameDisplayLength, fileName, bar, percentage, currentMB, speedStr, etaStr)
	}

	percentage := 0.0
	bar := strings.Repeat(" ", progressBarWidth)
	indeterminate := false

	if total > 0 {
		percentage = (float64(current) / float64(total)) * 100
		if percentage > 100 {
			percentage = 100
		} // Cap at 100
		if percentage < 0 {
			percentage = 0
		} // Floor at 0

		filledWidth := int(float64(progressBarWidth) * percentage / 100.0)
		if filledWidth > progressBarWidth {
			filledWidth = progressBarWidth
		}
		bar = strings.Repeat("=", filledWidth)
		// Add '>' if not full and some progress made or bar is empty
		if filledWidth < progressBarWidth && (filledWidth > 0 || percentage > 0) {
			if filledWidth > 0 { // If bar has content, replace last char
				bar = bar[:len(bar)-1] + ">"
			} else { // If bar is empty but there's some percentage, just add ">"
				bar = ">"
			}
		}
		bar += strings.Repeat(" ", progressBarWidth-len(bar))

	} else {
		indeterminate = true
		spinChars := []string{"|", "/", "-", "\\"}
		spinner := spinChars[int(time.Now().UnixNano()/(100*int64(time.Millisecond)))%len(spinChars)]
		mid := progressBarWidth / 2
		barRunes := []rune(strings.Repeat(" ", progressBarWidth))
		if mid > 0 && mid <= len(barRunes) {
			barRunes[mid-1] = []rune(spinner)[0]
		}
		bar = string(barRunes)
	}

	currentMB := float64(current) / (1024 * 1024)
	totalMBStr := "???.?? MB"
	if total > 0 {
		totalMBStr = fmt.Sprintf("%.2f MB", float64(total)/(1024*1024))
	}

	if indeterminate {
		return fmt.Sprintf("%-*s: [%s] (%6.2f MB / unknown) @ %s ETA: %s",
			maxFilenameDisplayLength, fileName, bar, currentMB, speedStr, etaStr)
	}
	return fmt.Sprintf("%-*s: [%s] %6.2f%% (%6.2f MB / %s) @ %s ETA: %s",
		maxFilenameDisplayLength, fileName, bar, percentage, currentMB, totalMBStr, speedStr, etaStr)
}

func calculateETA(speedBps float64, totalSize int64, currentSize int64) string {
	if speedBps <= 0 || totalSize <= 0 || currentSize >= totalSize {
		return "N/A"
	}

	remainingBytes := totalSize - int64(currentSize)
	remainingSeconds := float64(remainingBytes) / speedBps

	// Format the ETA
	if remainingSeconds < 60 {
		return fmt.Sprintf("%.0f sec", remainingSeconds)
	} else if remainingSeconds < 3600 {
		minutes := remainingSeconds / 60
		seconds := math.Mod(remainingSeconds, 60)
		return fmt.Sprintf("%.0f min %.0f sec", minutes, seconds)
	} else {
		hours := remainingSeconds / 3600
		minutes := math.Mod(remainingSeconds, 3600) / 60
		seconds := math.Mod(remainingSeconds, 60)
		return fmt.Sprintf("%.0f hr %.0f min %.0f sec", hours, minutes, seconds)
	}
}

// --- ProgressManager ---
type ProgressManager struct {
	bars          []*ProgressWriter
	mu            sync.Mutex
	linesPrinted  int
	redrawPending bool
	stopRedraw    chan struct{}
	wg            sync.WaitGroup
}

func NewProgressManager() *ProgressManager {
	m := &ProgressManager{
		bars:       make([]*ProgressWriter, 0),
		stopRedraw: make(chan struct{}),
	}
	m.wg.Add(1)
	go m.redrawLoop()
	return m
}

func (m *ProgressManager) AddDownload(fileName string, totalSize int64) *ProgressWriter {
	m.mu.Lock()
	defer m.mu.Unlock()
	pw := newProgressWriter(len(m.bars), fileName, totalSize, m)
	m.bars = append(m.bars, pw)
	m.redrawPending = true
	return pw
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
	fmt.Print("\033[?25l") // Hide cursor
	stdoutMutex.Unlock()

	defer func() {
		m.performActualDraw(true) // Final draw
		stdoutMutex.Lock()
		fmt.Print("\033[?25h") // Show cursor
		stdoutMutex.Unlock()
	}()

	for {
		forceRedrawThisCycle := false
		select {
		case <-m.stopRedraw:
			return
		case <-ticker.C:
			forceRedrawThisCycle = true
		}

		m.mu.Lock()
		// Update speeds for all active bars
		for _, bar := range m.bars {
			bar.UpdateSpeed() // This call is internally rate-limited by speedUpdateInterval
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
			m.performActualDraw(false)
		}
	}
}

func (m *ProgressManager) hasIndeterminateOrActiveBarsLocked() bool {
	for _, bar := range m.bars {
		bar.mu.Lock()
		isActive := !bar.IsFinished
		isIndeterminate := isActive && bar.Total == -1
		bar.mu.Unlock()
		if isIndeterminate || (isActive && bar.currentSpeedBps > 0) { // Redraw if indeterminate or active with speed
			return true
		}
	}
	return false
}

func (m *ProgressManager) performActualDraw(isFinalDraw bool) {
	m.mu.Lock()
	barsSnapshot := make([]*ProgressWriter, len(m.bars))
	for i, b := range m.bars {
		if isFinalDraw {
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
		barsSnapshot[i] = b
	}
	previousLines := m.linesPrinted
	m.mu.Unlock()

	stdoutMutex.Lock()
	defer stdoutMutex.Unlock()

	if previousLines > 0 {
		fmt.Printf("\033[%dA", previousLines) // Move cursor up
	}

	currentLinesDrawn := 0
	for _, bar := range barsSnapshot {
		progressString := bar.getProgressString()
		fmt.Printf("\033[2K%s\n", progressString) // Clear line, print, newline
		currentLinesDrawn++
	}

	if currentLinesDrawn < previousLines {
		for i := currentLinesDrawn; i < previousLines; i++ {
			fmt.Print("\033[2K\n")
		}
		if currentLinesDrawn > 0 {
			fmt.Printf("\033[%dA", previousLines-currentLinesDrawn)
		}
	}
	os.Stdout.Sync()

	m.mu.Lock()
	m.linesPrinted = currentLinesDrawn
	m.mu.Unlock()
}

func (m *ProgressManager) Stop() {
	close(m.stopRedraw)
	m.wg.Wait()
}

// --- Download Logic & Main ---
func downloadFile(url string, wg *sync.WaitGroup, downloadDir string, manager *ProgressManager) {
	defer wg.Done()

	fileName := filepath.Base(url)
	const suffixToRemove = "?download=true"
	if strings.HasSuffix(fileName, suffixToRemove) {
		fileName = strings.TrimSuffix(fileName, suffixToRemove)
	}

	if fileName == "." || fileName == "/" || fileName == "" {
		fileName = "download_" + strconv.FormatInt(time.Now().UnixNano(), 16)[:8]
		originalBase := filepath.Base(url)
		if strings.HasSuffix(originalBase, suffixToRemove) {
			originalBase = strings.TrimSuffix(originalBase, suffixToRemove)
		}
		if ext := filepath.Ext(originalBase); ext != "" && len(ext) < 6 && !strings.Contains(ext, "?") {
			fileName += ext
		} else if ext := filepath.Ext(fileName); ext == "" {
			fileName += ".file"
		}
	}
	filePath := filepath.Join(downloadDir, fileName)

	var initialTotalSize int64 = -1
	headResp, headErr := http.Head(url)
	if headErr == nil {
		if headResp.StatusCode == http.StatusOK {
			initialTotalSize = headResp.ContentLength
		}
		if headResp.Body != nil {
			headResp.Body.Close()
		}
	}

	pw := manager.AddDownload(fileName, initialTotalSize)

	out, createErr := os.Create(filePath)
	if createErr != nil {
		pw.MarkFinished(fmt.Sprintf("Create file: %v", shortenError(createErr, 15)))
		return
	}
	defer out.Close()

	resp, getErr := http.Get(url)
	if getErr != nil {
		pw.MarkFinished(fmt.Sprintf("GET: %v", shortenError(getErr, 15)))
		os.Remove(filePath)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		pw.MarkFinished(fmt.Sprintf("HTTP: %s", resp.Status))
		os.Remove(filePath)
		return
	}

	pw.mu.Lock()
	if pw.Total == -1 || (resp.ContentLength > 0 && pw.Total != resp.ContentLength) {
		pw.Total = resp.ContentLength
	}
	pw.mu.Unlock()
	manager.requestRedraw()

	_, copyErr := io.Copy(out, io.TeeReader(resp.Body, pw))
	if copyErr != nil {
		pw.MarkFinished(fmt.Sprintf("Copy: %v", shortenError(copyErr, 15)))
	} else {
		pw.MarkFinished("")
	}
}

func shortenError(err error, maxLen int) string {
	s := err.Error()
	if len(s) > maxLen {
		return s[:maxLen-3] + "..."
	}
	return s
}

func main() {
	var concurrency int
	flag.IntVar(&concurrency, "c", 3, "Number of concurrent downloads")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Println("Usage: dl -c 6 filelist.txt")
		os.Exit(1)
	}
	urlsFilePath := flag.Arg(0)

	file, err := os.Open(urlsFilePath)
	if err != nil {
		fmt.Printf("Error opening file %s: %v\n", urlsFilePath, err)
		os.Exit(1)
	}
	defer file.Close()

	var urls []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		url := strings.TrimSpace(scanner.Text())
		if url != "" {
			urls = append(urls, url)
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Printf("Error reading file %s: %v\n", urlsFilePath, err)
		os.Exit(1)
	}

	if len(urls) == 0 {
		fmt.Println("No URLs found in the file.")
		os.Exit(0)
	}

	downloadDir := "downloads"
	if _, err := os.Stat(downloadDir); os.IsNotExist(err) {
		if mkDirErr := os.Mkdir(downloadDir, 0755); mkDirErr != nil {
			fmt.Printf("Error creating download directory %s: %v\n", downloadDir, mkDirErr)
			os.Exit(1)
		}
	}

	manager := NewProgressManager()

	stdoutMutex.Lock()
	fmt.Printf("Attempting to download %d files from '%s' into '%s' (concurrency: %d)...\n",
		len(urls), urlsFilePath, downloadDir, concurrency)
	stdoutMutex.Unlock()

	var downloadWG sync.WaitGroup
	sem := make(chan struct{}, concurrency) // Semaphore to limit concurrency

	for _, url := range urls {
		sem <- struct{}{} // Acquire semaphore slot
		downloadWG.Add(1)
		go func(u string) {
			defer func() { <-sem }() // Release semaphore slot
			downloadFile(u, &downloadWG, downloadDir, manager)
		}(url)
	}

	downloadWG.Wait()
	manager.Stop()

	stdoutMutex.Lock()
	fmt.Printf("\nAll %d download tasks have been processed.\n", len(urls))
	stdoutMutex.Unlock()
}
