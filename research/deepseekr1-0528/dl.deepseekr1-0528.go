package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type workerStatus struct {
	current  *int64 // Bytes downloaded for current file (atomic)
	total    int64  // Total file size (-1 if unknown)
	filename string // Current filename
	active   bool   // Whether worker is actively downloading
}

func main() {
	fileFlag := flag.String("f", "", "Text file containing URLs")
	concurrencyFlag := flag.Int("c", 4, "Concurrency level")
	flag.Parse()

	if *fileFlag == "" {
		fmt.Println("Error: Missing required -f flag")
		flag.Usage()
		os.Exit(1)
	}

	urls, err := readURLsFromFile(*fileFlag)
	if err != nil {
		fmt.Printf("Error reading URLs: %v\n", err)
		os.Exit(1)
	}

	totalFiles := len(urls)
	var (
		completedFiles atomic.Uint32
		downloadedSize atomic.Uint64
		errors         []string
		errorLock      sync.Mutex
	)

	startTime := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg := new(sync.WaitGroup)
	urlChan := make(chan string)

	// Worker status tracking
	workerStatuses := make([]*workerStatus, *concurrencyFlag)
	for i := range workerStatuses {
		workerStatuses[i] = &workerStatus{
			current: new(int64),
			active:  false,
		}
	}

	// Progress display
	progressTicker := time.NewTicker(100 * time.Millisecond)
	defer progressTicker.Stop()

	go func() {
		prevLineCount := 0
		for range progressTicker.C {
			lineCount := printProgress(
				workerStatuses,
				&completedFiles,
				&downloadedSize,
				totalFiles,
				startTime,
			)

			if lineCount > 0 {
				// Move cursor up to redraw previous lines
				fmt.Printf("\033[%dA", prevLineCount)
			}
			prevLineCount = lineCount
		}
	}()

	// Start workers
	for i := 0; i < *concurrencyFlag; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			ws := workerStatuses[workerID]

			for url := range urlChan {
				// Reset worker status
				atomic.StoreInt64(ws.current, 0)
				ws.total = -1
				ws.filename = filepath.Base(url)
				if idx := strings.Index(ws.filename, "?"); idx != -1 {
					ws.filename = ws.filename[:idx]
				}
				if ws.filename == "" || ws.filename == "." || ws.filename == "/" {
					ws.filename = "download"
				}
				ws.active = true

				// Download file
				size, err := downloadFileWithProgress(url, ws)
				completedFiles.Add(1)
				if err != nil {
					errorLock.Lock()
					errors = append(errors, fmt.Sprintf("%s: %v", url, err))
					errorLock.Unlock()
				} else {
					downloadedSize.Add(uint64(size))
				}

				// Mark worker as inactive
				ws.active = false
			}
		}(i)
	}

	// Feed URLs to workers
	for _, url := range urls {
		urlChan <- url
	}
	close(urlChan)
	wg.Wait()
	progressTicker.Stop()

	// Print final status with completed lines
	fmt.Println() // Extra newline to move past progress display
	printProgress(workerStatuses, &completedFiles, &downloadedSize, totalFiles, startTime)
	fmt.Println() // Space after progress

	// Print any errors
	if len(errors) > 0 {
		fmt.Println("Errors encountered:")
		for _, e := range errors {
			fmt.Println("  ", e)
		}
	}
}

func readURLsFromFile(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
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
	return urls, scanner.Err()
}

func downloadFileWithProgress(url string, ws *workerStatus) (int64, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	ws.total = resp.ContentLength
	file, err := os.Create(ws.filename)
	if err != nil {
		return 0, fmt.Errorf("create file: %w", err)
	}
	defer file.Close()

	// Create progress reader
	progressReader := &progressReader{
		reader:  resp.Body,
		counter: ws.current,
	}

	size, err := io.Copy(file, progressReader)
	if err != nil {
		os.Remove(ws.filename)
		return 0, fmt.Errorf("copy content: %w", err)
	}
	return size, nil
}

type progressReader struct {
	reader  io.Reader
	counter *int64 // Pointer to atomic counter
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if n > 0 {
		atomic.AddInt64(pr.counter, int64(n))
	}
	return n, err
}

func printProgress(
	statuses []*workerStatus,
	completedFiles *atomic.Uint32,
	downloadedSize *atomic.Uint64,
	total int,
	start time.Time,
) int {
	completed := completedFiles.Load()
	totalBytes := downloadedSize.Load()
	elapsed := time.Since(start).Seconds()

	// Calculate global progress
	percent := float64(completed) / float64(total)
	barWidth := 40
	completeWidth := int(percent * float64(barWidth))
	if completeWidth > barWidth {
		completeWidth = barWidth
	}
	bar := "[" + strings.Repeat("=", completeWidth) + strings.Repeat(" ", barWidth-completeWidth) + "]"

	// Calculate ETA
	remaining := total - int(completed)
	eta := "ETA: --:--:--"
	if completed > 0 && remaining > 0 && elapsed > 0 {
		avgTimePerFile := elapsed / float64(completed)
		etaSeconds := avgTimePerFile * float64(remaining)
		d := time.Duration(etaSeconds) * time.Second
		eta = fmt.Sprintf("ETA: %02d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
	}

	// Calculate speed
	speedMBps := 0.0
	if elapsed > 0 {
		speedMBps = float64(totalBytes) / (elapsed * 1024 * 1024)
	}

	// Print global progress
	fmt.Printf("Global: %s %d/%d (%.1f%%) | Speed: %.1f MB/s | %s\n",
		bar, completed, total, percent*100, speedMBps, eta)

	linesPrinted := 1

	// Print only active workers
	for i, ws := range statuses {
		if !ws.active {
			continue
		}

		current := atomic.LoadInt64(ws.current)
		progress := ""
		if ws.total > 0 {
			// Known size
			pct := float64(current) / float64(ws.total)
			progress = fmt.Sprintf("[%s%s] %.1f%% (%s/%s)",
				strings.Repeat("=", int(pct*20)), // 20 character progress bar
				strings.Repeat(" ", 20-int(pct*20)),
				pct*100,
				formatBytes(current),
				formatBytes(ws.total),
			)
		} else {
			// Unknown size
			progress = fmt.Sprintf("%s downloaded                    ", formatBytes(current))
		}

		fmt.Printf("Worker %d: %-40s %s\n", i+1, progress, ws.filename)
		linesPrinted++
	}

	return linesPrinted
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}
