package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/db"
	"nimbus/internal/sshkeys"
)

// Keys wraps the sshkeys.Service.
type Keys struct {
	svc *sshkeys.Service
}

func NewKeys(svc *sshkeys.Service) *Keys { return &Keys{svc: svc} }

// keyView is the JSON projection of a stored key. We need a wrapper because
// the DB struct hides the encrypted blobs (json:"-") and the frontend wants a
// derived has_private_key flag.
type keyView struct {
	ID            uint   `json:"id"`
	Name          string `json:"name"`
	Label         string `json:"label,omitempty"`
	PublicKey     string `json:"public_key"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	IsDefault     bool   `json:"is_default"`
	OwnerID       *uint  `json:"owner_id,omitempty"`
	Source        string `json:"source,omitempty"`
	HasPrivateKey bool   `json:"has_private_key"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

func toKeyView(k *db.SSHKey) keyView {
	return keyView{
		ID:            k.ID,
		Name:          k.Name,
		Label:         k.Label,
		PublicKey:     k.PublicKey,
		Fingerprint:   k.Fingerprint,
		IsDefault:     k.IsDefault,
		OwnerID:       k.OwnerID,
		Source:        k.Source,
		HasPrivateKey: k.HasPrivateKey(),
		CreatedAt:     k.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:     k.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

type createKeyRequest struct {
	Name       string `json:"name"`
	Label      string `json:"label,omitempty"`
	PublicKey  string `json:"public_key,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	Generate   bool   `json:"generate,omitempty"`
	SetDefault bool   `json:"set_default,omitempty"`
}

// Create handles POST /api/keys. On generate, the response includes the
// freshly minted private key (the only time it'll ever cross the wire from
// the server's perspective unless re-downloaded later).
func (h *Keys) Create(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}

	row, err := h.svc.Create(r.Context(), sshkeys.CreateRequest{
		Name:       req.Name,
		Label:      req.Label,
		PublicKey:  req.PublicKey,
		PrivateKey: req.PrivateKey,
		Generate:   req.Generate,
		SetDefault: req.SetDefault,
	})
	if err != nil {
		response.FromError(w, err)
		return
	}

	body := struct {
		keyView
		PrivateKey string `json:"private_key,omitempty"`
	}{keyView: toKeyView(row)}

	// Surface the private key in the response only when we just generated it
	// — for imports the user already has it.
	if req.Generate {
		_, priv, err := h.svc.GetPrivateKey(r.Context(), row.ID)
		if err == nil {
			body.PrivateKey = priv
		}
	}
	response.Created(w, body)
}

// List handles GET /api/keys.
func (h *Keys) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.List(r.Context(), nil)
	if err != nil {
		response.FromError(w, err)
		return
	}
	out := make([]keyView, 0, len(rows))
	for i := range rows {
		out = append(out, toKeyView(&rows[i]))
	}
	response.Success(w, out)
}

// Get handles GET /api/keys/{id}.
func (h *Keys) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	row, err := h.svc.Get(r.Context(), id)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, toKeyView(row))
}

// PrivateKey handles GET /api/keys/{id}/private-key.
func (h *Keys) PrivateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	name, priv, err := h.svc.GetPrivateKey(r.Context(), id)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, map[string]string{
		"key_name":    name,
		"private_key": priv,
	})
}

// SetDefault handles POST /api/keys/{id}/default.
func (h *Keys) SetDefault(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := h.svc.SetDefault(r.Context(), id); err != nil {
		response.FromError(w, err)
		return
	}
	response.NoContent(w)
}

// Delete handles DELETE /api/keys/{id}.
func (h *Keys) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), id); err != nil {
		response.FromError(w, err)
		return
	}
	response.NoContent(w)
}

func parseID(w http.ResponseWriter, r *http.Request) (uint, bool) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		response.BadRequest(w, "invalid id")
		return 0, false
	}
	return uint(id), true
}
