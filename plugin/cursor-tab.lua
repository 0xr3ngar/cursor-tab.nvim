-- cursor-tab plugin loader
-- Do NOT auto-call setup() here; let the user's config call
-- require("cursor-tab").setup(opts) with their own options (e.g. server_path).
if vim.g.loaded_cursor_tab then
	return
end
vim.g.loaded_cursor_tab = 1
