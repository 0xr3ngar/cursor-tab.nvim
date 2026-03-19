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
	"time"

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

// DiagnosticInfo represents a single LSP diagnostic from Neovim.
// Sent by the Lua plugin from vim.diagnostic.get() results.
type DiagnosticInfo struct {
	Message   string `json:"message"`
	Severity  int32  `json:"severity"`   // 1=Error, 2=Warning, 3=Info, 4=Hint
	StartLine int32  `json:"start_line"` // 0-indexed
	StartCol  int32  `json:"start_col"`  // 0-indexed
	EndLine   int32  `json:"end_line"`   // 0-indexed
	EndCol    int32  `json:"end_col"`    // 0-indexed
	Source    string `json:"source"`     // e.g. "pyright", "eslint", "lua_ls"
}

// AdditionalFileInfo represents another open buffer sent from Neovim.
type AdditionalFileInfo struct {
	RelativeWorkspacePath     string   `json:"relative_workspace_path"`
	IsOpen                    bool     `json:"is_open"`
	VisibleRangeContent       []string `json:"visible_range_content,omitempty"`
	LastViewedAt              float64  `json:"last_viewed_at,omitempty"`
	StartLineNumberOneIndexed int32    `json:"start_line_number_one_indexed,omitempty"`
}

// ParameterHintInfo represents an LSP signature help entry from Neovim.
// Sent when the cursor is inside a function call for better argument suggestions.
type ParameterHintInfo struct {
	Label         string  `json:"label"`
	Documentation *string `json:"documentation,omitempty"`
}

type NewSuggestionRequest struct {
	FileContents    string               `json:"file_contents"`
	Line            int32                `json:"line"`
	Column          int32                `json:"column"`
	FilePath        string               `json:"file_path"`
	LanguageID      string               `json:"language_id"`
	WorkspacePath   string               `json:"workspace_path"`
	Intent          string               `json:"intent,omitempty"`
	Diagnostics     []DiagnosticInfo     `json:"diagnostics,omitempty"`
	AdditionalFiles []AdditionalFileInfo `json:"additional_files,omitempty"`
	LineEnding      string               `json:"line_ending,omitempty"`
	FileVersion     *int32               `json:"file_version,omitempty"`
	ClientTime      *float64             `json:"client_time,omitempty"`
	ParameterHints  []ParameterHintInfo  `json:"parameter_hints,omitempty"`
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
	SuggestionConfidence   *int32                     `json:"suggestion_confidence,omitempty"`
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
	enableMoreContext := true
	workspaceID := req.WorkspacePath
	clientTime := float64(time.Now().UnixMilli()) / 1000.0 // epoch seconds as float64
	if req.ClientTime != nil {
		clientTime = *req.ClientTime
	}
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
		IsDebug:           &isDebug,
		SupportsCpt:       &supportsCpt,
		SupportsCrlfCpt:   &supportsCrlfCpt,
		GiveDebugOutput:   &giveDebug,
		EnableMoreContext: &enableMoreContext,
		ClientTime:        &clientTime,
	}

	// Populate file version if provided (monotonic counter per buffer)
	if req.FileVersion != nil {
		streamReq.CurrentFile.FileVersion = req.FileVersion
	}

	// Populate line ending if provided
	if req.LineEnding != "" {
		streamReq.CurrentFile.LineEnding = &req.LineEnding
	}

	// Populate additional files (other open buffers)
	if len(req.AdditionalFiles) > 0 {
		logger.Info("Including additional files", "count", len(req.AdditionalFiles))
		for _, af := range req.AdditionalFiles {
			protoFile := &aiserverv1.AdditionalFile{
				RelativeWorkspacePath: af.RelativeWorkspacePath,
				IsOpen:                af.IsOpen,
				VisibleRangeContent:   af.VisibleRangeContent,
			}
			if af.LastViewedAt > 0 {
				protoFile.LastViewedAt = &af.LastViewedAt
			}
			if af.StartLineNumberOneIndexed > 0 {
				protoFile.StartLineNumberOneIndexed = []int32{af.StartLineNumberOneIndexed}
			}
			streamReq.AdditionalFiles = append(streamReq.AdditionalFiles, protoFile)
		}
	}

	// Populate diagnostics from Neovim LSP into both proto fields:
	// 1. CurrentFileInfo.Diagnostics (inline per-file diagnostics)
	// 2. StreamCppRequest.LinterErrors (top-level with file contents and source)
	if len(req.Diagnostics) > 0 {
		logger.Info("Including LSP diagnostics", "count", len(req.Diagnostics))

		var protoDiagnostics []*aiserverv1.Diagnostic
		var linterErrors []*aiserverv1.LinterError

		for _, d := range req.Diagnostics {
			diagRange := &aiserverv1.CursorRange{
				StartPosition: &aiserverv1.CursorPosition{
					Line:   d.StartLine,
					Column: d.StartCol,
				},
				EndPosition: &aiserverv1.CursorPosition{
					Line:   d.EndLine,
					Column: d.EndCol,
				},
			}

			// Map severity: vim.diagnostic and proto use the same values (1-4)
			severity := aiserverv1.DiagnosticSeverity(d.Severity)

			// CurrentFileInfo.Diagnostics entry
			protoDiagnostics = append(protoDiagnostics, &aiserverv1.Diagnostic{
				Message:  d.Message,
				Range:    diagRange,
				Severity: severity,
			})

			// LinterErrors entry (includes source and is_stale)
			linterErrors = append(linterErrors, &aiserverv1.LinterError{
				Message:  d.Message,
				Range:    diagRange,
				Source:   &d.Source,
				Severity: &severity,
			})

			logger.Debug("Diagnostic",
				"message", d.Message,
				"severity", d.Severity,
				"source", d.Source,
				"range", fmt.Sprintf("%d:%d-%d:%d", d.StartLine, d.StartCol, d.EndLine, d.EndCol),
			)
		}

		streamReq.CurrentFile.Diagnostics = protoDiagnostics
		streamReq.LinterErrors = &aiserverv1.LinterErrors{
			RelativeWorkspacePath: req.FilePath,
			Errors:                linterErrors,
			FileContents:          req.FileContents,
		}
	}

	// Populate parameter hints from LSP signature help
	if len(req.ParameterHints) > 0 {
		logger.Info("Including parameter hints", "count", len(req.ParameterHints))
		for _, ph := range req.ParameterHints {
			hint := &aiserverv1.CppParameterHint{
				Label: ph.Label,
			}
			if ph.Documentation != nil {
				hint.Documentation = ph.Documentation
			}
			streamReq.ParameterHints = append(streamReq.ParameterHints, hint)
			logger.Debug("Parameter hint", "label", ph.Label)
		}
	}

	// Cancel any previous in-flight stream immediately (don't block waiting for it)
	activeStreamMu.Lock()
	if activeStreamCancel != nil {
		activeStreamCancel()
		// Clear orphaned suggestions from the canceled stream
		store.ClearAll()
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

	// Parse FIRST suggestion synchronously - this is the one we return immediately.
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

	// Peek ahead: is there another suggestion starting?
	// We need to check for BeginEdit to know if there's a chain.
	var firstNextID string
	hasMore := false
	// Try to read the next chunk to see if there's a chained suggestion
	if stream.Receive() {
		resp := stream.Msg()
		if resp.DoneStream != nil && *resp.DoneStream {
			// Stream is done, no more suggestions
		} else if resp.BeginEdit != nil && *resp.BeginEdit {
			// There IS another suggestion - generate an ID for it
			firstNextID = generateSuggestionID()
			hasMore = true
		} else {
			// Could be a cursor_prediction_target or other chunk - check if more comes
			// Keep reading until we find BeginEdit or DoneStream
			for stream.Receive() {
				resp = stream.Msg()
				if resp.DoneStream != nil && *resp.DoneStream {
					break
				}
				if resp.BeginEdit != nil && *resp.BeginEdit {
					firstNextID = generateSuggestionID()
					hasMore = true
					break
				}
			}
		}
	}

	// Return first suggestion immediately
	response := SuggestionResponse{
		Suggestion:             firstSuggestion.Text,
		RangeReplace:           firstSuggestion.Range,
		BindingID:              firstSuggestion.BindingID,
		ShouldRemoveLeadingEol: firstSuggestion.ShouldRemoveLeadingEol,
		NextSuggestionID:       firstNextID,
		SuggestionConfidence:   firstSuggestion.SuggestionConfidence,
	}

	logAttrs := []any{
		"suggestion_length", len(firstSuggestion.Text),
		"suggestion_lines", len(strings.Split(firstSuggestion.Text, "\n")),
		"has_more_suggestions", hasMore,
		"suggestion_text", firstSuggestion.Text,
	}
	if firstSuggestion.Range != nil {
		logAttrs = append(logAttrs, "range_start_line", firstSuggestion.Range.StartLine)
		logAttrs = append(logAttrs, "range_end_line", firstSuggestion.Range.EndLine)
	}
	if firstNextID != "" {
		logAttrs = append(logAttrs, "next_suggestion_id", firstNextID)
	}
	if firstSuggestion.SuggestionConfidence != nil {
		logAttrs = append(logAttrs, "confidence", *firstSuggestion.SuggestionConfidence)
	}
	logger.Info("Returning first suggestion", logAttrs...)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)

	// Parse remaining suggestions in a goroutine with the cancellable context.
	// If a new request comes in, it cancels this context and the goroutine exits.
	if hasMore {
		go func() {
			defer cancel()
			storeRemainingChainedSuggestions(stream, firstNextID, ctx)
		}()
	} else {
		cancel() // No more work, release context
	}
}

// parseSuggestions is kept for potential batch use but not currently called.
// All suggestion parsing now happens synchronously via parseNextSuggestion in handleNewSuggestion.

// storeRemainingChainedSuggestions parses remaining suggestions from the stream
// and stores them in the suggestion store, chained via NextSuggestionID.
// firstID is the ID already generated for the 2nd suggestion (which we haven't parsed yet).
func storeRemainingChainedSuggestions(stream *connect.ServerStreamForClient[aiserverv1.StreamCppResponse], firstID string, ctx context.Context) {
	prevID := firstID
	storedCount := 0

	// Parse the suggestion whose BeginEdit we already consumed
	nextSuggestion, err := parseNextSuggestion(stream)
	if err != nil {
		logger.Error("Failed to parse chained suggestion in goroutine", "error", err)
		return
	}
	if nextSuggestion == nil {
		logger.Debug("Chained suggestion was empty in goroutine")
		return
	}

	// Store the first chained suggestion under the pre-generated ID
	nextSuggestion.NextSuggestionID = ""
	store.Store(firstID, nextSuggestion)
	storedCount++

	logStoredSuggestion(firstID, storedCount+1, nextSuggestion)

	// Continue parsing any further chained suggestions
	for {
		// Check if context was canceled (new request came in)
		select {
		case <-ctx.Done():
			logger.Info("Background chain parsing canceled by new request", "suggestions_stored", storedCount)
			return
		default:
		}

		if !stream.Receive() {
			break
		}
		resp := stream.Msg()

		if resp.DoneStream != nil && *resp.DoneStream {
			logger.Debug("Stream complete in goroutine")
			break
		}

		if resp.BeginEdit == nil || !*resp.BeginEdit {
			// Skip non-BeginEdit chunks (cursor_prediction_target etc.)
			continue
		}

		nextSuggestion, err := parseNextSuggestion(stream)
		if err != nil {
			logger.Error("Failed to parse chained suggestion", "error", err, "index", storedCount+2)
			break
		}
		if nextSuggestion == nil {
			break
		}

		thisID := generateSuggestionID()

		// Link previous suggestion to this one
		prevSuggestion := store.Get(prevID)
		if prevSuggestion != nil {
			prevSuggestion.NextSuggestionID = thisID
			store.Store(prevID, prevSuggestion)
		}

		nextSuggestion.NextSuggestionID = ""
		store.Store(thisID, nextSuggestion)
		storedCount++
		prevID = thisID

		logStoredSuggestion(thisID, storedCount+1, nextSuggestion)
	}

	if err := stream.Err(); err != nil {
		logger.Debug("Stream error in goroutine", "error", err)
	}

	logger.Info("Background chain parsing complete", "suggestions_stored", storedCount)
}

func logStoredSuggestion(id string, index int, s *suggestionstore.Suggestion) {
	logAttrs := []any{
		"suggestion_id", id,
		"index", index,
		"chars", len(s.Text),
		"suggestion_text", s.Text,
	}
	if s.Range != nil {
		logAttrs = append(logAttrs, "range_start_line", s.Range.StartLine)
		logAttrs = append(logAttrs, "range_end_line", s.Range.EndLine)
	}
	logger.Info("Stored chained suggestion", logAttrs...)
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
			// Capture suggestion confidence score
			if resp.SuggestionConfidence != nil {
				conf := *resp.SuggestionConfidence
				currentSuggestion.SuggestionConfidence = &conf
				logger.Debug("Suggestion confidence received", "confidence", conf)
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
			if currentSuggestion == nil {
				// No suggestion text was received, skip
				logger.Debug("DoneEdit received but no suggestion text")
				return nil, nil
			}

			// Strip leading newline if requested
			if currentSuggestion.ShouldRemoveLeadingEol && len(currentSuggestion.Text) > 0 {
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
		SuggestionConfidence:   suggestion.SuggestionConfidence,
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
