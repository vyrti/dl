package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"
)

var (
	fileList    = flag.String("f", "", "Path to file containing URLs (one per line)")
	concurrency = flag.Int("c", 4, "Number of concurrent downloads")
)

func main() {
	flag.Parse()
	if *fileList == "" {
		fmt.Println("Usage: downloader -f urls.txt [-c concurrency]")
		os.Exit(1)
	}

	// Read URLs
	f, err := os.Open(*fileList)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	var urls []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			urls = append(urls, line)
		}
	}
	if len(urls) == 0 {
		fmt.Println("No URLs found in", *fileList)
		os.Exit(1)
	}

	// Determine total size via HEAD
	var totalSize int64
	for _, u := range urls {
		resp, err := http.Head(u)
		if err != nil {
			fmt.Fprintf(os.Stderr, "HEAD %s: %v (skipping)\n", u, err)
			continue
		}
		resp.Body.Close()
		if resp.ContentLength > 0 {
			totalSize += resp.ContentLength
		}
	}
	if totalSize == 0 {
		fmt.Println("Could not determine total size, exiting.")
		os.Exit(1)
	}

	// Progress tracking
	var downloaded int64
	start := time.Now()

	// Launch progress printer
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				d := atomic.LoadInt64(&downloaded)
				pct := float64(d) / float64(totalSize) * 100
				elapsed := time.Since(start)
				speed := float64(d) / elapsed.Seconds()
				eta := time.Duration(float64(totalSize-d)/speed) * time.Second
				drawProgress(pct, speed, eta)
			case <-done:
				drawProgress(100, 0, 0)
				fmt.Println()
				return
			}
		}
	}()

	// Download with worker pool
	wg := sync.WaitGroup{}
	jobs := make(chan string)
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				err := download(u, &downloaded)
				if err != nil {
					fmt.Fprintf(os.Stderr, "ERROR downloading %s: %v\n", u, err)
				}
			}
		}()
	}

	for _, u := range urls {
		jobs <- u
	}
	close(jobs)
	wg.Wait()
	close(done)
}

// download a single URL, writing to local file and updating counter
func download(url string, downloaded *int64) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	fname := path.Base(resp.Request.URL.Path)
	if fname == "" || fname == "/" {
		fname = "index.html"
	}
	out, err := os.Create(fname)
	if err != nil {
		return err
	}
	defer out.Close()

	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			atomic.AddInt64(downloaded, int64(n))
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	return nil
}

// drawProgress renders a single-line progress bar with speed and ETA
func drawProgress(pct, speed float64, eta time.Duration) {
	width := 40
	filled := int(pct / 100 * float64(width))
	bar := fmt.Sprintf("[%s%s] %6.2f%%",
		string(repeat('=', filled)),
		string(repeat('.', width-filled)),
		pct,
	)
	speedStr := fmt.Sprintf("%6.1f KB/s", speed/1024)
	etaStr := fmt.Sprintf("ETA %s", eta.Truncate(time.Second))
	fmt.Printf("\r%s %s %s", bar, speedStr, etaStr)
}

func repeat(c rune, count int) []rune {
	r := make([]rune, count)
	for i := range r {
		r[i] = c
	}
	return r
}
