// go.beta/functions_search.go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// HFApiModelInfo represents a single model from the Hugging Face API (api/models endpoint).
type HFApiModelInfo struct {
	ModelID      string    `json:"modelId"` // e.g., "gpt2", "facebook/bart-large-cnn"
	Author       string    `json:"author"`  // e.g., "facebook" for "facebook/bart-large-cnn", can be empty
	SHA          string    `json:"sha,omitempty"`
	LastModified time.Time `json:"lastModified"`
	Tags         []string  `json:"tags,omitempty"`
	PipelineTag  string    `json:"pipeline_tag,omitempty"` // Task, e.g., "text-generation"
	Siblings     []struct {
		Rfilename string `json:"rfilename"`
	} `json:"siblings,omitempty"`
	Private     bool        `json:"private,omitempty"`
	Gated       interface{} `json:"gated,omitempty"` // Can be bool or string like "auto", "manual"
	Disabled    bool        `json:"disabled,omitempty"`
	Downloads   int         `json:"downloads"`
	Likes       int         `json:"likes"`
	LibraryName string      `json:"library_name,omitempty"` // e.g., "transformers"
	Spaces      []string    `json:"spaces,omitempty"`
}

// formatNumber formats large integers into a more readable string (e.g., 1.2K, 3.4M).
func formatNumber(n int) string {
	if n < 0 { // Should not happen for downloads/likes, but good to handle
		return fmt.Sprintf("%d", n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000.0)
	}
	if n < 1000000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1000000.0)
	}
	return fmt.Sprintf("%.1fB", float64(n)/1000000000.0)
}

// HandleModelSearch searches for models on Hugging Face and displays popular results.
func HandleModelSearch(query string) {
	appLogger.Printf("[ModelSearch] Initiating search for query: '%s'", query)
	fmt.Fprintf(os.Stderr, "[INFO] Searching for models matching '%s' on Hugging Face...\n", query)

	apiBaseURL := "https://huggingface.co/api/models"
	params := url.Values{}
	params.Add("search", query)
	params.Add("sort", "downloads") // Sort by downloads
	params.Add("direction", "-1")   // Descending order
	params.Add("limit", "20")       // Limit to 20 results
	params.Add("full", "true")      // Fetch full info to get more consistent fields like Author
	// `full=true` is a bit slower but provides more data.
	// `full=false` (or omitting) is faster but might miss some fields.
	// For comprehensive display like Author, Likes, PipelineTag, `full=true` is safer.

	fullURL := apiBaseURL + "?" + params.Encode()
	appLogger.Printf("[ModelSearch] Fetching from URL: %s", fullURL)

	client := http.Client{Timeout: 45 * time.Second} // Increased timeout for potentially larger "full=true" responses
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		appLogger.Printf("[ModelSearch] Error creating request: %v", err)
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to create search request: %v\n", err)
		return
	}
	req.Header.Set("User-Agent", "go-downloader-app/1.0 (model-search)") // Polite to set User-Agent
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		appLogger.Printf("[ModelSearch] Error performing request: %v", err)
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to search Hugging Face: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		appLogger.Printf("[ModelSearch] API request failed with status %s", resp.Status)
		// Try to read body for more error info if possible
		bodyBytes, readErr := io.ReadAll(resp.Body)
		if readErr == nil {
			appLogger.Printf("[ModelSearch] Response body: %s", string(bodyBytes))
		}
		fmt.Fprintf(os.Stderr, "[ERROR] Hugging Face API request failed: %s\n", resp.Status)
		return
	}

	var results []HFApiModelInfo
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		appLogger.Printf("[ModelSearch] Error decoding JSON response: %v", err)
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to parse search results: %v\n", err)
		return
	}

	if len(results) == 0 {
		fmt.Fprintf(os.Stderr, "[INFO] No models found matching your query '%s'.\n", query)
		appLogger.Printf("[ModelSearch] No results for query '%s'", query)
		return
	}

	fmt.Fprintf(os.Stderr, "\nTop %d model results for \"%s\" (sorted by downloads):\n", len(results), query)
	fmt.Println(strings.Repeat("=", 80))

	for i, model := range results {
		// The API already limits to 20, but this is a safeguard.
		if i >= 20 {
			break
		}

		// Determine Author: Use model.Author if present, otherwise derive from modelId
		authorDisplay := model.Author
		if authorDisplay == "" {
			parts := strings.Split(model.ModelID, "/")
			if len(parts) > 1 {
				authorDisplay = parts[0] // First part of "user/model_name"
			} else {
				authorDisplay = "N/A" // For models like "gpt2" that don't have explicit org/user in ID
			}
		}

		taskDisplay := model.PipelineTag
		if taskDisplay == "" {
			taskDisplay = "N/A"
		}

		// Append (Private) or (Gated) status to task display
		statusAddons := []string{}
		if model.Private {
			statusAddons = append(statusAddons, "Private")
		}
		if model.Gated != nil {
			gatedStr := fmt.Sprintf("%v", model.Gated) // Handles bool or string types
			if gatedStr == "true" || strings.ToLower(gatedStr) == "auto" || strings.ToLower(gatedStr) == "manual" {
				statusAddons = append(statusAddons, "Gated")
			}
		}
		if len(statusAddons) > 0 {
			taskDisplay = fmt.Sprintf("%s (%s)", taskDisplay, strings.Join(statusAddons, ", "))
		}

		fmt.Printf("%2d. Model ID: %s\n", i+1, model.ModelID)
		fmt.Printf("    Author: %s\n", authorDisplay)
		fmt.Printf("    Stats: Downloads: %s | Likes: %s | Updated: %s\n",
			formatNumber(model.Downloads),
			formatNumber(model.Likes),
			model.LastModified.Format("2006-01-02"))
		fmt.Printf("    Task: %s\n", taskDisplay)

		if len(model.Tags) > 0 {
			displayTags := []string{}
			currentLen := 0
			maxTagLen := 70 // Max char length for the tags line
			// Prioritize non-generic tags if possible, or just take first few
			for _, t := range model.Tags {
				// Skip very generic tags if the list becomes too long
				if (t == "transformers" || t == "pytorch" || t == "safetensors" || t == model.LibraryName || t == model.PipelineTag) && len(model.Tags) > 5 && currentLen > maxTagLen/2 {
					continue
				}
				if currentLen+len(t) > maxTagLen && len(displayTags) > 0 {
					displayTags = append(displayTags, "...")
					break
				}
				if len(displayTags) > 0 { // Add comma for subsequent tags
					currentLen += 2 // for ", "
				}
				displayTags = append(displayTags, t)
				currentLen += len(t)

				if len(displayTags) >= 10 && currentLen > maxTagLen/2 { // Limit number of tags shown too
					if len(model.Tags) > len(displayTags) {
						displayTags = append(displayTags, "...")
					}
					break
				}
			}
			if len(displayTags) > 0 {
				fmt.Printf("    Tags: %s\n", strings.Join(displayTags, ", "))
			}
		}
		fmt.Println(strings.Repeat("-", 40)) // Separator for each model entry
	}

	if len(results) < 20 && len(results) > 0 {
		fmt.Fprintf(os.Stderr, "\nFound %d model(s).\n", len(results))
	} else if len(results) >= 20 {
		// The API was asked for 20, so if we get 20, it implies it might be the limit.
		fmt.Fprintf(os.Stderr, "\nShowing the top %d models. More results might be available on Hugging Face.\n", len(results))
	}
	appLogger.Printf("[ModelSearch] Successfully displayed %d results for query '%s'", len(results), query)
}
