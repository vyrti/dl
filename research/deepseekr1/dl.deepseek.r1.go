// deepseek r1 (reasoner) 05.2025
// promtp:
// write app in go lang that will download all file from text file,
// text file must be specified using -f, also add concurrency option -c,
// terminal must show progress bar, speed, and ETA, think twice and recheck the answer,
// give 10 different code output that will perform this task,
// try to use external dependency as less as possible
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
	"sync/atomic"
	"time"
)

type Download struct {
	URL        string
	Filename   string
	Size       int64
	Downloaded int64
	Start      time.Time
	Done       bool
}

type ProgressState struct {
	Downloads []*Download
	Mutex     sync.RWMutex
}

func main() {
	filePtr := flag.String("f", "", "Path to file containing URLs")
	concurrencyPtr := flag.Int("c", 1, "Concurrency level")
	flag.Parse()

	if *filePtr == "" {
		fmt.Println("Error: Input file not specified")
		flag.Usage()
		os.Exit(1)
	}

	urls, err := readURLs(*filePtr)
	if err != nil {
		fmt.Printf("Error reading URLs: %v\n", err)
		os.Exit(1)
	}

	state := &ProgressState{
		Downloads: make([]*Download, len(urls)),
	}
	for i, url := range urls {
		state.Downloads[i] = &Download{URL: url}
	}

	go displayProgress(state, *concurrencyPtr)

	var wg sync.WaitGroup
	sem := make(chan struct{}, *concurrencyPtr)

	for i := range urls {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer func() {
				<-sem
				wg.Done()
			}()
			downloadFile(state, idx)
		}(i)
	}

	wg.Wait()
	fmt.Println("\nAll downloads completed!")
}

func readURLs(filename string) ([]string, error) {
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

func downloadFile(state *ProgressState, idx int) {
	state.Mutex.Lock()
	dl := state.Downloads[idx]
	dl.Start = time.Now()
	filename, err := getFilename(dl.URL)
	if err != nil {
		filename = "download_" + strconv.Itoa(idx)
	}
	dl.Filename = filename
	state.Mutex.Unlock()

	resp, err := http.Get(dl.URL)
	if err != nil {
		markFailed(state, idx)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		markFailed(state, idx)
		return
	}

	file, err := os.Create(dl.Filename)
	if err != nil {
		markFailed(state, idx)
		return
	}
	defer file.Close()

	size := resp.ContentLength
	state.Mutex.Lock()
	dl.Size = size
	state.Mutex.Unlock()

	counter := &WriteCounter{Download: dl}
	_, err = io.Copy(file, io.TeeReader(resp.Body, counter))
	if err != nil {
		markFailed(state, idx)
		return
	}

	state.Mutex.Lock()
	dl.Done = true
	state.Mutex.Unlock()
}

func markFailed(state *ProgressState, idx int) {
	state.Mutex.Lock()
	defer state.Mutex.Unlock()
	state.Downloads[idx].Done = true
	state.Downloads[idx].Filename = "FAILED"
}

func getFilename(url string) (string, error) {
	if !strings.Contains(url, "/") {
		return "index.html", nil
	}

	parts := strings.Split(url, "/")
	filename := parts[len(parts)-1]

	filename = strings.SplitN(filename, "?", 2)[0]
	filename = strings.SplitN(filename, "#", 2)[0]

	if filename == "" {
		return "index.html", nil
	}
	return filepath.Base(filename), nil
}

type WriteCounter struct {
	Download *Download
}

func (wc *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	atomic.AddInt64(&wc.Download.Downloaded, int64(n))
	return n, nil
}

func displayProgress(state *ProgressState, concurrency int) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		fmt.Print("\033[H\033[2J") // Clear screen

		state.Mutex.RLock()
		completed := 0
		for _, dl := range state.Downloads {
			if dl.Done {
				completed++
			}
		}

		total := len(state.Downloads)
		progress := float64(completed) / float64(total)
		bar := renderProgressBar(progress, 40)
		fmt.Printf("Overall: [%s] %.1f%% (%d/%d)\n\n", bar, progress*100, completed, total)

		// Show active downloads
		fmt.Println("Active downloads:")
		activeCount := 0
		for _, dl := range state.Downloads {
			if !dl.Done && !dl.Start.IsZero() && activeCount < concurrency {
				var speed float64
				var eta string

				if dl.Size > 0 {
					elapsed := time.Since(dl.Start).Seconds()
					if elapsed > 0 {
						speed = float64(dl.Downloaded) / elapsed
						remaining := float64(dl.Size-dl.Downloaded) / speed
						eta = formatDuration(time.Duration(remaining) * time.Second)
					}

					percent := float64(dl.Downloaded) / float64(dl.Size)
					fileBar := renderProgressBar(percent, 20)
					fmt.Printf("  %s [%s] %.1f%% (%s/s, ETA: %s)\n",
						filepath.Base(dl.Filename), fileBar, percent*100, formatBytes(speed), eta)
				} else {
					fmt.Printf("  %s [downloading...] %s downloaded\n",
						filepath.Base(dl.Filename), formatBytes(float64(dl.Downloaded)))
				}
				activeCount++
			}

			// Break early if we've shown enough
			if activeCount >= concurrency {
				break
			}
		}
		state.Mutex.RUnlock()
	}
}

func renderProgressBar(progress float64, width int) string {
	filled := int(progress * float64(width))
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("=", filled)
	if filled < width {
		bar += ">" + strings.Repeat(" ", width-filled-1)
	}
	return bar
}

func formatBytes(b float64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%.1fB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", b/float64(div), "KMGTPE"[exp])
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}
