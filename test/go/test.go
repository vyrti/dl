package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

// Structs to parse the Hugging Face API response
type RepoInfo struct {
	Siblings []Sibling `json:"siblings"`
	// Add other fields if needed, like ModelID, LastModified, etc.
}

type Sibling struct {
	Rfilename string `json:"rfilename"` // Relative filename
	// Add other fields if needed, like FileSize, Lfs, etc.
}

const outputFileName = "download_links.txt"

func main() {
	repoURLStr := flag.String("repoUrl", "", "Hugging Face repository URL (e.g., https://huggingface.co/deepseek-ai/DeepSeek-R1-0528)")
	flag.Parse()

	if *repoURLStr == "" {
		log.Fatalf("Error: -repoUrl flag is required. Example: -repoUrl https://huggingface.co/deepseek-ai/DeepSeek-R1-0528")
	}

	// 1. Parse the input URL and extract owner/repo_name
	parsedURL, err := url.Parse(*repoURLStr)
	if err != nil {
		log.Fatalf("Error parsing repository URL '%s': %v", *repoURLStr, err)
	}

	if parsedURL.Host != "huggingface.co" {
		log.Fatalf("Error: Expected a huggingface.co URL, got: %s", parsedURL.Host)
	}

	// Path will be like "/owner/repo_name"
	repoPath := strings.TrimPrefix(parsedURL.Path, "/")
	if strings.Count(repoPath, "/") != 1 {
		log.Fatalf("Error: Invalid repository path format. Expected 'owner/repo_name', got: %s", repoPath)
	}
	parts := strings.Split(repoPath, "/")
	repoOwner := parts[0]
	repoName := parts[1]
	repoID := fmt.Sprintf("%s/%s", repoOwner, repoName)

	fmt.Printf("Fetching file list for repository: %s\n", repoID)

	// 2. Construct the API URL
	apiURL := fmt.Sprintf("https://huggingface.co/api/models/%s", repoID)

	// 3. Fetch file list from the API
	resp, err := http.Get(apiURL)
	if err != nil {
		log.Fatalf("Error fetching data from API '%s': %v", apiURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Fatalf("Error: API request failed with status %s for URL %s", resp.Status, apiURL)
	}

	var repoData RepoInfo
	if err := json.NewDecoder(resp.Body).Decode(&repoData); err != nil {
		log.Fatalf("Error decoding JSON response: %v", err)
	}

	if len(repoData.Siblings) == 0 {
		log.Printf("No files found in repository %s. The API might have changed or the repo is empty/private.", repoID)
		return
	}

	fmt.Printf("Found %d files. Generating download URLs...\n", len(repoData.Siblings))

	// 4. Prepare to write to output file
	file, err := os.Create(outputFileName)
	if err != nil {
		log.Fatalf("Error creating output file '%s': %v", outputFileName, err)
	}
	defer file.Close()

	writer := bufio.NewWriter(file)

	// 5. Transform filenames and write to file
	var downloadURLs []string
	for _, sibling := range repoData.Siblings {
		if sibling.Rfilename == "" {
			continue // Skip if filename is empty
		}
		// Ensure filename is URL-safe (though rfilename usually is)
		safeFilename := url.PathEscape(sibling.Rfilename)

		// Construct the download URL
		// Base: https://huggingface.co/
		// RepoID: owner/repo_name
		// Path segment: /resolve/main/
		// Filename: FILENAME
		// Query: ?download=true
		downloadURL := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s?download=true", repoID, safeFilename)
		downloadURLs = append(downloadURLs, downloadURL)

		_, err := writer.WriteString(downloadURL + "\n")
		if err != nil {
			log.Printf("Warning: Error writing URL '%s' to file: %v", downloadURL, err)
		}
	}

	if err := writer.Flush(); err != nil {
		log.Fatalf("Error flushing writer to file '%s': %v", outputFileName, err)
	}

	fmt.Printf("Successfully generated %d download URLs and saved them to %s\n", len(downloadURLs), outputFileName)

	// Optional: Print a few generated URLs for quick verification
	if len(downloadURLs) > 0 {
		fmt.Println("\nFirst few generated URLs:")
		limit := 5
		if len(downloadURLs) < limit {
			limit = len(downloadURLs)
		}
		for i := 0; i < limit; i++ {
			fmt.Println(downloadURLs[i])
		}
	}
}

// Helper function to demonstrate path joining (not strictly needed here as we use Sprintf, but good for illustration)
func constructDownloadURL(baseURL, repoID, branch, filename string) string {
	u, _ := url.Parse(baseURL)
	u.Path = path.Join(u.Path, repoID, "resolve", branch, filename)
	q := u.Query()
	q.Set("download", "true")
	u.RawQuery = q.Encode()
	return u.String()
}
