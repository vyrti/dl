package main

import (
	"bufio"
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
	current  *int64 // Bytes downloaded for current file
	total    int64  // Total file size (-1 if unknown)
	filename string // Current filename
	active   bool   // Whether worker is active
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

	var (
		completedFiles atomic.Uint32
		downloadedSize atomic.Uint64
		errors         []string
		errorLock      sync.Mutex
	)

	startTime := time.Now()

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
				if ws.filename == "" {
					ws.filename = "download"
				}
				ws.active = true

				// Print starting message
				fmt.Printf("Worker %d: Starting download: %s\n", workerID+1, ws.filename)

				// Download file
				size, err := downloadFileWithProgress(url, ws)
				completedFiles.Add(1)
				if err != nil {
					errorLock.Lock()
					errors = append(errors, fmt.Sprintf("%s: %v", url, err))
					errorLock.Unlock()
					fmt.Printf("Worker %d: Error: %v\n", workerID+1, err)
				} else {
					downloadedSize.Add(uint64(size))
				}

				// Mark inactive
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

	// Print summary
	totalMB := float64(downloadedSize.Load()) / (1024 * 1024)
	elapsed := time.Since(startTime)

	var speed float64
	if elapsed.Seconds() > 0 {
		speed = totalMB / elapsed.Seconds()
	}

	fmt.Printf("\nSummary: Downloaded %d files in %s (%.2f MB/s)\n",
		completedFiles.Load(), elapsed.Round(time.Second), speed)

	// Print any errors
	if len(errors) > 0 {
		fmt.Println("\nErrors encountered:")
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
		ws:      ws,
		worker:  ws,
	}

	size, err := io.Copy(file, progressReader)
	if err != nil {
		os.Remove(ws.filename)
		return 0, fmt.Errorf("copy content: %w", err)
	}

	// Clear progress line after download completes
	fmt.Printf("\r\033[K")
	return size, nil
}

type progressReader struct {
	reader  io.Reader
	counter *int64
	ws      *workerStatus
	worker  *workerStatus
	last    time.Time
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if n > 0 {
		atomic.AddInt64(pr.counter, int64(n))

		// Throttle progress updates to 10 times per second
		if time.Since(pr.last) > 100*time.Millisecond {
			pr.last = time.Now()
			current := atomic.LoadInt64(pr.worker.current)
			total := pr.worker.total

			if total > 0 {
				pct := float64(current) / float64(total) * 100
				fmt.Printf("\rDownloading %s: %.1f%% (%s/%s)      ",
					pr.worker.filename,
					pct,
					formatBytes(current),
					formatBytes(total))
			} else {
				fmt.Printf("\rDownloading %s: %s       ",
					pr.worker.filename,
					formatBytes(current))
			}
		}
	}
	return n, err
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
