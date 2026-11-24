package router

import "net/http"

// health is deliberately unauthenticated so uptime monitors can use it.
func (h *Handler) health(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
