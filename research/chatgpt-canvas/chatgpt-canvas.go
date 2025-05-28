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
	"time"
)

type DownloadTask struct {
	URL      string
	FileName string
}

type Progress struct {
	Total     int64
	Completed int64
	Start     time.Time
	Mutex     sync.Mutex
}

func main() {
	fileFlag := flag.String("f", "urls.txt", "Path to the file containing URLs")
	concurrency := flag.Int("c", 4, "Number of concurrent downloads")
	flag.Parse()

	urls, err := readLines(*fileFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading file: %v\n", err)
		os.Exit(1)
	}

	tasks := make([]DownloadTask, 0, len(urls))
	for _, url := range urls {
		fileName := filepath.Base(url)
		tasks = append(tasks, DownloadTask{URL: url, FileName: fileName})
	}

	progress := &Progress{Total: int64(len(tasks)), Start: time.Now()}
	wg := sync.WaitGroup{}
	taskChan := make(chan DownloadTask)

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskChan {
				downloadFile(task)
				progress.Update()
			}
		}()
	}

	go func() {
		for _, task := range tasks {
			taskChan <- task
		}
		close(taskChan)
	}()

	go func() {
		for {
			time.Sleep(1 * time.Second)
			progress.Display()
			if progress.Completed == progress.Total {
				break
			}
		}
	}()

	wg.Wait()
	progress.Display()
	fmt.Println("\nAll downloads completed.")
}

func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

func downloadFile(task DownloadTask) {
	resp, err := http.Get(task.URL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error downloading %s: %v\n", task.URL, err)
		return
	}
	defer resp.Body.Close()

	out, err := os.Create(task.FileName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating file %s: %v\n", task.FileName, err)
		return
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error saving file %s: %v\n", task.FileName, err)
	}
}

func (p *Progress) Update() {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()
	p.Completed++
}

func (p *Progress) Display() {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()
	elapsed := time.Since(p.Start).Seconds()
	speed := float64(p.Completed) / elapsed
	eta := float64(p.Total-p.Completed) / speed
	fmt.Printf("\rProgress: %d/%d | Speed: %.2f files/s | ETA: %.0fs",
		p.Completed, p.Total, speed, eta)
}
