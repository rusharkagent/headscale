package hscontrol

// Tailnet REST API — /api/v1/tailnet
//
// This package adds a plain JSON REST surface for multi-tenancy tailnet
// management. It is registered in createRouter() before the gRPC-gateway
// wildcard so specific paths take priority.
//
// Endpoints:
//   GET    /api/v1/tailnet          — list all tailnets
//   POST   /api/v1/tailnet          — create a tailnet
//   GET    /api/v1/tailnet/{id}     — get a tailnet by ID
//   PUT    /api/v1/tailnet/{id}     — update a tailnet (base_domain, acl_policy)
//   DELETE /api/v1/tailnet/{id}     — delete a tailnet
//   PUT    /api/v1/tailnet/{id}/policy — set the ACL policy for a tailnet

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/juanfont/headscale/hscontrol/db"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/rs/zerolog/log"
)

// ---- Wire types (JSON) -------------------------------------------------------

type tailnetResponse struct {
	ID         uint      `json:"id"`
	Name       string    `json:"name"`
	IPv4Prefix string    `json:"ipv4_prefix,omitempty"`
	IPv6Prefix string    `json:"ipv6_prefix,omitempty"`
	BaseDomain string    `json:"base_domain,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type createTailnetRequest struct {
	Name       string `json:"name"`
	IPv4Prefix string `json:"ipv4_prefix,omitempty"`
	IPv6Prefix string `json:"ipv6_prefix,omitempty"`
	BaseDomain string `json:"base_domain,omitempty"`
	ACLPolicy  string `json:"acl_policy,omitempty"`
}

type updateTailnetRequest struct {
	BaseDomain string `json:"base_domain,omitempty"`
	ACLPolicy  string `json:"acl_policy,omitempty"`
}

type setPolicyRequest struct {
	Policy string `json:"policy"`
}

func tailnetToResponse(tn types.Tailnet) tailnetResponse {
	resp := tailnetResponse{
		ID:         tn.ID,
		Name:       tn.Name,
		BaseDomain: tn.BaseDomain,
		CreatedAt:  tn.CreatedAt,
		UpdatedAt:  tn.UpdatedAt,
	}

	if tn.IPv4Prefix.IsValid() {
		resp.IPv4Prefix = tn.IPv4Prefix.String()
	}

	if tn.IPv6Prefix.IsValid() {
		resp.IPv6Prefix = tn.IPv6Prefix.String()
	}

	return resp
}

// ---- Handler helpers ---------------------------------------------------------

func writeJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Error().Err(err).Msg("tailnet API: writing JSON response")
	}
}

func writeError(w http.ResponseWriter, statusCode int, msg string) {
	writeJSON(w, statusCode, map[string]string{"error": msg})
}

func tailnetIDFromURL(r *http.Request) (uint, bool) {
	idStr := chi.URLParam(r, "id")

	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return 0, false
	}

	return uint(id), true //nolint:gosec
}

// ---- Handlers ----------------------------------------------------------------

// listTailnets handles GET /api/v1/tailnet
func (h *Headscale) listTailnets(w http.ResponseWriter, r *http.Request) {
	tailnets, err := h.state.GetDB().ListTailnets()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := make([]tailnetResponse, 0, len(tailnets))
	for _, tn := range tailnets {
		resp = append(resp, tailnetToResponse(tn))
	}

	writeJSON(w, http.StatusOK, resp)
}

// getTailnet handles GET /api/v1/tailnet/{id}
func (h *Headscale) getTailnet(w http.ResponseWriter, r *http.Request) {
	id, ok := tailnetIDFromURL(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid tailnet id")
		return
	}

	tn, err := h.state.GetDB().GetTailnetByID(id)
	if err != nil {
		if errors.Is(err, db.ErrTailnetNotFound) {
			writeError(w, http.StatusNotFound, "tailnet not found")
			return
		}

		writeError(w, http.StatusInternalServerError, err.Error())

		return
	}

	writeJSON(w, http.StatusOK, tailnetToResponse(*tn))
}

// createTailnet handles POST /api/v1/tailnet
func (h *Headscale) createTailnet(w http.ResponseWriter, r *http.Request) {
	var req createTailnetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	tn := types.Tailnet{
		Name:       req.Name,
		BaseDomain: req.BaseDomain,
		ACLPolicy:  req.ACLPolicy,
	}

	if req.IPv4Prefix != "" {
		p, err := netip.ParsePrefix(req.IPv4Prefix)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid ipv4_prefix: "+err.Error())
			return
		}

		tn.IPv4Prefix = p
	}

	if req.IPv6Prefix != "" {
		p, err := netip.ParsePrefix(req.IPv6Prefix)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid ipv6_prefix: "+err.Error())
			return
		}

		tn.IPv6Prefix = p
	}

	if err := h.state.GetDB().CreateTailnet(&tn); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Register a new IP allocator for this tailnet if it has its own prefix.
	if tn.IPv4Prefix.IsValid() || tn.IPv6Prefix.IsValid() {
		var p4, p6 *netip.Prefix
		if tn.IPv4Prefix.IsValid() {
			p := tn.IPv4Prefix
			p4 = &p
		}

		if tn.IPv6Prefix.IsValid() {
			p := tn.IPv6Prefix
			p6 = &p
		}

		if err := h.state.RegisterTailnetAllocator(tn.ID, p4, p6); err != nil {
			log.Warn().Err(err).Uint("tailnet.id", tn.ID).Msg("failed to register IP allocator for new tailnet")
		}
	}

	// Register per-tailnet policy manager if ACL was provided.
	if req.ACLPolicy != "" {
		if err := h.state.RegisterTailnetPolicy(tn.ID, []byte(req.ACLPolicy)); err != nil {
			log.Warn().Err(err).Uint("tailnet.id", tn.ID).Msg("failed to register policy manager for new tailnet")
		}
	}

	// Update tailnet cache.
	h.state.UpdateTailnetCache(tn)

	writeJSON(w, http.StatusCreated, tailnetToResponse(tn))
}

// updateTailnet handles PUT /api/v1/tailnet/{id}
func (h *Headscale) updateTailnet(w http.ResponseWriter, r *http.Request) {
	id, ok := tailnetIDFromURL(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid tailnet id")
		return
	}

	tn, err := h.state.GetDB().GetTailnetByID(id)
	if err != nil {
		if errors.Is(err, db.ErrTailnetNotFound) {
			writeError(w, http.StatusNotFound, "tailnet not found")
			return
		}

		writeError(w, http.StatusInternalServerError, err.Error())

		return
	}

	var req updateTailnetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.BaseDomain != "" {
		tn.BaseDomain = req.BaseDomain
	}

	if req.ACLPolicy != "" {
		tn.ACLPolicy = req.ACLPolicy
	}

	if err := h.state.GetDB().DB.Save(tn).Error; err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Update in-memory caches.
	h.state.UpdateTailnetCache(*tn)

	if req.ACLPolicy != "" {
		if err := h.state.RegisterTailnetPolicy(tn.ID, []byte(req.ACLPolicy)); err != nil {
			log.Warn().Err(err).Uint("tailnet.id", tn.ID).Msg("failed to update policy manager for tailnet")
		}
	}

	writeJSON(w, http.StatusOK, tailnetToResponse(*tn))
}

// deleteTailnet handles DELETE /api/v1/tailnet/{id}
func (h *Headscale) deleteTailnet(w http.ResponseWriter, r *http.Request) {
	id, ok := tailnetIDFromURL(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid tailnet id")
		return
	}

	tn, err := h.state.GetDB().GetTailnetByID(id)
	if err != nil {
		if errors.Is(err, db.ErrTailnetNotFound) {
			writeError(w, http.StatusNotFound, "tailnet not found")
			return
		}

		writeError(w, http.StatusInternalServerError, err.Error())

		return
	}

	// Soft-delete via GORM.
	if err := h.state.GetDB().DB.Delete(tn).Error; err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// setTailnetPolicy handles PUT /api/v1/tailnet/{id}/policy
func (h *Headscale) setTailnetPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := tailnetIDFromURL(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid tailnet id")
		return
	}

	tn, err := h.state.GetDB().GetTailnetByID(id)
	if err != nil {
		if errors.Is(err, db.ErrTailnetNotFound) {
			writeError(w, http.StatusNotFound, "tailnet not found")
			return
		}

		writeError(w, http.StatusInternalServerError, err.Error())

		return
	}

	var req setPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.Policy == "" {
		writeError(w, http.StatusBadRequest, "policy is required")
		return
	}

	tn.ACLPolicy = req.Policy

	if err := h.state.GetDB().DB.Save(tn).Error; err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Re-register per-tailnet policy manager with new policy.
	if err := h.state.RegisterTailnetPolicy(tn.ID, []byte(req.Policy)); err != nil {
		writeError(w, http.StatusInternalServerError, "policy saved but failed to apply: "+err.Error())
		return
	}

	h.state.UpdateTailnetCache(*tn)

	writeJSON(w, http.StatusOK, tailnetToResponse(*tn))
}
