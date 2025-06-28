package main

import (
	"encoding/json"
	"fmt"
	"io" // Added import for io.ReadAll
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// HFFile holds information about a file from Hugging Face.
type HFFile struct {
	URL      string
	Filename string // Original filename from the repository (Sibling.Rfilename)
}

// --- Structs for Hugging Face API ---
type RepoInfo struct {
	Siblings []Sibling `json:"siblings"`
}

type Sibling struct {
	Rfilename string `json:"rfilename"`
}

// --- Hugging Face URL Fetching Logic ---
func fetchHuggingFaceURLs(repoInput string, hfToken string) ([]HFFile, error) {
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

	branch := "main" // Assuming "main" branch, could be parameterized if needed

	appLogger.Printf("[HF] Determined RepoID: %s, Branch for download URLs: %s", repoID, branch)
	fmt.Fprintf(os.Stderr, "[INFO] Fetching file list for repository: %s (branch: %s)...\n", repoID, branch)

	apiURL := fmt.Sprintf("https://huggingface.co/api/models/%s", repoID)
	appLogger.Printf("[HF] Using API endpoint for repo files: %s", apiURL)

	httpClient := http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request for API '%s': %w", apiURL, err)
	}

	if hfToken != "" {
		req.Header.Set("Authorization", "Bearer "+hfToken)
		appLogger.Printf("[HF] Using Hugging Face token for API request to %s", apiURL)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error fetching data from API '%s': %w", apiURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Try to read body for more error info from HF API
		bodyBytes, readErr := io.ReadAll(resp.Body)
		var errorDetail string
		if readErr == nil && len(bodyBytes) > 0 {
			errorDetail = string(bodyBytes)
			appLogger.Printf("[HF] API error response body: %s", errorDetail)
		}
		return nil, fmt.Errorf("API request to %s failed with status %s. Detail: %s", apiURL, resp.Status, errorDetail)
	}

	var repoData RepoInfo
	if err := json.NewDecoder(resp.Body).Decode(&repoData); err != nil {
		return nil, fmt.Errorf("error decoding JSON response: %w", err)
	}

	if len(repoData.Siblings) == 0 {
		appLogger.Printf("[HF] No files found in repository %s via API.", repoID)
		fmt.Fprintf(os.Stderr, "[INFO] No files found in repository %s. The API might have changed, the repo is empty, or access is restricted (check --token and HF_TOKEN for private/gated repos).\n", repoID)
		return []HFFile{}, nil
	}

	appLogger.Printf("[HF] Found %d file entries in repository %s.", len(repoData.Siblings), repoID)
	fmt.Fprintf(os.Stderr, "[INFO] Found %d file entries. Generating download info...\n", len(repoData.Siblings))

	var hfFiles []HFFile
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
		hfFiles = append(hfFiles, HFFile{URL: dlURL, Filename: sibling.Rfilename})
		appLogger.Printf("[HF] Generated download info: URL: %s for rfilename: %s", dlURL, sibling.Rfilename)
	}
	fmt.Fprintf(os.Stderr, "[INFO] Successfully generated info for %d files from Hugging Face repository.\n", len(hfFiles))
	return hfFiles, nil
}
