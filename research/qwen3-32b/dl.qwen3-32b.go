package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func extractFilename(url string) string {
	// Remove query parameters and hash
	url = strings.Split(url, "?")[0]
	url = strings.Split(url, "#")[0]
	filename := filepath.Base(url)
	if filename == "." || filename == "/" || filename == "" {
		filename = fmt.Sprintf("download-%d.gguf", time.Now().UnixNano())
	}
	return filename
}

func main() {
	// Parse flags
	var filePath string
	var concurrency int
	flag.StringVar(&filePath, "f", "", "Path to the text file with URLs")
	flag.IntVar(&concurrency, "c", 5, "Number of concurrent downloads")
	flag.Parse()

	// Validate inputs
	if filePath == "" {
		log.Fatalf("❌ Please specify a file with -f")
	}

	// Read URLs from file
	log.Printf("📂 Reading URLs from file: %s\n", filePath)
	data, err := os.ReadFile(filePath)
	if err != nil {
		log.Fatalf("❌ Error reading file: %v", err)
	}
	urls := string(data)
	urlList := strings.Split(urls, "\n")

	// Filter valid URLs
	var validURLs []string
	for _, url := range urlList {
		url = strings.TrimSpace(url)
		if url != "" {
			validURLs = append(validURLs, url)
		}
	}

	if len(validURLs) == 0 {
		log.Println("❌ No valid URLs to download.")
		return
	}

	// Channel to distribute URLs
	urlChan := make(chan string, concurrency)
	var wg sync.WaitGroup

	// Track progress
	total := len(validURLs)
	var completed int64
	startTime := time.Now()

	// Progress goroutine
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				current := atomic.LoadInt64(&completed)
				if current >= int64(total) {
					log.Println("✅ All downloads completed!")
					return
				}
				percent := float64(current) / float64(total) * 100
				speed := float64(current) / time.Since(startTime).Seconds()
				eta := time.Since(startTime).Seconds() * (float64(total) - float64(current)) / float64(current)
				log.Printf("🔄 Progress: %.2f%% (%d/%d) | Speed: %.2f files/sec | ETA: %.2fs", percent, current, total, speed, eta)
			}
		}
	}()

	// Start workers
	for i := 0; i < concurrency; i++ {
		go func() {
			log.Printf("✅ Worker started")
			for url := range urlChan {
				wg.Done()
				log.Printf("📥 Processing URL: %s", url)

				// Create HTTP client with timeout
				client := &http.Client{
					Timeout: 10 * time.Minute, // Hugging Face may take time
				}

				// Set headers to avoid 403
				req, err := http.NewRequest("GET", url, nil)
				if err != nil {
					log.Printf("❌ Error creating request for %s: %v", url, err)
					continue
				}
				req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GoDownloader/1.0)")
				req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
				req.Header.Set("Accept-Language", "en-US,en;q=0.5")

				resp, err := client.Do(req)
				if err != nil {
					log.Printf("❌ Error downloading %s: %v", url, err)
					continue
				}
				defer resp.Body.Close()

				// Log HTTP status
				log.Printf("🌐 HTTP Status: %s (%d)", resp.Status, resp.StatusCode)
				if resp.StatusCode != 200 {
					log.Printf("❌ Invalid HTTP status for %s: %d", url, resp.StatusCode)
					continue
				}

				// Generate and save file
				filename := extractFilename(url)
				file, err := os.Create(filename)
				if err != nil {
					log.Printf("❌ Error creating file %s: %v", filename, err)
					continue
				}
				defer file.Close()

				// Copy response to file
				_, err = io.Copy(file, resp.Body)
				if err != nil {
					log.Printf("❌ Error writing file %s: %v", filename, err)
				} else {
					log.Printf("✅ Downloaded file: %s", filename)
				}

				atomic.AddInt64(&completed, 1)
			}
		}()
	}

	// Feed URLs to workers
	log.Printf("📤 Sending %d URLs to workers", len(validURLs))
	for _, url := range validURLs {
		wg.Add(1)
		urlChan <- url
	}
	close(urlChan)
	wg.Wait()
}
