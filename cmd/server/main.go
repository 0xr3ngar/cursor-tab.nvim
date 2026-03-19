package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"connectrpc.com/connect"
	aiserverv1 "github.com/bengu3/cursor-tab.nvim/cursor-api/gen/aiserver/v1"
	"github.com/bengu3/cursor-tab.nvim/internal/cursor"
	"github.com/bengu3/cursor-tab.nvim/internal/suggestionstore"
	"github.com/google/uuid"
)

var cursorClient *cursor.Client
var store = suggestionstore.NewStore()
var logger *slog.Logger

// Cancel-and-replace pattern: new requests cancel any in-flight stream
var activeStreamMu sync.Mutex
var activeStreamCancel context.CancelFunc

// Per-file diff history: tracks diffs from accepted suggestions (not user edits).
// Format: "5-|old line\n5+|new line\n" — matches Cursor IDE reference implementation.
// Sliding window of max 3 entries per file.
var diffHistoryMu sync.Mutex
var diffHistoryMap = make(map[string][]string) // fileName -> []diffString

type NewSuggestionRequest struct {
	FileContents  string `json:"file_contents"`
	Line          int32  `json:"line"`
	Column        int32  `json:"column"`
	FilePath      string `json:"file_path"`
	LanguageID    string `json:"language_id"`
	WorkspacePath string `json:"workspace_path"`
	Intent        string `json:"intent,omitempty"`
}

// RecordDiffRequest is sent by the Lua plugin after accepting a suggestion.
// It records the diff between old and new text for file_diff_histories.
type RecordDiffRequest struct {
	FilePath  string   `json:"file_path"`
	StartLine int      `json:"start_line"` // 0-indexed
	OldLines  []string `json:"old_lines"`
	NewLines  []string `json:"new_lines"`
}

type SuggestionResponse struct {
	Suggestion             string                     `json:"suggestion"`
	Error                  string                     `json:"error,omitempty"`
	RangeReplace           *suggestionstore.RangeInfo `json:"range_replace,omitempty"`
	NextSuggestionID       string                     `json:"next_suggestion_id,omitempty"`
	BindingID              string                     `json:"binding_id,omitempty"`
	ShouldRemoveLeadingEol bool                       `json:"should_remove_leading_eol,omitempty"`
}

// generateSuggestionID creates a unique suggestion ID using UUID
func generateSuggestionID() string {
	return fmt.Sprintf("sugg_%s", uuid.New().String())
}

func handleNewSuggestion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req NewSuggestionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Error("Error decoding request", "error", err)
		json.NewEncoder(w).Encode(SuggestionResponse{Error: err.Error()})
		return
	}

	logger.Info("New suggestion request",
		"file_path", req.FilePath,
		"line", req.Line,
		"column", req.Column,
		"language_id", req.LanguageID,
		"workspace_path", req.WorkspacePath,
		"content_length", len(req.FileContents),
		"intent", req.Intent,
	)

	if cursorClient == nil {
		json.NewEncoder(w).Encode(SuggestionResponse{Error: "cursor client not initialized"})
		return
	}

	// Determine intent source for CppIntentInfo
	// Match Cursor IDE behavior: "typing" (default), "line_changed", "cursor_prediction"
	intentSource := "typing"
	switch req.Intent {
	case "line_changed":
		intentSource = "line_changed"
	case "cursor_prediction":
		intentSource = "cursor_prediction"
	case "typing", "":
		intentSource = "typing"
	default:
		intentSource = "typing" // Default to typing for unknown intents
	}
	logger.Info("Using intent source", "raw_intent", req.Intent, "mapped_source", intentSource)

	lines := strings.Split(req.FileContents, "\n")
	totalLines := int32(len(lines))

	// Look up diff history for this file
	diffHistoryMu.Lock()
	diffHistory := diffHistoryMap[req.FilePath]
	// Make a copy to avoid holding the lock
	diffHistoryCopy := make([]string, len(diffHistory))
	copy(diffHistoryCopy, diffHistory)
	diffHistoryMu.Unlock()

	logger.Info("Including diff history", "file", req.FilePath, "entries", len(diffHistoryCopy))
	for i, d := range diffHistoryCopy {
		logger.Debug("Diff history entry", "index", i, "diff", d)
	}

	giveDebug := true
	supportsCpt := true
	supportsCrlfCpt := true
	isDebug := false
	workspaceID := req.WorkspacePath
	streamReq := &aiserverv1.StreamCppRequest{
		WorkspaceId: &workspaceID,
		CurrentFile: &aiserverv1.CurrentFileInfo{
			Contents:              req.FileContents,
			RelativeWorkspacePath: req.FilePath,
			LanguageId:            req.LanguageID,
			TotalNumberOfLines:    totalLines,
			WorkspaceRootPath:     req.WorkspacePath,
			CursorPosition: &aiserverv1.CursorPosition{
				Line:   req.Line,
				Column: req.Column,
			},
		},
		CppIntentInfo: &aiserverv1.CppIntentInfo{
			Source: intentSource,
		},
		FileDiffHistories: []*aiserverv1.CppFileDiffHistory{
			{
				FileName:    req.FilePath,
				DiffHistory: diffHistoryCopy,
			},
		},
		IsDebug:         &isDebug,
		SupportsCpt:     &supportsCpt,
		SupportsCrlfCpt: &supportsCrlfCpt,
		GiveDebugOutput: &giveDebug,
	}

	// Cancel any previous in-flight stream immediately (don't block waiting for it)
	activeStreamMu.Lock()
	if activeStreamCancel != nil {
		activeStreamCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	activeStreamCancel = cancel
	activeStreamMu.Unlock()

	stream, err := cursorClient.StreamCpp(ctx, streamReq)
	if err != nil {
		logger.Error("Failed to stream from Cursor API", "error", err)
		json.NewEncoder(w).Encode(SuggestionResponse{Error: err.Error()})
		return
	}

	// Parse first suggestion using new early return pattern
	firstSuggestion, err := parseNextSuggestion(stream)
	if err != nil {
		logger.Error("Failed to parse first suggestion", "error", err)
		json.NewEncoder(w).Encode(SuggestionResponse{Error: err.Error()})
		return
	}

	if firstSuggestion == nil {
		json.NewEncoder(w).Encode(SuggestionResponse{Error: "no suggestion returned"})
		return
	}

	// Peek at next chunk to see if there are more suggestions
	// After DoneEdit, next chunk is either BeginEdit (more suggestions) or DoneStream (done)
	var nextSuggestionID string
	var hasMoreSuggestions bool

	if stream.Receive() {
		resp := stream.Msg()

		if resp.BeginEdit != nil && *resp.BeginEdit {
			// There's another suggestion coming!
			hasMoreSuggestions = true
			nextSuggestionID = generateSuggestionID()

			logger.Debug("More suggestions detected, starting background processing",
				"next_suggestion_id", nextSuggestionID)

			// Start background processing (stream is positioned at BeginEdit)
			go storeRemainingSuggestions(ctx, stream, nextSuggestionID)
		} else if resp.DoneStream != nil && *resp.DoneStream {
			// Stream is done, no more suggestions
			hasMoreSuggestions = false
			logger.Debug("No more suggestions, stream complete")
		}
	}

	// Build response
	response := SuggestionResponse{
		Suggestion:             firstSuggestion.Text,
		RangeReplace:           firstSuggestion.Range,
		BindingID:              firstSuggestion.BindingID,
		ShouldRemoveLeadingEol: firstSuggestion.ShouldRemoveLeadingEol,
	}

	if hasMoreSuggestions {
		response.NextSuggestionID = nextSuggestionID
	}

	// Build log attributes
	logAttrs := []any{
		"suggestion_length", len(firstSuggestion.Text),
		"suggestion_lines", len(strings.Split(firstSuggestion.Text, "\n")),
		"has_more_suggestions", hasMoreSuggestions,
		"suggestion_text", firstSuggestion.Text, // Full text
	}
	if firstSuggestion.Range != nil {
		logAttrs = append(logAttrs, "range_start_line", firstSuggestion.Range.StartLine)
		logAttrs = append(logAttrs, "range_end_line", firstSuggestion.Range.EndLine)
	}
	if response.NextSuggestionID != "" {
		logAttrs = append(logAttrs, "next_suggestion_id", response.NextSuggestionID)
	}
	logger.Info("Returning first suggestion", logAttrs...)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func parseSuggestions(stream *connect.ServerStreamForClient[aiserverv1.StreamCppResponse]) ([]*suggestionstore.Suggestion, error) {
	var suggestions []*suggestionstore.Suggestion
	var currentSuggestion *suggestionstore.Suggestion
	chunkCount := 0

	for stream.Receive() {
		resp := stream.Msg()
		chunkCount++

		// Log entire response object structure
		logger.Debug("Received stream chunk", "chunk_number", chunkCount, "response", fmt.Sprintf("%+v", resp))

		// Log debug information if available
		if resp.DebugModelInput != nil || resp.DebugModelOutput != nil {
			debugAttrs := []any{}
			if resp.DebugModelInput != nil {
				debugAttrs = append(debugAttrs, "model_input", *resp.DebugModelInput)
			}
			if resp.DebugModelOutput != nil {
				debugAttrs = append(debugAttrs, "model_output", *resp.DebugModelOutput)
			}
			logger.Debug("Model debug info", debugAttrs...)
		}

		// Handle different chunk types
		if resp.RangeToReplace != nil {
			if currentSuggestion == nil {
				currentSuggestion = &suggestionstore.Suggestion{}
			}
			currentSuggestion.Range = &suggestionstore.RangeInfo{
				StartLine:   resp.RangeToReplace.StartLineNumber,
				StartColumn: 0,
				EndLine:     resp.RangeToReplace.EndLineNumberInclusive,
				EndColumn:   -1,
			}
			if resp.BindingId != nil {
				currentSuggestion.BindingID = *resp.BindingId
			}
			if resp.ShouldRemoveLeadingEol != nil {
				currentSuggestion.ShouldRemoveLeadingEol = *resp.ShouldRemoveLeadingEol
			}
		}

		if resp.Text != "" {
			if currentSuggestion == nil {
				currentSuggestion = &suggestionstore.Suggestion{}
			}
			currentSuggestion.Text += resp.Text
		}

		// Done with current suggestion
		if resp.DoneEdit != nil && *resp.DoneEdit {
			if currentSuggestion != nil {
				suggestions = append(suggestions, currentSuggestion)
				logger.Info("Completed suggestion",
					"index", len(suggestions),
					"chars", len(currentSuggestion.Text),
					"range", currentSuggestion.Range,
				)
				currentSuggestion = nil
			}
		}

		// Beginning new suggestion
		if resp.BeginEdit != nil && *resp.BeginEdit {
			logger.Debug("Beginning new suggestion")
		}

		// Stream is done
		if resp.DoneStream != nil && *resp.DoneStream {
			logger.Debug("Stream complete")
			break
		}
	}

	if err := stream.Err(); err != nil && err != io.EOF {
		return nil, fmt.Errorf("stream error: %w", err)
	}

	logger.Info("Parsed suggestions from stream", "total_suggestions", len(suggestions))
	return suggestions, nil
}

// parseNextSuggestion reads the stream until the next DoneEdit and returns the complete suggestion.
// Returns nil if stream ends (DoneStream) without another suggestion.
func parseNextSuggestion(stream *connect.ServerStreamForClient[aiserverv1.StreamCppResponse]) (*suggestionstore.Suggestion, error) {
	var currentSuggestion *suggestionstore.Suggestion

	for stream.Receive() {
		resp := stream.Msg()

		// Handle range_to_replace
		if resp.RangeToReplace != nil {
			if currentSuggestion == nil {
				currentSuggestion = &suggestionstore.Suggestion{}
			}
			currentSuggestion.Range = &suggestionstore.RangeInfo{
				StartLine:   resp.RangeToReplace.StartLineNumber,
				StartColumn: 0,
				EndLine:     resp.RangeToReplace.EndLineNumberInclusive,
				EndColumn:   -1,
			}
			if resp.BindingId != nil {
				currentSuggestion.BindingID = *resp.BindingId
			}
			if resp.ShouldRemoveLeadingEol != nil {
				currentSuggestion.ShouldRemoveLeadingEol = *resp.ShouldRemoveLeadingEol
			}
		}

		// Accumulate text
		if resp.Text != "" {
			if currentSuggestion == nil {
				currentSuggestion = &suggestionstore.Suggestion{}
			}
			currentSuggestion.Text += resp.Text
		}

		// Check for completion markers
		if resp.DoneEdit != nil && *resp.DoneEdit {
			// Strip leading newline if requested
			if currentSuggestion != nil && currentSuggestion.ShouldRemoveLeadingEol && len(currentSuggestion.Text) > 0 {
				if currentSuggestion.Text[0] == '\n' {
					currentSuggestion.Text = currentSuggestion.Text[1:]
					logger.Debug("Stripped leading newline from suggestion")
				}
			}

			logger.Debug("Parsed complete suggestion",
				"chars", len(currentSuggestion.Text),
				"range", currentSuggestion.Range,
				"should_remove_leading_eol", currentSuggestion.ShouldRemoveLeadingEol)
			return currentSuggestion, nil // Complete suggestion ready!
		}

		if resp.DoneStream != nil && *resp.DoneStream {
			logger.Debug("Stream ended")
			return nil, nil // Stream ended, no more suggestions
		}
	}

	// Handle stream errors
	if err := stream.Err(); err != nil && err != io.EOF {
		return nil, fmt.Errorf("stream error: %w", err)
	}

	return currentSuggestion, nil
}

// storeRemainingSuggestions processes remaining suggestions in the stream and stores them in the cache.
// This runs in a background goroutine after the first suggestion has been returned to the client.
func storeRemainingSuggestions(ctx context.Context, stream *connect.ServerStreamForClient[aiserverv1.StreamCppResponse], firstNextID string) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Background storage panic", "panic", r)
		}
	}()

	currentID := firstNextID
	count := 0

	for {
		// Check for cancellation
		select {
		case <-ctx.Done():
			logger.Info("Background processing cancelled",
				"reason", ctx.Err(),
				"suggestions_stored", count,
			)
			return
		default:
			// Continue processing
		}

		// Parse next suggestion
		suggestion, err := parseNextSuggestion(stream)
		if err != nil {
			logger.Error("Error parsing background suggestion",
				"error", err,
				"suggestions_stored", count)
			return
		}

		if suggestion == nil {
			// Stream ended
			logger.Info("Background processing complete",
				"suggestions_stored", count)
			return
		}

		// Peek at next chunk to see if there are more suggestions
		var nextSuggestionID string
		if stream.Receive() {
			resp := stream.Msg()

			if resp.BeginEdit != nil && *resp.BeginEdit {
				// There's another suggestion coming
				nextSuggestionID = generateSuggestionID()
			} else if resp.DoneStream != nil && *resp.DoneStream {
				// Stream is done, no more suggestions
				nextSuggestionID = ""
			}
		}

		// Store this suggestion with the next ID (or empty if last)
		suggestion.NextSuggestionID = nextSuggestionID
		store.Store(currentID, suggestion)
		count++

		// Log the addition
		logAttrs := []any{
			"suggestion_id", currentID,
			"next_id", nextSuggestionID,
			"chars", len(suggestion.Text),
			"suggestion_text", suggestion.Text,
		}
		if suggestion.Range != nil {
			logAttrs = append(logAttrs,
				"range_start_line", suggestion.Range.StartLine,
				"range_end_line", suggestion.Range.EndLine)
		}
		logger.Info("Stored background suggestion", logAttrs...)

		// Log ALL suggestions currently in store
		allSuggestions := store.GetAll()
		logger.Debug("All suggestions in store after addition",
			"total_suggestions_in_store", len(allSuggestions))
		for id, s := range allSuggestions {
			storeLogAttrs := []any{
				"id", id,
				"chars", len(s.Text),
				"text", s.Text,
				"next_id", s.NextSuggestionID,
			}
			if s.Range != nil {
				storeLogAttrs = append(storeLogAttrs,
					"range_start_line", s.Range.StartLine,
					"range_end_line", s.Range.EndLine)
			}
			logger.Debug("  -> Suggestion in store", storeLogAttrs...)
		}

		// If there's no next suggestion, we're done
		if nextSuggestionID == "" {
			logger.Info("Background processing complete",
				"suggestions_stored", count)
			return
		}

		// Move to next ID
		currentID = nextSuggestionID
	}
}

func handleGetSuggestion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract ID from path: /suggestion/{id}
	suggestionID := strings.TrimPrefix(r.URL.Path, "/suggestion/")
	if suggestionID == "" || suggestionID == r.URL.Path {
		json.NewEncoder(w).Encode(SuggestionResponse{Error: "suggestion ID required"})
		return
	}

	storeKeysBeforeGet := store.Keys()
	logger.Info("Get suggestion request", "suggestion_id", suggestionID)
	logger.Debug("Store state before get",
		"total_suggestions_in_store", len(storeKeysBeforeGet),
		"store_keys", storeKeysBeforeGet)

	// Get suggestion from store
	suggestion := store.Get(suggestionID)
	if suggestion == nil {
		logger.Warn("Suggestion not found in store", "suggestion_id", suggestionID)
		json.NewEncoder(w).Encode(SuggestionResponse{Error: "suggestion not found"})
		return
	}

	response := SuggestionResponse{
		Suggestion:             suggestion.Text,
		RangeReplace:           suggestion.Range,
		BindingID:              suggestion.BindingID,
		ShouldRemoveLeadingEol: suggestion.ShouldRemoveLeadingEol,
		NextSuggestionID:       suggestion.NextSuggestionID,
	}

	// Delete this suggestion from store (already retrieved)
	store.Delete(suggestionID)

	storeKeysAfterDelete := store.Keys()
	retrievalLogAttrs := []any{
		"suggestion_id", suggestionID,
		"chars", len(suggestion.Text),
		"suggestion_text", suggestion.Text,
		"next_suggestion_id", suggestion.NextSuggestionID,
	}
	if suggestion.Range != nil {
		retrievalLogAttrs = append(retrievalLogAttrs,
			"range_start_line", suggestion.Range.StartLine,
			"range_end_line", suggestion.Range.EndLine)
	}
	logger.Info("Returning stored suggestion", retrievalLogAttrs...)
	logger.Debug("Store state after deletion",
		"total_suggestions_in_store", len(storeKeysAfterDelete),
		"store_keys", storeKeysAfterDelete)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleRecordDiff records the diff from an accepted suggestion into per-file history.
// This is called by the Lua plugin after Tab-accepting a suggestion.
// Format matches Cursor IDE reference: "5-|old line\n5+|new line\n"
func handleRecordDiff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req RecordDiffRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Error("Error decoding record_diff request", "error", err)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Build diff string in the reference format: "lineNum-|old\nlineNum+|new\n"
	var diffStr string
	maxLen := len(req.OldLines)
	if len(req.NewLines) > maxLen {
		maxLen = len(req.NewLines)
	}

	for i := 0; i < maxLen; i++ {
		lineNum := req.StartLine + i + 1 // 1-indexed for the diff format

		var oldLine *string
		var newLine *string
		if i < len(req.OldLines) {
			oldLine = &req.OldLines[i]
		}
		if i < len(req.NewLines) {
			newLine = &req.NewLines[i]
		}

		if oldLine != nil && newLine != nil && *oldLine == *newLine {
			continue // identical lines, skip
		}

		if oldLine != nil && newLine != nil {
			// Modified line
			diffStr += fmt.Sprintf("%d-|%s\n%d+|%s\n", lineNum, *oldLine, lineNum, *newLine)
		} else if newLine != nil {
			// Added line
			diffStr += fmt.Sprintf("%d+|%s\n", lineNum, *newLine)
		} else if oldLine != nil {
			// Removed line
			diffStr += fmt.Sprintf("%d-|%s\n", lineNum, *oldLine)
		}
	}

	if diffStr == "" {
		logger.Debug("record_diff: no actual diff to record", "file", req.FilePath)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "no_diff"})
		return
	}

	// Append to per-file history, sliding window of 3
	diffHistoryMu.Lock()
	history := diffHistoryMap[req.FilePath]
	history = append(history, diffStr)
	if len(history) > 3 {
		history = history[len(history)-3:]
	}
	diffHistoryMap[req.FilePath] = history
	diffHistoryMu.Unlock()

	logger.Info("Recorded diff for file",
		"file", req.FilePath,
		"diff", diffStr,
		"total_entries", len(history),
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func main() {
	// Parse command-line flags
	port := flag.Int("port", 0, "Port to listen on (0 = OS assigns available port)")
	flag.Parse()

	// Set up structured logging
	logFile, err := os.OpenFile("/tmp/cursor-tab.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	// Create JSON handler for structured logging
	logger = slog.New(slog.NewJSONHandler(logFile, &slog.HandlerOptions{
		Level: slog.LevelDebug, // Include debug logs
	}))

	cursorClient, err = cursor.NewClient()
	if err != nil {
		logger.Error("Failed to initialize Cursor client", "error", err)
	}

	// POST /suggestion/new - generate new suggestions from Cursor
	http.HandleFunc("/suggestion/new", handleNewSuggestion)

	// GET /suggestion/{id} - retrieve existing suggestion from store
	http.HandleFunc("/suggestion/", handleGetSuggestion)

	// POST /diff/record - record diff from accepted suggestion
	http.HandleFunc("/diff/record", handleRecordDiff)

	// Create listener to get actual port
	listener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", *port))
	if err != nil {
		logger.Error("Failed to create listener", "error", err)
		os.Exit(1)
	}

	// Get the actual port that was assigned
	serverPort := listener.Addr().(*net.TCPAddr).Port

	// Add port to logger context for all subsequent logs
	logger = logger.With("port", serverPort)

	// Print port to stdout for Lua to parse (before any other output)
	fmt.Printf("SERVER_PORT=%d\n", serverPort)

	logger.Info("Server starting",
		"address", fmt.Sprintf("localhost:%d", serverPort),
		"endpoints", []string{
			"POST /suggestion/new",
			"GET /suggestion/{id}",
		},
	)

	if err := http.Serve(listener, nil); err != nil {
		logger.Error("Server error", "error", err)
		os.Exit(1)
	}
}
