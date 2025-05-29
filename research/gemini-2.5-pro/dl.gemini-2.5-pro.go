package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"mime" // Moved here
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Progress represents the state of a single download
type Progress struct {
	url        string
	filePath   string
	totalBytes int64
	readBytes  int64
	startTime  time.Time
	speed      float64 // bytes per second
	eta        time.Duration
	err        error
	completed  bool
	inProgress bool
}

// WriteCounter is an io.Writer that tracks the number of bytes written
// and updates progress.
type WriteCounter struct {
	progress *Progress
	mu       sync.Mutex // To protect progress updates
	// lastTick time.Time // Not strictly needed for this implementation of speed/ETA
}

func (wc *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	wc.mu.Lock()
	defer wc.mu.Unlock()

	wc.progress.readBytes += int64(n)

	// Calculate speed and ETA
	now := time.Now()
	if wc.progress.totalBytes > 0 { // Only calculate if totalBytes is known
		elapsed := now.Sub(wc.progress.startTime).Seconds()
		if elapsed > 0.1 { // Avoid division by zero or tiny elapsed times
			wc.progress.speed = float64(wc.progress.readBytes) / elapsed
			if wc.progress.speed > 0 {
				remainingBytes := wc.progress.totalBytes - wc.progress.readBytes
				wc.progress.eta = time.Duration(float64(remainingBytes)/wc.progress.speed) * time.Second
			} else {
				wc.progress.eta = 0 // Infinite or unknown
			}
		}
	} else if wc.progress.totalBytes == 0 && wc.progress.readBytes > 0 { // Handle 0-byte files correctly
		// If totalBytes is 0 but we've read some, it means server didn't send Content-Length
		// or it's a 0-byte file. Speed can still be calculated based on time.
		elapsed := now.Sub(wc.progress.startTime).Seconds()
		if elapsed > 0.1 {
			wc.progress.speed = float64(wc.progress.readBytes) / elapsed
		}
		wc.progress.eta = 0 // ETA is unknown or 0 for 0-byte files
	} else {
		// If totalBytes is -1 (unknown), ETA cannot be calculated accurately.
		// Speed can still be calculated.
		elapsed := now.Sub(wc.progress.startTime).Seconds()
		if elapsed > 0.1 {
			wc.progress.speed = float64(wc.progress.readBytes) / elapsed
		}
		wc.progress.eta = 0
	}
	return n, nil
}

func main() {
	filePath := flag.String("f", "", "Path to the text file containing URLs")
	concurrency := flag.Int("c", 4, "Number of concurrent downloads")
	flag.Parse()

	if *filePath == "" {
		fmt.Println("Error: -f flag (file path) is required.")
		flag.Usage()
		os.Exit(1)
	}
	if *concurrency <= 0 {
		fmt.Println("Error: -c flag (concurrency) must be a positive integer.")
		flag.Usage()
		os.Exit(1)
	}

	file, err := os.Open(*filePath)
	if err != nil {
		fmt.Printf("Error opening file %s: %v\n", *filePath, err)
		os.Exit(1)
	}
	defer file.Close()

	var urls []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			// Basic URL validation
			_, err := url.ParseRequestURI(line)
			if err == nil {
				urls = append(urls, line)
			} else {
				fmt.Printf("Skipping invalid URL: %s (%v)\n", line, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Printf("Error reading file %s: %v\n", *filePath, err)
		os.Exit(1)
	}

	if len(urls) == 0 {
		fmt.Println("No valid URLs found in the file.")
		return
	}

	var wg sync.WaitGroup
	urlChan := make(chan string, len(urls)) // Buffered channel for URLs
	progressMap := make(map[string]*Progress)
	var mapMutex sync.Mutex // To protect progressMap

	// Initialize progress for all URLs
	for _, u := range urls {
		progressMap[u] = &Progress{url: u}
	}

	// Start worker goroutines
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for u := range urlChan {
				mapMutex.Lock()
				prog := progressMap[u]
				prog.inProgress = true
				prog.startTime = time.Now() // Set start time when download actually begins
				mapMutex.Unlock()

				err := downloadFile(prog) // downloadFile now takes *Progress

				mapMutex.Lock()
				prog.err = err
				prog.completed = true
				prog.inProgress = false
				if err == nil && prog.totalBytes >= 0 && prog.readBytes < prog.totalBytes {
					// If no error but readBytes didn't reach totalBytes (e.g. connection drop after headers)
					// ensure it's marked as fully read if totalBytes is known and positive.
					// If totalBytes was -1, readBytes is the best we have.
					// If totalBytes is 0, readBytes should also be 0.
					if prog.totalBytes > 0 {
						prog.readBytes = prog.totalBytes
					}
				} else if err == nil && prog.totalBytes == 0 {
					prog.readBytes = 0 // For 0-byte files
				}
				mapMutex.Unlock()
			}
		}(i)
	}

	// Feed URLs to workers
	for _, u := range urls {
		urlChan <- u
	}
	close(urlChan) // Close channel to signal workers no more URLs

	// Goroutine to display progress
	var displayWg sync.WaitGroup
	displayWg.Add(1)
	go func() {
		defer displayWg.Done()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		// Initial clear
		fmt.Print("\033[H\033[2J")

		for {
			mapMutex.Lock()
			allDone := true
			var totalReadBytes, grandTotalBytes int64
			var currentOverallSpeed float64
			activeDownloads := 0
			knownSizeDownloads := 0 // Count downloads with known total size for overall ETA

			// Move cursor to top-left and clear screen (ANSI escape codes)
			// This might flicker or not work perfectly on all terminals.
			// For a smoother experience, a library like termui or tview would be better,
			// but we are avoiding external dependencies.
			fmt.Print("\033[H\033[2J")

			fmt.Println("Download Progress:")
			fmt.Println(strings.Repeat("-", 100)) // Wider separator

			sortedURLs := make([]string, 0, len(urls))
			for u := range progressMap {
				sortedURLs = append(sortedURLs, u)
			}
			// Sort alphabetically for consistent display order (optional)
			// sort.Strings(sortedURLs)

			for _, u := range sortedURLs { // Iterate in a consistent order
				prog := progressMap[u]
				if !prog.completed {
					allDone = false
				}
				if prog.inProgress {
					activeDownloads++
				}

				if prog.totalBytes > 0 { // Only add to grandTotal if known
					grandTotalBytes += prog.totalBytes
					knownSizeDownloads++
				} else if prog.totalBytes == 0 && prog.completed && prog.err == nil {
					// Count 0-byte files as "known size" for overall percentage
					knownSizeDownloads++
				}
				totalReadBytes += prog.readBytes
				if prog.inProgress { // Sum speed only for active downloads
					currentOverallSpeed += prog.speed
				}

				fileName := prog.filePath
				if fileName == "" { // Fallback if filePath not set yet
					parsedURL, err := url.Parse(prog.url)
					if err == nil {
						fileName = filepath.Base(parsedURL.Path)
						if fileName == "." || fileName == "" {
							fileName = "download"
						}
					} else {
						fileName = "unknown_file"
					}
				}

				percentage := 0.0
				if prog.totalBytes > 0 {
					percentage = (float64(prog.readBytes) / float64(prog.totalBytes)) * 100
				} else if prog.totalBytes == 0 && prog.completed && prog.err == nil {
					percentage = 100.0 // 0-byte file, completed
				} else if prog.totalBytes == -1 && prog.completed && prog.err == nil {
					percentage = 100.0 // Unknown size, completed successfully
				} else if prog.totalBytes == -1 && prog.inProgress {
					// No percentage for unknown size in progress, or show based on arbitrary assumption
				}

				barWidth := 30
				completedWidth := 0
				if percentage > 0 {
					completedWidth = int(percentage / 100 * float64(barWidth))
				}
				progressBar := "[" + strings.Repeat("=", completedWidth) + strings.Repeat(" ", barWidth-completedWidth) + "]"

				status := "Pending"
				if prog.inProgress {
					status = "Downloading"
				} else if prog.completed {
					if prog.err != nil {
						status = fmt.Sprintf("Error: %v", prog.err)
						status = status[:min(len(status), 25)] // Truncate error
					} else {
						status = "Completed"
					}
				}

				etaStr := "- -"
				if prog.inProgress && prog.eta > 0 && prog.totalBytes > 0 && prog.readBytes < prog.totalBytes {
					etaStr = prog.eta.Round(time.Second).String()
				} else if prog.inProgress && prog.totalBytes == -1 {
					etaStr = "Unknown"
				}

				speedStr := "0 B/s"
				if prog.speed > 0 {
					speedStr = formatBytes(prog.speed) + "/s"
				}
				if !prog.inProgress && !prog.completed {
					speedStr = "- - - -"
				} else if prog.completed && prog.err == nil {
					speedStr = "Done   " // Padding for alignment
				}

				fmt.Printf("%-35s %s %6.2f%% (%10s / %10s) %12s ETA: %8s Status: %s\n",
					truncateString(filepath.Base(fileName), 35), // Use filepath.Base for safety
					progressBar,
					percentage,
					formatBytes(float64(prog.readBytes)),
					formatBytes(float64(prog.totalBytes)), // Will show -1 B for unknown
					speedStr,
					etaStr,
					status,
				)
			}
			fmt.Println(strings.Repeat("-", 100))

			overallPercentage := 0.0
			// Calculate overall percentage based on downloads with known sizes
			// or if all are done.
			if grandTotalBytes > 0 {
				overallPercentage = (float64(totalReadBytes) / float64(grandTotalBytes)) * 100
			} else if allDone && len(urls) > 0 { // If all tasks are done
				// If grandTotalBytes is 0, it means all files were 0-byte or size unknown.
				// If all completed without error, then 100%.
				noErrors := true
				for _, u := range urls {
					if progressMap[u].err != nil {
						noErrors = false
						break
					}
				}
				if noErrors {
					overallPercentage = 100.0
				}
			}

			overallETAStr := "- -"
			if currentOverallSpeed > 0 && grandTotalBytes > 0 && totalReadBytes < grandTotalBytes {
				remainingTotalBytes := grandTotalBytes - totalReadBytes
				overallETA := time.Duration(float64(remainingTotalBytes)/currentOverallSpeed) * time.Second
				overallETAStr = overallETA.Round(time.Second).String()
			} else if allDone {
				overallETAStr = "Done"
			} else if knownSizeDownloads == 0 && activeDownloads > 0 {
				overallETAStr = "Unknown" // If no downloads have known sizes
			}

			fmt.Printf("Overall: [%6.2f%%] Total: %s / %s | Speed: %s/s | ETA: %s | Active: %d/%d | Files: %d/%d\n",
				overallPercentage,
				formatBytes(float64(totalReadBytes)),
				formatBytes(float64(grandTotalBytes)), // Will show 0 B if all unknown
				formatBytes(currentOverallSpeed),
				overallETAStr,
				activeDownloads,
				*concurrency,
				countCompleted(progressMap),
				len(urls),
			)
			mapMutex.Unlock()

			if allDone {
				break
			}
			<-ticker.C
		}
	}()

	wg.Wait()                          // Wait for all download goroutines to finish
	time.Sleep(500 * time.Millisecond) // Give the display goroutine a moment to print the final state
	displayWg.Wait()                   // Wait for the display goroutine to finish its last print

	fmt.Println(strings.Repeat("=", 100))
	fmt.Println("All downloads processed.")
	hadErrors := false
	for _, u := range urls {
		prog := progressMap[u] // Already locked or not needed post-completion
		if prog.err != nil {
			fmt.Printf("Failed to download %s (%s): %v\n", u, prog.filePath, prog.err)
			hadErrors = true
		} else {
			fmt.Printf("Successfully downloaded %s to %s\n", u, prog.filePath)
		}
	}
	if hadErrors {
		os.Exit(1)
	}
}

func countCompleted(progressMap map[string]*Progress) int {
	// No lock needed if called from display goroutine which already holds the lock,
	// or if called after all downloads are done.
	// For safety, if this were more general, it would need a lock.
	// In this specific usage, it's called when the mapMutex is held by the display goroutine
	// or after all goroutines are done.
	completedCount := 0
	for _, p := range progressMap {
		if p.completed {
			completedCount++
		}
	}
	return completedCount
}

func downloadFile(prog *Progress) error {
	// prog.startTime is already set by the main goroutine when picking up the task

	client := http.Client{
		Timeout: 30 * time.Minute, // Add a timeout for the entire download
	}
	req, err := http.NewRequest("GET", prog.url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	// Set a User-Agent, some servers require it
	req.Header.Set("User-Agent", "GoFileDownloader/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to start download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s (URL: %s)", resp.Status, prog.url)
	}

	prog.mu.Lock()                       // Lock progress while updating totalBytes and filePath
	prog.totalBytes = resp.ContentLength // Can be -1 if server doesn't send it

	// Derive filename
	fileName := ""
	contentDisposition := resp.Header.Get("Content-Disposition")
	if contentDisposition != "" {
		_, params, err := mime.ParseMediaType(contentDisposition)
		if err == nil {
			if f, ok := params["filename"]; ok {
				fileName = f
			}
		}
	}

	if fileName == "" {
		parsedURL, err := url.Parse(prog.url)
		if err != nil {
			prog.mu.Unlock()
			return fmt.Errorf("could not parse URL for filename: %w", err)
		}
		fileName = filepath.Base(parsedURL.Path)
		if fileName == "" || fileName == "." || fileName == "/" {
			// Generate a unique name if path base is not useful
			fileName = "download_" + strings.ReplaceAll(time.Now().Format(time.RFC3339Nano), ":", "-")
		}
	}
	// Sanitize filename (basic)
	fileName = strings.ReplaceAll(fileName, "/", "_")
	fileName = strings.ReplaceAll(fileName, "\\", "_")
	// Potentially more sanitization needed for robust cross-platform filenames

	prog.filePath = SanitizeFilename(fileName) // Store the determined filename
	prog.mu.Unlock()

	// Create directory if it doesn't exist (e.g. if filename implies a path, though sanitization above might remove slashes)
	// For simplicity, this example saves to the current directory.
	// If prog.filePath could contain directories, os.MkdirAll(filepath.Dir(prog.filePath), os.ModePerm) would be needed.

	tempFilePath := prog.filePath + ".tmp"
	out, err := os.Create(tempFilePath)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", tempFilePath, err)
	}
	// defer out.Close() // Close is handled explicitly before rename

	counter := &WriteCounter{
		progress: prog,
		// lastTick: time.Now(), // Not used in this WriteCounter version
	}

	// io.Copy will write to file and update progress via WriteCounter
	_, err = io.Copy(out, io.TeeReader(resp.Body, counter))
	// Explicitly close the output file *before* checking io.Copy error or renaming
	closeErr := out.Close()

	if err != nil { // Error during copy
		os.Remove(tempFilePath) // Clean up temp file on error
		return fmt.Errorf("failed to write to file: %w", err)
	}
	if closeErr != nil { // Error during close
		os.Remove(tempFilePath)
		return fmt.Errorf("failed to close temp file: %w", closeErr)
	}

	// Rename temp file to actual filename upon successful download
	err = os.Rename(tempFilePath, prog.filePath)
	if err != nil {
		os.Remove(tempFilePath) // Attempt to clean up if rename fails
		return fmt.Errorf("failed to rename temp file from %s to %s: %w", tempFilePath, prog.filePath, err)
	}

	// Ensure progress reflects completion if ContentLength was -1 or 0
	prog.mu.Lock()
	if prog.totalBytes == -1 || prog.totalBytes == 0 { // If size was unknown or 0
		info, statErr := os.Stat(prog.filePath)
		if statErr == nil {
			prog.totalBytes = info.Size() // Update totalBytes to actual downloaded size
			prog.readBytes = info.Size()  // And readBytes
		}
	} else {
		prog.readBytes = prog.totalBytes // Ensure it's marked as fully read
	}
	// prog.completed is set by the worker goroutine after this function returns
	prog.mu.Unlock()
	return nil
}

// SanitizeFilename removes or replaces characters that are problematic in filenames.
// This is a basic sanitizer. A more robust one would be needed for all edge cases.
func SanitizeFilename(name string) string {
	// Replace common problematic characters
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	name = strings.ReplaceAll(name, ":", "-")
	name = strings.ReplaceAll(name, "*", "-")
	name = strings.ReplaceAll(name, "?", "-")
	name = strings.ReplaceAll(name, "\"", "'")
	name = strings.ReplaceAll(name, "<", "-")
	name = strings.ReplaceAll(name, ">", "-")
	name = strings.ReplaceAll(name, "|", "-")

	// Trim leading/trailing spaces and dots
	name = strings.TrimSpace(name)
	name = strings.Trim(name, ".")

	if name == "" {
		return "unnamed_file_" + time.Now().Format("20060102150405")
	}
	// Limit length (optional)
	// maxLen := 200
	// if len(name) > maxLen {
	// 	name = name[:maxLen]
	// }
	return name
}

// formatBytes converts bytes to a human-readable string (KB, MB, GB)
func formatBytes(b float64) string {
	if b < 0 { // Handle ContentLength = -1 (unknown size)
		return "Unknown"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%.0f B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 5; n /= unit { // exp < 5 for up to PB
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", b/float64(div), "KMGTPE"[exp])
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen < 3 { // Not enough space for "..."
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
