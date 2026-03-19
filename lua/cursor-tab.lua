local M = {}

M.ns_id = vim.api.nvim_create_namespace("cursor_tab")
M.current_suggestion = nil
M.current_suggestion_text = nil
M.current_line = nil
M.current_col = nil
M.accepting = false
M.server_url = nil
M.server_port = nil
M.server_ready = false
M.server_path = nil
M.server_job = nil
M.debounce_timer = nil
M.debounce_time_ms = 150
M.enabled = true
M.pending_job = nil
M.next_suggestion_id = nil
M.jump_marker = nil
M.is_nes_active = false
M.last_cursor_line = nil
M.file_versions = {} -- per-buffer monotonic version counter (bufnr -> int)
M.buf_last_viewed = {} -- per-buffer last viewed timestamp (bufnr -> epoch float)
M.min_confidence = 0 -- minimum suggestion confidence to display (0-100, 0 = show all)

-- Simple logging function
function M.log(msg)
	local f = io.open("/tmp/cursor-tab-debug.log", "a")
	if f then
		f:write(os.date("%H:%M:%S") .. " " .. msg .. "\n")
		f:close()
	end
end

-- Record a suggestion-application diff to the server for file_diff_histories.
-- This matches the Cursor IDE reference: only tracks diffs from accepted suggestions,
-- not user edits. Format: "lineNum-|old\nlineNum+|new\n", sliding window of 3.
function M.record_diff(start_line, old_lines, new_lines)
	if not M.server_ready or not M.server_url then
		return
	end

	local file_path = vim.fn.expand("%:p")
	local req = {
		file_path = file_path,
		start_line = start_line,  -- 0-indexed
		old_lines = old_lines,
		new_lines = new_lines,
	}

	local json_data = vim.fn.json_encode(req)
	M.log("record_diff: start=" .. start_line .. " old=" .. #old_lines .. " new=" .. #new_lines)

	vim.fn.jobstart({
		"curl", "-s", "-X", "POST",
		"-H", "Content-Type: application/json",
		"-d", json_data,
		M.server_url .. "/diff/record",
	}, {
		on_stdout = function(_, data)
			if data and #data > 0 then
				local text = table.concat(data, "")
				if text ~= "" then
					M.log("record_diff response: " .. text)
				end
			end
		end,
		stdout_buffered = true,
	})
end

function M.setup(opts)
	opts = opts or {}

	-- Confidence threshold: 0 = show all, 50 = default (50%), 100 = only highest confidence
	if opts.min_confidence ~= nil then
		M.min_confidence = opts.min_confidence
	end

	-- Define highlights
	vim.api.nvim_set_hl(0, "CursorTabJumpMarker", {
		fg = "#000000",
		bg = "#50fa7b",
		bold = true,
	})
	vim.api.nvim_set_hl(0, "CursorTabNES", {
		fg = "#e0ffe0",
		bg = "#2d4a2d",
		italic = true,
	})
	vim.api.nvim_set_hl(0, "CursorTabNESLine", {
		bg = "#1a2e1a",
	})
	vim.api.nvim_set_hl(0, "CursorTabOldText", {
		fg = "#ff6b6b",
		bg = "#2a1a1a",
		strikethrough = true,
	})
	vim.api.nvim_set_hl(0, "CursorTabOldLine", {
		fg = "#4a4a4a",
		bg = "#1a1a1a",
		strikethrough = true,
	})
	vim.api.nvim_set_hl(0, "CursorTabGhost", {
		fg = "#6a737d",
		italic = true,
	})

	if not opts.server_path then
		local installer = require("cursor-tab.installer")
		local source = debug.getinfo(1, "S").source
		local plugin_dir = vim.fn.fnamemodify(source:sub(2), ":h:h")

		-- Try to ensure binary exists (download if needed)
		if not installer.ensure_binary(plugin_dir) then
			vim.notify("cursor-tab: Failed to install server binary. Try running :CursorTabInstall", vim.log.levels.ERROR)
			return
		end

		M.server_path = installer.get_binary_path(plugin_dir)
	else
		M.server_path = opts.server_path
	end

	M.ensure_server()

	-- Cleanup server on Neovim exit
	vim.api.nvim_create_autocmd("VimLeavePre", {
		callback = function()
			if M.server_job then
				vim.fn.jobstop(M.server_job)
				M.server_job = nil
			end
		end,
	})

	-- Track file version (monotonic counter per buffer, incremented on every text change)
	vim.api.nvim_create_autocmd({ "TextChangedI", "TextChanged" }, {
		callback = function()
			local bufnr = vim.api.nvim_get_current_buf()
			M.file_versions[bufnr] = (M.file_versions[bufnr] or 0) + 1
		end,
	})

	-- Track buffer last-viewed timestamps for additional_files
	vim.api.nvim_create_autocmd({ "BufEnter", "BufWinEnter" }, {
		callback = function()
			local bufnr = vim.api.nvim_get_current_buf()
			M.buf_last_viewed[bufnr] = vim.loop.now() / 1000.0 -- epoch seconds as float
		end,
	})

	vim.api.nvim_create_autocmd({ "TextChangedI" }, {
		callback = function()
			if not M.is_nes_active then
				M.show_suggestion()
			end
		end,
	})

	-- Trigger suggestions on normal mode edits (dd, p, x, etc.)
	vim.api.nvim_create_autocmd({ "TextChanged" }, {
		callback = function()
			if not M.is_nes_active and not M.accepting then
				M.show_suggestion()
			end
		end,
	})

	vim.api.nvim_create_autocmd({ "InsertLeave" }, {
		callback = function()
			-- Clear any inline suggestion when leaving insert mode
			-- but preserve NES suggestions (they work in normal mode too)
			if not M.is_nes_active then
				M.clear_suggestion()
			end
		end,
	})

	-- Insert mode Tab
	vim.keymap.set("i", "<Tab>", function()
		if M.accept_suggestion() then
			return ""
		else
			return "\t"
		end
	end, { noremap = true, silent = true, expr = true })

	-- Normal mode Tab for accepting NES suggestions
	vim.keymap.set("n", "<Tab>", function()
		if M.accept_suggestion() then
			return ""
		else
			-- Fallback to default Tab behavior
			return "\t"
		end
	end, { noremap = true, silent = true, expr = true })

	-- Escape clears any active suggestion/NES in normal mode
	vim.keymap.set("n", "<Esc>", function()
		if M.current_suggestion or M.is_nes_active then
			M.clear_suggestion()
			return ""
		end
		-- Fallback to default Escape behavior
		return "<Esc>"
	end, { noremap = true, silent = true, expr = true })

	vim.api.nvim_create_user_command("CursorTab", function(args)
		if args.args == "toggle" then
			M.enabled = not M.enabled
			if M.enabled then
				vim.notify("CursorTab enabled", vim.log.levels.INFO)
			else
				M.clear_suggestion()
				vim.notify("CursorTab disabled", vim.log.levels.INFO)
			end
		elseif args.args == "enable" then
			M.enabled = true
			vim.notify("CursorTab enabled", vim.log.levels.INFO)
		elseif args.args == "disable" then
			M.enabled = false
			M.clear_suggestion()
			vim.notify("CursorTab disabled", vim.log.levels.INFO)
		else
			vim.notify("Usage: :CursorTab [toggle|enable|disable]", vim.log.levels.ERROR)
		end
	end, {
		nargs = 1,
		complete = function()
			return { "toggle", "enable", "disable" }
		end,
	})

	vim.api.nvim_create_user_command("CursorTabInstall", function()
		local installer = require("cursor-tab.installer")
		local source = debug.getinfo(1, "S").source
		local plugin_dir = vim.fn.fnamemodify(source:sub(2), ":h:h")

		installer.download_binary(plugin_dir, function(success)
			if success then
				vim.notify("cursor-tab: Installation complete. Restart Neovim.", vim.log.levels.INFO)
			else
				vim.notify("cursor-tab: Installation failed", vim.log.levels.ERROR)
			end
		end)
	end, {})
end

function M.ensure_server()
	if M.server_job and vim.fn.jobwait({ M.server_job }, 0)[1] == -1 then
		return true
	end

	if not M.server_path then
		return false
	end

	-- Reset state
	M.server_ready = false
	M.server_port = nil
	M.server_url = nil

	M.server_job = vim.fn.jobstart({ M.server_path, "--port", "0" }, {
		on_stdout = function(_, data)
			if data and #data > 0 then
				for _, line in ipairs(data) do
					-- Parse "SERVER_PORT=12345" from stdout
					local port = line:match("SERVER_PORT=(%d+)")
					if port then
						M.server_port = tonumber(port)
						M.server_url = "http://localhost:" .. M.server_port
						M.server_ready = true
					end
				end
			end
		end,
		on_exit = function(_, exit_code)
			if exit_code ~= 0 then
				vim.notify("cursor-tab server exited with code " .. exit_code, vim.log.levels.ERROR)
			end
			M.server_job = nil
			M.server_ready = false
			M.server_port = nil
			M.server_url = nil
		end,
		on_stderr = function(_, data)
			if data and #data > 0 and data[1] ~= "" then
				vim.notify("cursor-tab server: " .. table.concat(data, "\n"), vim.log.levels.WARN)
			end
		end,
	})

	if M.server_job == 0 or M.server_job == -1 then
		vim.notify("Failed to start cursor-tab server at " .. M.server_path, vim.log.levels.ERROR)
		M.server_job = nil
		return false
	end

	-- Wait a bit for server to initialize and report its port
	vim.defer_fn(function() end, 100)
	return true
end

-- Collect LSP signature help (parameter hints) for the current cursor position.
-- Returns a list of {label=string, documentation=string|nil} or empty table.
function M.get_parameter_hints(bufnr, line, col)
	local hints = {}
	local clients = vim.lsp.get_clients({ bufnr = bufnr })
	if #clients == 0 then
		return hints
	end

	-- Only query clients that support signatureHelp
	for _, client in ipairs(clients) do
		if client.server_capabilities.signatureHelpProvider then
			local params = vim.lsp.util.make_position_params(0, client.offset_encoding)
			local result = client.request_sync("textDocument/signatureHelp", params, 200, bufnr)
			if result and result.result and result.result.signatures then
				for _, sig in ipairs(result.result.signatures) do
					local hint = { label = sig.label }
					if sig.documentation then
						if type(sig.documentation) == "string" then
							hint.documentation = sig.documentation
						elseif type(sig.documentation) == "table" and sig.documentation.value then
							hint.documentation = sig.documentation.value
						end
					end
					table.insert(hints, hint)
				end
			end
		end
	end
	return hints
end

function M.get_suggestion(suggestion_id, callback, intent)
	if not M.ensure_server() then
		if callback then
			callback(nil)
		end
		return
	end

	-- Wait for server to be ready (port discovered)
	if not M.server_ready or not M.server_url then
		-- Retry after a short delay
		vim.defer_fn(function()
			M.get_suggestion(suggestion_id, callback, intent)
		end, 50)
		return
	end

	if M.pending_job then
		vim.fn.jobstop(M.pending_job)
		M.pending_job = nil
	end

	if suggestion_id then
		-- GET existing suggestion from store
		M.log("get_suggestion called with ID: " .. suggestion_id)
		M.pending_job = vim.fn.jobstart({
			"curl",
			"-s",
			"-X",
			"GET",
			M.server_url .. "/suggestion/" .. suggestion_id,
		}, {
			on_stdout = function(_, data)
				if not data or #data == 0 then
					return
				end

				local response_text = table.concat(data, "\n")
				if response_text == "" then
					return
				end

				local ok, response = pcall(vim.fn.json_decode, response_text)
				if ok and response and response.suggestion then
					if callback then
						callback(response.suggestion, response.range_replace, response.next_suggestion_id, response.should_remove_leading_eol)
					end
				else
					if callback then
						callback(nil, nil, nil, false)
					end
				end

				M.pending_job = nil
			end,
			on_exit = function()
				M.pending_job = nil
			end,
			stdout_buffered = true,
		})
	else
		-- POST new suggestion request to Cursor
		local bufnr = vim.api.nvim_get_current_buf()
		local cursor = vim.api.nvim_win_get_cursor(0)
		local line = cursor[1] - 1
		local col = cursor[2]
		local workspace_path = vim.fn.getcwd()

		-- Collect LSP diagnostics for current buffer
		local diagnostics = {}
		local raw_diagnostics = vim.diagnostic.get(bufnr)
		for _, d in ipairs(raw_diagnostics) do
			-- Map vim.diagnostic.severity to proto DiagnosticSeverity values:
			-- vim: ERROR=1, WARN=2, INFO=3, HINT=4
			-- proto: ERROR=1, WARNING=2, INFORMATION=3, HINT=4 (same mapping)
			local severity = d.severity or 1
			table.insert(diagnostics, {
				message = d.message or "",
				severity = severity,
				start_line = d.lnum or 0,       -- 0-indexed
				start_col = d.col or 0,          -- 0-indexed
				end_line = d.end_lnum or d.lnum or 0,
				end_col = d.end_col or d.col or 0,
				source = d.source or "",
			})
		end

		-- Collect additional files: other open listed buffers with their paths
		local additional_files = {}
		local current_file = vim.fn.expand("%:p")
		for _, b in ipairs(vim.api.nvim_list_bufs()) do
			if vim.api.nvim_buf_is_loaded(b) and vim.bo[b].buflisted and b ~= bufnr then
				local bname = vim.api.nvim_buf_get_name(b)
				if bname ~= "" and bname ~= current_file then
					local entry = {
						relative_workspace_path = bname,
						is_open = true,
						last_viewed_at = M.buf_last_viewed[b] or 0,
					}
					-- Get visible range content for buffers currently in a window
					for _, win in ipairs(vim.api.nvim_list_wins()) do
						if vim.api.nvim_win_get_buf(win) == b then
							local top = vim.fn.line("w0", win)
							local bot = vim.fn.line("w$", win)
							local visible_lines = vim.api.nvim_buf_get_lines(b, top - 1, bot, false)
							entry.visible_range_content = visible_lines
							entry.start_line_number_one_indexed = top
							break
						end
					end
					table.insert(additional_files, entry)
				end
			end
		end

		-- Line ending detection
		local line_ending = vim.bo.fileformat == "dos" and "\r\n" or "\n"

		-- File version (monotonic counter)
		local file_version = M.file_versions[bufnr] or 0

		-- Collect LSP parameter/signature hints for function call context
		local parameter_hints = M.get_parameter_hints(bufnr, line, col)

		local req = {
			file_contents = table.concat(vim.api.nvim_buf_get_lines(bufnr, 0, -1, false), "\n"),
			line = line,
			column = col,
			file_path = vim.fn.expand("%:p"),
			language_id = vim.bo.filetype,
			workspace_path = workspace_path,
			intent = intent or "typing",
			diagnostics = diagnostics,
			additional_files = additional_files,
			line_ending = line_ending,
			file_version = file_version,
			client_time = os.clock(), -- epoch-style timing
			parameter_hints = parameter_hints,
		}

		local json_data = vim.fn.json_encode(req)

		M.pending_job = vim.fn.jobstart({
			"curl",
			"-s",
			"-X",
			"POST",
			"-H",
			"Content-Type: application/json",
			"-d",
			json_data,
			M.server_url .. "/suggestion/new",
		}, {
			on_stdout = function(_, data)
				if not data or #data == 0 then
					return
				end

				local response_text = table.concat(data, "\n")
				if response_text == "" then
					return
				end

				local ok, response = pcall(vim.fn.json_decode, response_text)
				if ok and response and response.suggestion then
					-- Check confidence threshold: skip low-confidence suggestions
					local confidence = response.suggestion_confidence
					if confidence and M.min_confidence > 0 and confidence < M.min_confidence then
						M.log("Skipping low-confidence suggestion: " .. confidence .. " < " .. M.min_confidence)
						if callback then
							callback(nil, nil, nil, false)
						end
					elseif callback then
						callback(response.suggestion, response.range_replace, response.next_suggestion_id, response.should_remove_leading_eol)
					end
				else
					if callback then
						callback(nil, nil, nil, false)
					end
				end

				M.pending_job = nil
			end,
			on_exit = function()
				M.pending_job = nil
			end,
			stdout_buffered = true,
		})
	end
end

-- Shared NES diff display: red old lines above, green new lines above original, dim original
function M.display_nes_diff(bufnr, start_line, end_line, new_text_lines)
	local line_count = vim.api.nvim_buf_line_count(bufnr)
	local display_line = start_line

	-- Get original text that will be replaced
	local original_lines = {}
	if start_line <= end_line and start_line < line_count then
		local actual_end = math.min(end_line + 1, line_count)
		original_lines = vim.api.nvim_buf_get_lines(bufnr, start_line, actual_end, false)
	end

	-- Build combined virt_lines: red old + green new, shown ABOVE the target line
	local virt_above = {}

	-- Red old text (strikethrough)
	for _, old_text in ipairs(original_lines) do
		table.insert(virt_above, { { "  " .. old_text, "CursorTabOldText" } })
	end

	-- Green new text
	for _, new_text in ipairs(new_text_lines) do
		table.insert(virt_above, { { "  " .. new_text, "CursorTabNES" } })
	end

	if #virt_above > 0 then
		vim.api.nvim_buf_set_extmark(bufnr, M.ns_id, display_line, 0, {
			virt_lines = virt_above,
			virt_lines_above = true,
		})
	end

	-- Dim/hide original lines in the replacement range
	for l = start_line, math.min(end_line, line_count - 1) do
		vim.api.nvim_buf_set_extmark(bufnr, M.ns_id, l, 0, {
			line_hl_group = "CursorTabOldLine",
		})
	end

	-- Tab marker on first target line
	vim.api.nvim_buf_set_extmark(bufnr, M.ns_id, display_line, 0, {
		virt_text = { { " Tab ", "CursorTabJumpMarker" } },
		virt_text_pos = "right_align",
		hl_mode = "combine",
	})

	return display_line
end

function M.show_suggestion(suggestion_id, intent)
	if not M.enabled or (M.accepting and not suggestion_id) then
		return
	end

	if M.debounce_timer then
		vim.fn.timer_stop(M.debounce_timer)
		M.debounce_timer = nil
	end

	M.clear_suggestion()

	-- If suggestion_id provided, get next suggestion immediately without debouncing
	if suggestion_id then
		M.log("show_suggestion called with ID: " .. suggestion_id)
		M.get_suggestion(suggestion_id, function(suggestion, range_replace, next_suggestion_id, should_remove_leading_eol)
			if not suggestion or suggestion == "" then
				M.log("NES: no suggestion returned for ID " .. suggestion_id)
				M.accepting = false
				return
			end

			-- Strip carriage returns
			suggestion = suggestion:gsub("\r", "")

			M.log("NES show: suggestion=" .. string.sub(suggestion, 1, 120)
				.. " range=" .. vim.inspect(range_replace)
				.. " next_id=" .. tostring(next_suggestion_id))

			-- Store for acceptance
			M.current_suggestion_text = suggestion
			M.current_range_replace = range_replace
			M.next_suggestion_id = next_suggestion_id
			M.is_nes_active = true

			local bufnr = vim.api.nvim_get_current_buf()
			local line_count = vim.api.nvim_buf_line_count(bufnr)

			-- Determine where to display based on range_replace
			local display_line = vim.api.nvim_win_get_cursor(0)[1] - 1
			local start_line = display_line
			local end_line = display_line

			if range_replace then
				start_line = range_replace.start_line - 1
				end_line = range_replace.end_line - 1
			end

			-- Validate bounds
			if start_line < 0 then start_line = 0 end
			if end_line < 0 then end_line = 0 end
			if start_line >= line_count then start_line = line_count - 1 end
			if end_line >= line_count then end_line = line_count - 1 end
			display_line = start_line

			-- Strip leading newline for display
			local display_text = suggestion
			if vim.startswith(display_text, "\n") then
				display_text = string.sub(display_text, 2)
			end

			local lines = vim.split(display_text, "\n", { plain = true })

			-- Use shared NES diff display
			local display_line = M.display_nes_diff(bufnr, start_line, end_line, lines)

			M.current_line = display_line
			M.current_col = 0

			-- Move cursor to the target line (for normal mode Tab)
			vim.api.nvim_win_set_cursor(0, { display_line + 1, 0 })

			M.log("NES display: shown at line=" .. display_line
				.. " text_lines=" .. #lines)

			-- Done showing next suggestion, allow new suggestions
			M.accepting = false

			-- Set up autocmd to clear NES if user moves away or types
			M.setup_nes_clear_autocmd()
		end)
		return
	end

	-- Otherwise, debounce and get new suggestion
	M.debounce_timer = vim.fn.timer_start(M.debounce_time_ms, function()
		M.debounce_timer = nil

		local line = vim.api.nvim_win_get_cursor(0)[1] - 1
		local col = vim.api.nvim_win_get_cursor(0)[2]
		local mode = vim.api.nvim_get_mode().mode

		-- Compute intent source matching Cursor IDE behavior:
		-- "typing" = default (cursor on same line)
		-- "line_changed" = cursor moved to different line since last request
		-- "cursor_prediction" = after accepting a suggestion (passed explicitly)
		local effective_intent
		if intent == "cursor_prediction" then
			effective_intent = "cursor_prediction"
		elseif M.last_cursor_line ~= nil and M.last_cursor_line ~= line then
			effective_intent = "line_changed"
		else
			effective_intent = "typing"
		end
		M.last_cursor_line = line

		M.log("show_suggestion debounce: mode=" .. mode .. " intent=" .. effective_intent)

		M.get_suggestion(nil, function(suggestion, range_replace, next_suggestion_id, should_remove_leading_eol)
			if not suggestion or suggestion == "" then
				return
			end

			-- Strip carriage returns (Windows line endings)
			suggestion = suggestion:gsub("\r", "")

			M.log("New suggestion: text=" .. string.sub(suggestion, 1, 80)
				.. " range=" .. vim.inspect(range_replace)
				.. " next_id=" .. tostring(next_suggestion_id)
				.. " mode=" .. mode)

			-- Re-check cursor position and validate it hasn't changed
			local current_line = vim.api.nvim_win_get_cursor(0)[1] - 1
			local current_col = vim.api.nvim_win_get_cursor(0)[2]

			-- Validate the position is still valid
			-- In normal mode, only check line (col shifts after dd/x/etc.)
			if mode == "n" then
				if current_line ~= line then
					M.log("Position check FAILED (normal): line moved " .. line .. " -> " .. current_line)
					return
				end
				if current_col ~= col then
					M.log("Position check: col shifted in normal mode " .. col .. " -> " .. current_col .. " (OK, continuing)")
				end
			else
				if current_line ~= line or current_col ~= col then
					M.log("Position check FAILED (insert): " .. line .. ":" .. col .. " -> " .. current_line .. ":" .. current_col)
					return -- Cursor moved, discard this suggestion
				end
			end

			-- Update col to current value (may have shifted in normal mode after dd/x/etc.)
			col = current_col

			-- Validate line is within bounds
			local bufnr = vim.api.nvim_get_current_buf()
			local line_text = vim.api.nvim_buf_get_lines(bufnr, line, line + 1, false)[1]
			if not line_text then
				return
			end

			-- In normal mode, col check is less strict (cursor can be at end of line)
			if mode == "i" and col > #line_text then
				return
			end

			M.clear_suggestion()
			M.current_suggestion_text = suggestion
			M.current_range_replace = range_replace
			M.next_suggestion_id = next_suggestion_id

			-- Check if this should show as NES diff view:
			-- 1. Targets a different line (remote suggestion)
			-- 2. We're in normal mode (any suggestion with a range)
			local is_nes_style = false
			if range_replace then
				local start_line = range_replace.start_line - 1
				if start_line ~= line or mode == "n" then
					is_nes_style = true
				end
			end

			-- Show as NES diff view
			if is_nes_style then
				M.is_nes_active = true
				local line_count = vim.api.nvim_buf_line_count(bufnr)
				local start_line = range_replace.start_line - 1
				local end_line = range_replace.end_line - 1

				if start_line < 0 then start_line = 0 end
				if end_line < 0 then end_line = 0 end
				if start_line >= line_count then start_line = line_count - 1 end
				if end_line >= line_count then end_line = line_count - 1 end

				local display_text = suggestion
				if vim.startswith(display_text, "\n") then
					display_text = string.sub(display_text, 2)
				end
				local new_lines = vim.split(display_text, "\n", { plain = true })

				local display_line = M.display_nes_diff(bufnr, start_line, end_line, new_lines)

				M.current_line = display_line
				M.current_col = 0

				-- Move cursor to NES target in normal mode so Tab works and
				-- the clear autocmd doesn't fire immediately due to distance
				if mode == "n" then
					vim.api.nvim_win_set_cursor(0, { display_line + 1, 0 })
					M.log("NES debounce: moved cursor to target line " .. display_line)
				end

				M.setup_nes_clear_autocmd()
				return
			end

			-- Standard inline suggestion display
			local display_text = suggestion
			local display_line = line
			local display_col = col

			if range_replace then
				-- LineRange only has line numbers, use cursor position for column precision
				local bufnr = vim.api.nvim_get_current_buf()
				-- API returns 1-indexed line numbers, convert to 0-indexed
				local start_line = range_replace.start_line - 1
				local end_line = range_replace.end_line - 1

				-- Use the range's line for display, but validate it first
				-- If the range extends beyond current buffer or is far from cursor, use request line
				local line_count = vim.api.nvim_buf_line_count(bufnr)
				local range_out_of_bounds = false
				local range_far_from_cursor = math.abs(start_line - line) > 5 -- More than 5 lines away

				if start_line >= 0 and start_line < line_count and not range_far_from_cursor then
					display_line = start_line
				else
					display_line = line
					range_out_of_bounds = true
				end

				-- For single-line replacements on current line, use cursor position
				-- Also apply if range was out of bounds (treat as same-line)
				if (start_line == line and end_line == line) or range_out_of_bounds then
					-- Strip leading newline if present (API sometimes includes it)
					local clean_suggestion = suggestion
					if vim.startswith(clean_suggestion, "\n") then
						clean_suggestion = string.sub(clean_suggestion, 2)
					end

					-- Get text from start of line to cursor (use display_line, not start_line)
					local current_line_text = vim.api.nvim_buf_get_lines(bufnr, display_line, display_line + 1, false)[1]
						or ""
					local replaced_text = string.sub(current_line_text, 1, col)

					-- If suggestion starts with the replaced text, strip it for display
					if vim.startswith(clean_suggestion, replaced_text) then
						display_text = string.sub(clean_suggestion, #replaced_text + 1)
					else
						display_text = clean_suggestion
					end
				end
				-- For multi-line replacements, show full suggestion
			end

			-- Validate display position is within bounds
			local bufnr = vim.api.nvim_get_current_buf()
			local line_count = vim.api.nvim_buf_line_count(bufnr)
			if display_line < 0 or display_line >= line_count then
				return
			end

			local display_line_text = vim.api.nvim_buf_get_lines(bufnr, display_line, display_line + 1, false)[1] or ""
			if display_col > #display_line_text then
				return
			end

			local lines = vim.split(display_text, "\n", { plain = true })
			local virt_lines = {}

			-- If suggestion starts with newline, first line will be empty
			-- In that case, show everything as virt_lines below current line
			if #lines > 0 and lines[1] == "" then
				-- Skip empty first line, show rest as virt_lines
				for i = 2, #lines do
					table.insert(virt_lines, { { lines[i], "CursorTabGhost" } })
				end
				if #virt_lines > 0 then
					M.current_suggestion = vim.api.nvim_buf_set_extmark(0, M.ns_id, display_line, display_col, {
						virt_lines = virt_lines,
						virt_lines_above = false,
					})
				end
			else
				-- Normal case: first line inline, rest as virt_lines
				for i, text in ipairs(lines) do
					if i == 1 then
						M.current_suggestion = vim.api.nvim_buf_set_extmark(0, M.ns_id, display_line, display_col, {
							virt_text = { { text, "CursorTabGhost" } },
							virt_text_pos = "inline",
							hl_mode = "combine",
						})
					else
						table.insert(virt_lines, { { text, "CursorTabGhost" } })
					end
				end

				if #virt_lines > 0 then
					vim.api.nvim_buf_set_extmark(0, M.ns_id, display_line, display_col, {
						virt_lines = virt_lines,
						virt_lines_above = false,
					})
				end
			end

			M.current_line = display_line
			M.current_col = display_col
		end, effective_intent)
	end)
end

function M.clear_suggestion()
	if M.current_suggestion or M.is_nes_active then
		vim.api.nvim_buf_clear_namespace(0, M.ns_id, 0, -1)
		M.current_suggestion = nil
		M.current_suggestion_text = nil
		M.current_range_replace = nil
		M.current_line = nil
		M.current_col = nil
		M.next_suggestion_id = nil
		M.is_nes_active = false
		-- Remove NES clear autocmd if it exists
		if M.nes_clear_augroup then
			pcall(vim.api.nvim_del_augroup_by_id, M.nes_clear_augroup)
			M.nes_clear_augroup = nil
		end
	end
end

-- Set up autocmds to clear NES display when user starts typing or makes other edits
function M.setup_nes_clear_autocmd()
	-- Remove any existing group first
	if M.nes_clear_augroup then
		pcall(vim.api.nvim_del_augroup_by_id, M.nes_clear_augroup)
	end

	-- Defer setup to skip the initial cursor positioning events
	vim.defer_fn(function()
		-- NES might have been cleared or accepted already
		if not M.is_nes_active then
			return
		end

		M.nes_clear_augroup = vim.api.nvim_create_augroup("CursorTabNESClear", { clear = true })

		-- Clear NES if user starts typing (not Tab)
		vim.api.nvim_create_autocmd({ "InsertCharPre" }, {
			group = M.nes_clear_augroup,
			callback = function()
				if M.is_nes_active and not M.accepting then
					M.log("NES cleared: user started typing")
					M.clear_suggestion()
				end
			end,
			once = true,
		})

		-- Clear NES if user makes a different edit in normal mode (dd, x, p, etc.)
		vim.api.nvim_create_autocmd({ "TextChanged" }, {
			group = M.nes_clear_augroup,
			callback = function()
				if M.is_nes_active and not M.accepting then
					M.log("NES cleared: text changed (user edit)")
					M.clear_suggestion()
				end
			end,
			once = true,
		})
	end, 50)
end

function M.accept_suggestion()
	if M.accepting then
		return false
	end
	-- Accept if we have an active inline suggestion OR an active NES
	if not M.current_suggestion and not M.is_nes_active then
		return false
	end
	if not M.current_suggestion_text then
		return false
	end

	local line = vim.api.nvim_win_get_cursor(0)[1] - 1
	local col = vim.api.nvim_win_get_cursor(0)[2]
	local suggestion = M.current_suggestion_text
	local range_replace = M.current_range_replace
	local next_suggestion_id = M.next_suggestion_id
	local was_nes = M.is_nes_active

	M.log("accept_suggestion: suggestion=" .. string.sub(suggestion, 1, 80)
		.. " range=" .. vim.inspect(range_replace)
		.. " next_id=" .. tostring(next_suggestion_id)
		.. " is_nes=" .. tostring(was_nes)
		.. " mode=" .. vim.api.nvim_get_mode().mode)

	M.accepting = true
	M.clear_suggestion()

	vim.schedule(function()
		-- Temporarily disable events during text insertion
		local eventignore_save = vim.o.eventignore
		vim.o.eventignore = "TextChangedI,TextChanged,CursorMoved,CursorMovedI"

		local bufnr = vim.api.nvim_get_current_buf()
		local line_count = vim.api.nvim_buf_line_count(bufnr)
		local new_lines = vim.split(suggestion, "\n", { plain = true })

		if range_replace then
			-- API returns 1-indexed line numbers, convert to 0-indexed
			local start_line = range_replace.start_line - 1
			local end_line = range_replace.end_line - 1

			M.log("accept: range start_line=" .. start_line .. " end_line=" .. end_line
				.. " cursor_line=" .. line .. " buf_lines=" .. line_count)

			-- Validate range is within bounds
			if start_line < 0 then start_line = 0 end
			if end_line < 0 then end_line = 0 end
			if start_line >= line_count then start_line = line_count - 1 end
			if end_line >= line_count then end_line = line_count - 1 end

			-- Handle special case: start_line > end_line means "insert after end_line"
			if start_line > end_line then
				local insert_after = end_line + 1
				M.log("accept: insert-between-lines at " .. insert_after)

				local clean = suggestion
				if clean:sub(1, 1) == "\n" then
					clean = clean:sub(2)
				end
				local insert_lines = vim.split(clean, "\n", { plain = true })

				-- Record diff: inserting new lines (no old lines)
				M.record_diff(insert_after, {}, insert_lines)

				vim.api.nvim_buf_set_lines(bufnr, insert_after, insert_after, false, insert_lines)
				local final_line = insert_after + #insert_lines - 1
				vim.api.nvim_win_set_cursor(0, { final_line + 1, #insert_lines[#insert_lines] })
			else
				-- Full range replacement: always replace entire lines when range_replace exists
				-- (covers NES, same-line, and multi-line cases)
				M.log("accept: full range replacement, lines " .. start_line .. "-" .. end_line)

				-- Capture old lines BEFORE replacement for diff history
				local old_lines = vim.api.nvim_buf_get_lines(bufnr, start_line, end_line + 1, false)

				local clean_suggestion = suggestion
				if vim.startswith(clean_suggestion, "\n") then
					clean_suggestion = string.sub(clean_suggestion, 2)
				end
				local replace_lines = vim.split(clean_suggestion, "\n", { plain = true })

				-- Record diff: old lines -> new lines
				M.record_diff(start_line, old_lines, replace_lines)

				vim.api.nvim_buf_set_lines(bufnr, start_line, end_line + 1, false, replace_lines)
				local final_line = start_line + #replace_lines - 1
				vim.api.nvim_win_set_cursor(0, { final_line + 1, #replace_lines[#replace_lines] })
			end
		else
			-- No range to replace, just insert at cursor
			M.log("accept: no range, inserting at cursor")
			local line_text = vim.api.nvim_get_current_line()
			if #new_lines == 1 then
				-- Record diff: old line -> old line with insertion
				local old_line_text = line_text
				local new_line_text = line_text:sub(1, col) .. suggestion .. line_text:sub(col + 1)
				M.record_diff(line, { old_line_text }, { new_line_text })

				vim.api.nvim_buf_set_text(bufnr, line, col, line, col, { suggestion })
				vim.api.nvim_win_set_cursor(0, { line + 1, col + #suggestion })
			else
				local before = line_text:sub(1, col)
				local after = line_text:sub(col + 1)

				new_lines[1] = before .. new_lines[1]
				new_lines[#new_lines] = new_lines[#new_lines] .. after

				-- Record diff: old single line -> new multi-line
				M.record_diff(line, { line_text }, new_lines)

				vim.api.nvim_buf_set_lines(bufnr, line, line + 1, false, new_lines)
				vim.api.nvim_win_set_cursor(0, { line + #new_lines, #new_lines[#new_lines] - #after })
			end
		end

		-- Restore eventignore
		vim.o.eventignore = eventignore_save

		-- If there's a next suggestion, immediately show it
		if next_suggestion_id then
			M.log("Scheduling next suggestion: " .. next_suggestion_id)
			vim.defer_fn(function()
				M.log("Showing next suggestion: " .. next_suggestion_id)
				M.show_suggestion(next_suggestion_id)
			end, 10)
		else
			M.log("No next suggestion, requesting more from API")
			M.accepting = false
			-- Auto-request new suggestions to extend the chain seamlessly
			-- Use "cursor_prediction" intent since we just accepted a suggestion
			vim.defer_fn(function()
				if not M.is_nes_active and not M.accepting then
					M.show_suggestion(nil, "cursor_prediction")
				end
			end, 50)
		end
	end)

	return true
end

return M
