package router

import (
	"net/http"
	"sort"
)

// models exposes enabled aliases as OpenAI-compatible model IDs.
func (h *Handler) models(w http.ResponseWriter) {
	aliases := h.cfg.EnabledAliases()
	sort.Strings(aliases)
	data := make([]map[string]any, 0, len(aliases))
	for _, alias := range aliases {
		data = append(data, map[string]any{"id": alias, "object": "model", "owned_by": "padi-router"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
