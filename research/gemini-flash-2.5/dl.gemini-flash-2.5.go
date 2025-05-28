package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	filePath    string
	concurrency int
	outputDir   string
)

func init() {
	flag.StringVar(&filePath, "f", "", "Path to the text file containing URLs")
	flag.IntVar(&concurrency, "c", 5, "Number of concurrent downloads")
	flag.StringVar(&outputDir, "o", ".", "Output directory for downloaded files")
	flag.Parse()
}

// --- Common Helper Functions and Structs ---

// formatBytes formats bytes into human-readable strings (e.g., 1.2 MB).
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

// formatSpeed formats bytes per second into human-readable speed (e.g., 5.0 MB/s).
func formatSpeed(bps float64) string {
	return fmt.Sprintf("%s/s", formatBytes(int64(bps)))
}

// getFilenameFromURL extracts a filename from a URL.
// It tries to get the last segment, removes query parameters,
// and provides a fallback if no clear filename is present.
func getFilenameFromURL(url string) string {
	parsedURL := strings.Split(url, "/")
	filename := parsedURL[len(parsedURL)-1]
	if filename == "" || !strings.Contains(filename, ".") {
		// Fallback for URLs without clear filenames (e.g., "http://example.com/data")
		// Use a hash or timestamp to ensure uniqueness and add a common extension
		return "downloaded_file_" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".bin"
	}
	// Basic sanitation: remove query params
	if idx := strings.Index(filename, "?"); idx != -1 {
		filename = filename[:idx]
	}
	return filename
}

// ProgressUpdate contains data for a single download's progress.
type ProgressUpdate struct {
	ID         int
	Filename   string
	Downloaded int64
	Total      int64
	Speed      float64
	ETA        time.Duration
	Done       bool  // Indicates if download is complete
	Error      error // To report download errors for this specific file
}

// ProgressReader wraps an io.Reader to provide progress updates.
// It sends updates to a channel for centralized reporting.
type ProgressReader struct {
	io.Reader
	Total      int64 // Total size of the file
	Downloaded int64 // Bytes downloaded so far
	Start      time.Time
	LastUpdate time.Time
	LastBytes  int64
	Speed      float64 // Bytes per second
	Filename   string
	ID         int                   // Unique ID for this download
	UpdateChan chan<- ProgressUpdate // Channel to send updates to a central reporter
	Done       bool                  // Indicates if download is complete
}

// NewProgressReader creates a new ProgressReader.
func NewProgressReader(r io.Reader, total int64, filename string, id int, updateChan chan<- ProgressUpdate) *ProgressReader {
	return &ProgressReader{
		Reader:     r,
		Total:      total,
		Start:      time.Now(),
		LastUpdate: time.Now(),
		Filename:   filename,
		ID:         id,
		UpdateChan: updateChan,
	}
}

// Read implements the io.Reader interface, tracking progress.
func (pr *ProgressReader) Read(p []byte) (n int, err error) {
	n, err = pr.Reader.Read(p)
	pr.Downloaded += int64(n)

	now := time.Now()
	elapsed := now.Sub(pr.LastUpdate).Seconds()
	// Update every 100ms or on completion/error/first read
	if elapsed >= 0.1 || pr.Downloaded == pr.Total || err != nil || pr.LastBytes == 0 {
		bytesSinceLastUpdate := pr.Downloaded - pr.LastBytes
		if elapsed > 0 {
			pr.Speed = float64(bytesSinceLastUpdate) / elapsed
		} else {
			pr.Speed = 0 // Avoid division by zero
		}

		var eta time.Duration
		if pr.Speed > 0 && pr.Total > 0 {
			remainingBytes := pr.Total - pr.Downloaded
			eta = time.Duration(float64(remainingBytes)/pr.Speed) * time.Second
		}

		pr.UpdateChan <- ProgressUpdate{
			ID:         pr.ID,
			Filename:   pr.Filename,
			Downloaded: pr.Downloaded,
			Total:      pr.Total,
			Speed:      pr.Speed,
			ETA:        eta,
			Done:       (err == io.EOF || pr.Downloaded == pr.Total),
			Error:      err,
		}
		pr.LastUpdate = now
		pr.LastBytes = pr.Downloaded
	}
	return
}

// readURLsFromFile reads URLs from the specified file.
func readURLsFromFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
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
		return nil, fmt.Errorf("error reading file: %w", err)
	}
	return urls, nil
}

// --- Main Application Logic ---

func main() {
	if filePath == "" {
		fmt.Println("Error: Text file path must be specified using -f")
		flag.Usage()
		os.Exit(1)
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Printf("Error creating output directory %s: %v\n", outputDir, err)
		os.Exit(1)
	}

	urls, err := readURLsFromFile(filePath)
	if err != nil {
		fmt.Printf("Error reading URLs from file: %v\n", err)
		os.Exit(1)
	}

	if len(urls) == 0 {
		fmt.Println("No URLs found in the file.")
		return
	}

	fmt.Printf("Starting download of %d files with %d concurrent connections...\n", len(urls), concurrency)

	progressChan := make(chan ProgressUpdate)
	doneReporting := make(chan struct{})
	tasks := make(chan struct {
		ID  int
		URL string
	})
	var wg sync.WaitGroup

	// Start the progress reporter goroutine
	// It will listen for updates and manage the terminal display
	go reportProgress(progressChan, doneReporting, len(urls), concurrency)

	// Start worker goroutines
	// These workers will pick up tasks from the 'tasks' channel
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go worker(tasks, progressChan, outputDir, &wg)
	}

	// Distribute tasks to the workers
	for i, url := range urls {
		tasks <- struct {
			ID  int
			URL string
		}{ID: i + 1, URL: url}
	}
	close(tasks) // No more tasks to send, signal workers to finish after current tasks

	wg.Wait()           // Wait for all workers to finish their tasks
	close(progressChan) // Signal the reporter that no more updates will come
	<-doneReporting     // Wait for the reporter to finish its final display
	fmt.Println("All downloads completed.")
}

// worker goroutine to process download tasks.
func worker(tasks <-chan struct {
	ID  int
	URL string
}, progressChan chan<- ProgressUpdate, outputDir string, wg *sync.WaitGroup) {
	defer wg.Done()
	for task := range tasks {
		filename := getFilenameFromURL(task.URL)
		outputPath := filepath.Join(outputDir, filename)
		err := downloadFileWithProgress(task.URL, outputPath, task.ID, progressChan)
		if err != nil {
			// Send a final update with error if download failed
			progressChan <- ProgressUpdate{ID: task.ID, Filename: filename, Error: err, Done: true}
		}
	}
}

// downloadFileWithProgress downloads a file using ProgressReader to report progress.
func downloadFileWithProgress(url, filepath string, id int, progressChan chan<- ProgressUpdate) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	out, err := os.Create(filepath)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", filepath, err)
	}
	defer out.Close()

	// Wrap the response body with our ProgressReader to get updates
	reader := NewProgressReader(resp.Body, resp.ContentLength, getFilenameFromURL(url), id, progressChan)
	_, err = io.Copy(out, reader) // io.Copy will call Read on our ProgressReader
	if err != nil {
		return fmt.Errorf("failed to copy data: %w", err)
	}
	return nil
}

// reportProgress listens for updates from all downloads and prints them to the console.
// It uses ANSI escape codes to clear and redraw multiple lines, providing a dynamic display.
func reportProgress(progressChan <-chan ProgressUpdate, done chan<- struct{}, totalFiles, maxConcurrent int) {
	// Map to store current progress for each active download ID
	activeDownloads := make(map[int]ProgressUpdate)
	completedFiles := 0
	totalFilesToDownload := totalFiles // Total number of files to download

	// Print initial empty lines for progress bars and overall status
	// This reserves space on the terminal and moves the cursor down.
	for i := 0; i < maxConcurrent; i++ {
		fmt.Println("") // Placeholder for each concurrent download
	}
	fmt.Println("") // Placeholder for overall status

	// Use a ticker to control the update frequency to the terminal
	ticker := time.NewTicker(100 * time.Millisecond) // Update every 100ms
	defer ticker.Stop()

	for {
		select {
		case update, ok := <-progressChan:
			if !ok { // Channel closed, no more updates are expected
				goto endReporting
			}

			// Store or update the progress for this download
			activeDownloads[update.ID] = update
			if update.Done {
				// If a download is done, remove it from active list and increment completed count
				delete(activeDownloads, update.ID)
				if update.Error == nil {
					completedFiles++
				}
			}

		case <-ticker.C:
			// On each tick, clear the previous display and redraw
			// Move cursor up by (maxConcurrent + 1 for overall status) lines
			fmt.Printf("\033[%dA", maxConcurrent+1) // Move cursor up

			// Redraw active downloads
			i := 0
			for _, p := range activeDownloads {
				progress := 0.0
				if p.Total > 0 {
					progress = float64(p.Downloaded) / float64(p.Total) * 100
				}
				// Create a simple text-based progress bar
				barLength := 50
				filled := int(progress / 100 * float64(barLength))
				bar := strings.Repeat("=", filled) + strings.Repeat("-", barLength-filled)

				// Print line content, then clear rest of line, then newline
				fmt.Printf("%-20.20s [%s] %s/%s (%.2f%%) @ %s ETA: %v\033[K\n", // \033[K clears current line
					p.Filename, bar,
					formatBytes(p.Downloaded), formatBytes(p.Total),
					progress, formatSpeed(p.Speed), p.ETA.Round(time.Second),
				)
				i++
			}
			// Fill remaining lines if less than maxConcurrent are currently active
			for ; i < maxConcurrent; i++ {
				fmt.Print("\033[K\n") // Clear empty line and move to next line
			}
			// Print overall status
			fmt.Printf("Overall: %d/%d files completed.\033[K\n", completedFiles, totalFilesToDownload) // \033[K clears current line

			// Crucially, flush stdout to ensure changes are rendered immediately
			os.Stdout.Sync()
		}
	}

endReporting:
	// Final clear and print after all downloads are done and channel is closed
	fmt.Printf("\033[%dA", maxConcurrent+1) // Move cursor up again
	// Clear all lines in the display block
	for i := 0; i < maxConcurrent; i++ {
		fmt.Print("\033[K\n") // Clear line and move to next
	}
	fmt.Print("\033[K\n") // Clear overall status line

	// Print final summary or any errors
	if len(activeDownloads) > 0 { // Should ideally be empty if all are 'Done'
		fmt.Println("Some downloads might have ended with errors:")
		for _, p := range activeDownloads {
			if p.Error != nil {
				fmt.Printf("  - Error downloading %s: %v\n", p.Filename, p.Error)
			}
		}
	}
	fmt.Printf("Overall: %d/%d files completed.\n", completedFiles, totalFilesToDownload)
	os.Stdout.Sync() // Final flush
	close(done)      // Signal that reporting is complete
}
