// hf.go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// --- Structs for Hugging Face API ---
type RepoInfo struct {
	Siblings []Sibling `json:"siblings"`
}

type Sibling struct {
	Rfilename string `json:"rfilename"`
}

// --- Hugging Face URL Fetching Logic ---
func fetchHuggingFaceURLs(repoInput string) ([]string, error) {
	appLogger.Printf("[HF] Processing Hugging Face repository input: %s", repoInput)

	var repoID string

	if strings.HasPrefix(repoInput, "http://") || strings.HasPrefix(repoInput, "https://") {
		parsedInputURL, err := url.Parse(repoInput)
		if err != nil {
			return nil, fmt.Errorf("error parsing repository URL '%s': %w", repoInput, err)
		}
		if parsedInputURL.Host != "huggingface.co" {
			return nil, fmt.Errorf("expected a huggingface.co URL, got: %s", parsedInputURL.Host)
		}
		repoPath := strings.TrimPrefix(parsedInputURL.Path, "/")
		pathParts := strings.Split(repoPath, "/")
		if len(pathParts) < 2 {
			return nil, fmt.Errorf("invalid repository path in URL. Expected 'owner/repo_name', got: '%s'", repoPath)
		}
		repoID = fmt.Sprintf("%s/%s", pathParts[0], pathParts[1])
	} else if strings.Count(repoInput, "/") == 1 {
		parts := strings.Split(repoInput, "/")
		if len(parts[0]) > 0 && len(parts[1]) > 0 {
			repoID = repoInput
		} else {
			return nil, fmt.Errorf("invalid repository ID format. Expected 'owner/repo_name', got: '%s'", repoInput)
		}
	} else {
		return nil, fmt.Errorf("invalid -hf input '%s'. Expected 'owner/repo_name' or full https://huggingface.co/owner/repo_name URL", repoInput)
	}

	branch := "main"

	appLogger.Printf("[HF] Determined RepoID: %s, Branch for download URLs: %s", repoID, branch)
	fmt.Fprintf(os.Stderr, "[INFO] Fetching file list for repository: %s (branch: %s)...\n", repoID, branch)

	apiURL := fmt.Sprintf("https://huggingface.co/api/models/%s", repoID)
	appLogger.Printf("[HF] Using API endpoint for repo files: %s", apiURL)

	httpClient := http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Get(apiURL)
	if err != nil {
		return nil, fmt.Errorf("error fetching data from API '%s': %w", apiURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API request failed with status %s for URL %s", resp.Status, apiURL)
	}

	var repoData RepoInfo
	if err := json.NewDecoder(resp.Body).Decode(&repoData); err != nil {
		return nil, fmt.Errorf("error decoding JSON response: %w", err)
	}

	if len(repoData.Siblings) == 0 {
		appLogger.Printf("[HF] No files found in repository %s via API.", repoID)
		fmt.Fprintf(os.Stderr, "[INFO] No files found in repository %s. The API might have changed or the repo is empty/private.\n", repoID)
		return []string{}, nil
	}

	appLogger.Printf("[HF] Found %d file entries in repository %s.", len(repoData.Siblings), repoID)
	fmt.Fprintf(os.Stderr, "[INFO] Found %d file entries. Generating download URLs...\n", len(repoData.Siblings))

	var downloadURLs []string
	for _, sibling := range repoData.Siblings {
		if sibling.Rfilename == "" {
			appLogger.Printf("[HF] Skipping sibling with empty rfilename.")
			continue
		}

		rfilenameParts := strings.Split(sibling.Rfilename, "/")
		escapedRfilenameParts := make([]string, len(rfilenameParts))
		for i, p := range rfilenameParts {
			escapedRfilenameParts[i] = url.PathEscape(p)
		}
		safeRfilenamePath := strings.Join(escapedRfilenameParts, "/")

		dlURL := fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s?download=true", repoID, branch, safeRfilenamePath)
		downloadURLs = append(downloadURLs, dlURL)
		appLogger.Printf("[HF] Generated download URL: %s for rfilename: %s", dlURL, sibling.Rfilename)
	}
	fmt.Fprintf(os.Stderr, "[INFO] Successfully generated %d download URLs from Hugging Face repository.\n", len(downloadURLs))
	return downloadURLs, nil
}
