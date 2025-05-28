package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Progress tracks the download progress for a single file.
type Progress struct {
	mu         sync.Mutex
	downloaded int64
	total      int64
	startTime  time.Time
	filename   string
	done       bool
}

// formatBytes converts bytes to a human-readable string (e.g., "1.2 MB").
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// download handles downloading a file from a URL and updates progress.
func download(url string, p *Progress) {
	resp, err := http.Get(url)
	if err != nil {
		log.Printf("Error downloading %s: %v", url, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Bad status for %s: %s", url, resp.Status)
		return
	}

	// Get total size from Content-Length header, if available.
	totalStr := resp.Header.Get("Content-Length")
	if totalStr != "" {
		total, err := strconv.ParseInt(totalStr, 10, 64)
		if err == nil {
			p.mu.Lock()
			p.total = total
			p.mu.Unlock()
		}
	}

	// Set start time after successful HTTP request.
	p.mu.Lock()
	p.startTime = time.Now()
	p.mu.Unlock()

	// Create local file to save the download.
	file, err := os.Create(p.filename)
	if err != nil {
		log.Printf("Error creating file %s: %v", p.filename, err)
		return
	}
	defer file.Close()

	// Download in 1MB chunks and update progress.
	buf := make([]byte, 1024*1024) // 1MB buffer
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			file.Write(buf[:n])
			p.mu.Lock()
			p.downloaded += int64(n)
			p.mu.Unlock()
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Error reading from %s: %v", url, err)
			break
		}
	}
	p.mu.Lock()
	p.done = true
	p.mu.Unlock()
}

func main() {
	// Parse command-line flags.
	fileFlag := flag.String("f", "", "text file containing URLs")
	concurrencyFlag := flag.Int("c", 1, "concurrency level")
	flag.Parse()

	if *fileFlag == "" {
		log.Fatal("Please specify a text file with -f")
	}

	// Open the URL list file.
	file, err := os.Open(*fileFlag)
	if err != nil {
		log.Fatalf("Error opening file: %v", err)
	}
	defer file.Close()

	// Read URLs from the file.
	scanner := bufio.NewScanner(file)
	var urls []string
	for scanner.Scan() {
		url := strings.TrimSpace(scanner.Text())
		if url != "" {
			urls = append(urls, url)
		}
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("Error reading file: %v", err)
	}

	// Initialize progress tracking for each URL.
	var progresses []*Progress
	for _, url := range urls {
		p := &Progress{filename: filepath.Base(url)}
		progresses = append(progresses, p)
	}

	// Use WaitGroup to wait for all goroutines to complete.
	var wg sync.WaitGroup
	wg.Add(1) // For progress goroutine

	// Start progress reporting goroutine.
	go func() {
		defer wg.Done()
		for {
			time.Sleep(1 * time.Second)
			var lines []string
			allDone := true
			for _, p := range progresses {
				p.mu.Lock()
				if p.done {
					lines = append(lines, fmt.Sprintf("Download completed: %s (%s)", p.filename, formatBytes(p.downloaded)))
				} else {
					allDone = false
					var line string
					if p.total > 0 {
						// Calculate progress when total size is known.
						percentage := float64(p.downloaded) / float64(p.total) * 100
						barWidth := 50
						filled := int(percentage / 100 * float64(barWidth))
						bar := strings.Repeat("=", filled)
						if filled < barWidth {
							bar += ">"
							bar += strings.Repeat(" ", barWidth-filled-1)
						} else {
							bar += strings.Repeat(" ", barWidth-filled)
						}
						elapsed := time.Since(p.startTime)
						speed := 0.0
						if elapsed > 0 {
							speed = float64(p.downloaded) / elapsed.Seconds()
						}
						eta := "?"
						if speed > 0 {
							remaining := p.total - p.downloaded
							etaSeconds := float64(remaining) / speed
							eta = (time.Duration(etaSeconds) * time.Second).String()
						}
						line = fmt.Sprintf("Downloading %s: [%s] %.2f%% (%s/%s), Speed: %.2f KB/s, ETA: %s",
							p.filename, bar, percentage, formatBytes(p.downloaded), formatBytes(p.total), speed/1024, eta)
					} else {
						// Progress without total size.
						elapsed := time.Since(p.startTime)
						speed := 0.0
						if elapsed > 0 {
							speed = float64(p.downloaded) / elapsed.Seconds()
						}
						line = fmt.Sprintf("Downloading %s: (%s), Speed: %.2f KB/s",
							p.filename, formatBytes(p.downloaded), speed/1024)
					}
					lines = append(lines, line)
				}
				p.mu.Unlock()
			}
			if allDone {
				break
			}
			// Print progress lines and move cursor up to overwrite on next iteration.
			output := strings.Join(lines, "\n") + "\n"
			fmt.Print(output)
			fmt.Printf("\033[%dA", len(lines))
		}
		// Print final completion messages.
		for _, p := range progresses {
			fmt.Printf("Download completed: %s (%s)\n", p.filename, formatBytes(p.downloaded))
		}
	}()

	// Start download goroutines with concurrency control.
	sem := make(chan struct{}, *concurrencyFlag)
	for i, url := range urls {
		p := progresses[i]
		wg.Add(1)
		go func(url string, p *Progress) {
			defer wg.Done()
			sem <- struct{}{}        // Acquire semaphore
			defer func() { <-sem }() // Release semaphore
			download(url, p)
		}(url, p)
	}

	// Wait for all downloads and progress reporting to complete.
	wg.Wait()
}
