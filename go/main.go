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
	maxFilenameDisplayLength = 20
	progressBarWidth         = 25
	redrawInterval           = 150 * time.Millisecond
	speedUpdateInterval      = 750 * time.Millisecond
)

var stdoutMutex sync.Mutex // This mutex will be used by performActualDraw

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
	FileName             string
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

// MODIFIED getProgressString
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
	etaStr := "N/A" // Default ETA string

	if isFinished {
		if errorMsg == "" { // Finished and NO error
			speedStr = "Done    "
			etaStr = "" // Clear ETA string when finished and successful
		} else { // Finished WITH an error
			speedStr = "Error   "
			// etaStr remains "N/A" for errors. Could be set to "" if desired.
		}
	} else { // Not finished
		if currentSpeed > 0 && total > 0 && current < total {
			etaStr = calculateETA(currentSpeed, total, current)
		}
	}

	if isFinished {
		if errorMsg != "" {
			maxErrorDisplay := progressBarWidth + 20
			displayError := errorMsg
			if len(displayError) > maxErrorDisplay {
				displayError = displayError[:maxErrorDisplay-3] + "..."
			}
			if etaStr != "" {
				return fmt.Sprintf("%-*s: [ERROR: %s] ETA: %s", maxFilenameDisplayLength, fileName, displayError, etaStr)
			}
			return fmt.Sprintf("%-*s: [ERROR: %s]", maxFilenameDisplayLength, fileName, displayError)
		}
		// Successfully finished
		percentage := 100.0
		bar := strings.Repeat("=", progressBarWidth)
		currentMB := float64(current) / (1024 * 1024)
		if etaStr != "" { // Should be false here as etaStr is "" for success
			return fmt.Sprintf("%-*s: [%s] %6.2f%% (%6.2f MB) @ %s ETA: %s",
				maxFilenameDisplayLength, fileName, bar, percentage, currentMB, speedStr, etaStr)
		}
		return fmt.Sprintf("%-*s: [%s] %6.2f%% (%6.2f MB) @ %s",
			maxFilenameDisplayLength, fileName, bar, percentage, currentMB, speedStr)
	}

	// For "Not Finished" lines
	percentage := 0.0
	barFill := strings.Repeat(" ", progressBarWidth)
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

		barContent := strings.Repeat("=", filledWidth)
		if filledWidth < progressBarWidth && filledWidth >= 0 && percentage < 100 {
			if filledWidth > 0 {
				barContent = barContent[:len(barContent)-1] + ">"
			} else if percentage > 0 {
				barContent = ">"
			}
		}
		barFill = barContent + strings.Repeat(" ", progressBarWidth-len(barContent))
	} else {
		indeterminate = true
		spinChars := []string{"|", "/", "-", "\\"}
		spinner := spinChars[int(time.Now().UnixNano()/(int64(redrawInterval)/2))%len(spinChars)]
		mid := progressBarWidth / 2
		barRunes := []rune(strings.Repeat(" ", progressBarWidth))
		if mid > 0 && mid <= len(barRunes) {
			barRunes[max(0, mid-1)] = []rune(spinner)[0]
		} else if len(barRunes) > 0 {
			barRunes[0] = []rune(spinner)[0]
		}
		barFill = string(barRunes)
	}
	bar := "[" + barFill + "]"

	currentMB := float64(current) / (1024 * 1024)
	totalMBStr := "???.?? MB"
	if total > 0 {
		totalMBStr = fmt.Sprintf("%.2f MB", float64(total)/(1024*1024))
	}

	// For "Not Finished" lines, always include ETA (which might be "N/A")
	if indeterminate {
		return fmt.Sprintf("%-*s: %s (%6.2f MB / unknown) @ %s ETA: %s",
			maxFilenameDisplayLength, fileName, bar, currentMB, speedStr, etaStr)
	}
	return fmt.Sprintf("%-*s: %s %6.2f%% (%6.2f MB / %s) @ %s ETA: %s",
		maxFilenameDisplayLength, fileName, bar, percentage, currentMB, totalMBStr, speedStr, etaStr)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func calculateETA(speedBps float64, totalSize int64, currentSize int64) string {
	if speedBps <= 0 || totalSize <= 0 || currentSize >= totalSize {
		return "N/A"
	}

	remainingBytes := totalSize - int64(currentSize)
	remainingSeconds := float64(remainingBytes) / speedBps

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

type ProgressManager struct {
	bars          []*ProgressWriter
	mu            sync.Mutex
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
		m.performActualDraw(true)
		stdoutMutex.Lock()
		fmt.Print("\033[?25h") // Show cursor
		fmt.Println()          // Ensure prompt is on a new line
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
		for _, bar := range m.bars {
			bar.UpdateSpeed()
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
		isIndeterminate := isActive && bar.Total <= 0
		bar.mu.Unlock()
		if isIndeterminate || isActive {
			return true
		}
	}
	return false
}

func (m *ProgressManager) getOverallProgressString(barsSnapshot []*ProgressWriter) string {
	var totalCurrentBytes int64
	var totalExpectedBytes int64
	var overallSpeedBps float64
	allFinished := true
	totalFiles := len(barsSnapshot)
	finishedFiles := 0

	for _, bar := range barsSnapshot {
		bar.mu.Lock()
		totalCurrentBytes += bar.Current
		if bar.Total > 0 {
			totalExpectedBytes += bar.Total
		}
		overallSpeedBps += bar.currentSpeedBps
		if !bar.IsFinished {
			allFinished = false
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
	} else if allFinished && totalFiles > 0 {
		percentage = 100.0
	}

	totalCurrentStr := fmt.Sprintf("%.2f MB", float64(totalCurrentBytes)/(1024*1024))
	totalExpectedStr := "???.?? MB"
	if totalExpectedBytes > 0 {
		totalExpectedStr = fmt.Sprintf("%.2f MB", float64(totalExpectedBytes)/(1024*1024))
	} else if allFinished && totalFiles > 0 {
		totalExpectedStr = totalCurrentStr
	}

	overallSpeedStr := formatSpeed(overallSpeedBps)
	etaStr := "N/A"

	if !allFinished && overallSpeedBps > 0 && totalExpectedBytes > 0 && totalCurrentBytes < totalExpectedBytes {
		remainingBytes := totalExpectedBytes - totalCurrentBytes
		if remainingBytes > 0 {
			etaStr = calculateETA(overallSpeedBps, totalExpectedBytes, totalCurrentBytes)
		}
	} else if allFinished {
		etaStr = "Done"
		overallSpeedStr = "Completed"
	}

	barWidth := progressBarWidth + 10
	filledWidth := 0
	if percentage > 0 {
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

// performActualDraw using full screen clear (assumed to be working)
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
	m.mu.Unlock()

	stdoutMutex.Lock()
	defer stdoutMutex.Unlock()

	fmt.Print("\033[H\033[2J") // Clear screen and move to home

	fmt.Println("Download Progress:") // Optional static header
	fmt.Println(strings.Repeat("-", 80))

	for _, bar := range barsSnapshot {
		fmt.Println(bar.getProgressString())
	}

	if len(barsSnapshot) > 0 {
		fmt.Println(strings.Repeat("-", 80))
		fmt.Println(m.getOverallProgressString(barsSnapshot))
	}

	os.Stdout.Sync()
}

func (m *ProgressManager) Stop() {
	close(m.stopRedraw)
	m.wg.Wait()
}

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
	headReq, _ := http.NewRequest("HEAD", url, nil)
	headClient := http.Client{Timeout: 5 * time.Second}
	headResp, headErr := headClient.Do(headReq)

	if headErr == nil && headResp.StatusCode == http.StatusOK {
		initialTotalSize = headResp.ContentLength
		headResp.Body.Close()
	}

	pw := manager.AddDownload(fileName, initialTotalSize)

	client := http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		pw.MarkFinished(fmt.Sprintf("Req create: %v", shortenError(err, 15)))
		return
	}

	resp, getErr := client.Do(req)
	if getErr != nil {
		pw.MarkFinished(fmt.Sprintf("GET: %v", shortenError(getErr, 15)))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		pw.MarkFinished(fmt.Sprintf("HTTP: %s", resp.Status))
		return
	}

	pw.mu.Lock()
	if pw.Total == -1 || (resp.ContentLength > 0 && pw.Total != resp.ContentLength) {
		pw.Total = resp.ContentLength
	}
	pw.mu.Unlock()
	manager.requestRedraw()

	out, createErr := os.Create(filePath)
	if createErr != nil {
		pw.MarkFinished(fmt.Sprintf("Create file: %v", shortenError(createErr, 15)))
		return
	}
	defer out.Close()

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
	var urlsFilePath string

	flag.IntVar(&concurrency, "c", 3, "Number of concurrent downloads")
	flag.StringVar(&urlsFilePath, "f", "", "Path to the text file containing URLs")
	flag.Parse()

	if urlsFilePath == "" {
		fmt.Println("Error: -f flag (file path) is required.")
		flag.Usage()
		os.Exit(1)
	}

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

	fmt.Printf("Attempting to download %d files from '%s' into '%s' (concurrency: %d)...\n",
		len(urls), urlsFilePath, downloadDir, concurrency)
	time.Sleep(100 * time.Millisecond)

	var downloadWG sync.WaitGroup
	sem := make(chan struct{}, concurrency)

	for _, url := range urls {
		sem <- struct{}{}
		downloadWG.Add(1)
		go func(u string) {
			defer func() { <-sem }()
			downloadFile(u, &downloadWG, downloadDir, manager)
		}(url)
	}

	downloadWG.Wait()
	manager.Stop()

	fmt.Printf("All %d download tasks have been processed.\n", len(urls))
}