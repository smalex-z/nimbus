package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"nimbus/internal/api/response"
	"nimbus/internal/audit"
	"nimbus/internal/ctxutil"
	"nimbus/internal/db"
	"nimbus/internal/sshkeys"
)

// Keys wraps the sshkeys.Service.
type Keys struct {
	svc   *sshkeys.Service
	audit *audit.Service
}

func NewKeys(svc *sshkeys.Service) *Keys { return &Keys{svc: svc} }

// WithAudit installs the audit-log sink. Nil disables emission.
func (h *Keys) WithAudit(a *audit.Service) *Keys { h.audit = a; return h }

// keyView is the JSON projection of a stored key. We need a wrapper because
// the DB struct hides the encrypted blobs (json:"-") and the frontend wants a
// derived has_private_key flag.
type keyView struct {
	ID              uint   `json:"id"`
	Name            string `json:"name"`
	Label           string `json:"label,omitempty"`
	PublicKey       string `json:"public_key"`
	Fingerprint     string `json:"fingerprint,omitempty"`
	IsDefault       bool   `json:"is_default"`
	OwnerID         *uint  `json:"owner_id,omitempty"`
	Source          string `json:"source,omitempty"`
	HasPrivateKey   bool   `json:"has_private_key"`
	SystemGenerated bool   `json:"system_generated"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func toKeyView(k *db.SSHKey) keyView {
	return keyView{
		ID:              k.ID,
		Name:            k.Name,
		Label:           k.Label,
		PublicKey:       k.PublicKey,
		Fingerprint:     k.Fingerprint,
		IsDefault:       k.IsDefault,
		OwnerID:         k.OwnerID,
		Source:          k.Source,
		HasPrivateKey:   k.HasPrivateKey(),
		SystemGenerated: k.SystemGenerated,
		CreatedAt:       k.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:       k.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
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

// createKeyResponse extends the standard keyView with the freshly-minted
// private key, which is only returned on the generate path. Lifted out of an
// anonymous struct so swag can reference the type in @Success annotations.
type createKeyResponse struct {
	keyView
	PrivateKey string `json:"private_key,omitempty"`
}

// privateKeyResponse is the body of GET /keys/{id}/private-key.
type privateKeyResponse struct {
	KeyName    string `json:"key_name"`
	PrivateKey string `json:"private_key"`
}

// attachPrivateKeyRequest is the body of POST /keys/{id}/private-key.
type attachPrivateKeyRequest struct {
	PrivateKey string `json:"private_key"`
}

// requesterID returns the signed-in user's ID for owner-gating, or writes a
// 401 and returns (0, false) when the request is somehow unauthenticated
// (shouldn't happen behind requireAuth, but cheap to defend).
func requesterID(w http.ResponseWriter, r *http.Request) (uint, bool) {
	user := ctxutil.User(r.Context())
	if user == nil {
		response.Error(w, http.StatusUnauthorized, "Not authenticated")
		return 0, false
	}
	return user.ID, true
}

// Create handles POST /api/keys. On generate, the response includes the
// freshly minted private key (the only time it'll ever cross the wire from
// the server's perspective unless re-downloaded later).
//
// @Summary     Create or import an SSH key
// @Description Either generates a new ed25519 keypair (`generate=true`) or stores
// @Description an imported public/private key. The freshly generated private half
// @Description is returned only on this response — re-download via /keys/{id}/private-key later.
// @Tags        keys
// @Security    cookieAuth
// @Accept      json
// @Produce     json
// @Param       body body     createKeyRequest true "Key creation request"
// @Success     201  {object} EnvelopeOK{data=createKeyResponse}
// @Failure     400  {object} EnvelopeError
// @Failure     401  {object} EnvelopeError
// @Router      /keys [post]
func (h *Keys) Create(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}

	row, err := h.svc.Create(r.Context(), sshkeys.CreateRequest{
		Name:       req.Name,
		Label:      req.Label,
		PublicKey:  req.PublicKey,
		PrivateKey: req.PrivateKey,
		Generate:   req.Generate,
		SetDefault: req.SetDefault,
		OwnerID:    &uid,
	})
	if err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:      "key.create",
			TargetType:  "key",
			TargetLabel: req.Name,
			Details:     map[string]any{"generate": req.Generate, "set_default": req.SetDefault},
			Success:     false,
			ErrorMsg:    err.Error(),
		})
		response.FromError(w, err)
		return
	}
	h.audit.Record(r.Context(), audit.Event{
		Action:      "key.create",
		TargetType:  "key",
		TargetID:    strconv.FormatUint(uint64(row.ID), 10),
		TargetLabel: row.Name,
		Details:     map[string]any{"generate": req.Generate, "set_default": req.SetDefault, "fingerprint": row.Fingerprint},
		Success:     true,
	})

	body := createKeyResponse{keyView: toKeyView(row)}

	// Surface the private key in the response only when we just generated it
	// — for imports the user already has it.
	if req.Generate {
		_, priv, err := h.svc.GetPrivateKey(r.Context(), row.ID, &uid)
		if err == nil {
			body.PrivateKey = priv
		}
	}
	response.Created(w, body)
}

// List handles GET /api/keys — strictly scoped to the caller's own keys.
//
// @Summary     List SSH keys
// @Description Returns the caller's stored SSH keys. Public-key + metadata only;
// @Description the encrypted private half is fetched via GET /keys/{id}/private-key.
// @Tags        keys
// @Security    cookieAuth
// @Produce     json
// @Success     200 {object} EnvelopeOK{data=[]keyView}
// @Failure     401 {object} EnvelopeError
// @Router      /keys [get]
func (h *Keys) List(w http.ResponseWriter, r *http.Request) {
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	rows, err := h.svc.List(r.Context(), &uid)
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
//
// @Summary     Get a single SSH key
// @Tags        keys
// @Security    cookieAuth
// @Produce     json
// @Param       id  path     int true "Key ID"
// @Success     200 {object} EnvelopeOK{data=keyView}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Router      /keys/{id} [get]
func (h *Keys) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	row, err := h.svc.Get(r.Context(), id, &uid)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, toKeyView(row))
}

// PrivateKey handles GET /api/keys/{id}/private-key.
//
// @Summary     Download the private half of an SSH key
// @Description Returns the decrypted private key. Available only to the key owner;
// @Description fails with 404 if no private half is stored.
// @Tags        keys
// @Security    cookieAuth
// @Produce     json
// @Param       id  path     int true "Key ID"
// @Success     200 {object} EnvelopeOK{data=privateKeyResponse}
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Router      /keys/{id}/private-key [get]
func (h *Keys) PrivateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	name, priv, err := h.svc.GetPrivateKey(r.Context(), id, &uid)
	if err != nil {
		response.FromError(w, err)
		return
	}
	response.Success(w, privateKeyResponse{
		KeyName:    name,
		PrivateKey: priv,
	})
}

// AttachPrivateKey handles POST /api/keys/{id}/private-key. Body:
// `{"private_key":"<PEM/OpenSSH>"}`. Used to vault a private half on a
// public-only key after the fact.
//
// @Summary     Attach a private key to a public-only key
// @Description Vaults the private half of a key originally imported as a public
// @Description key only. After this call, has_private_key flips true.
// @Tags        keys
// @Security    cookieAuth
// @Accept      json
// @Param       id   path     int                     true "Key ID"
// @Param       body body     attachPrivateKeyRequest true "Private key payload"
// @Success     204 "No Content"
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Router      /keys/{id}/private-key [post]
func (h *Keys) AttachPrivateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	var req attachPrivateKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.BadRequest(w, "invalid JSON body")
		return
	}
	if req.PrivateKey == "" {
		response.BadRequest(w, "private_key is required")
		return
	}
	if err := h.svc.AttachPrivateKey(r.Context(), id, req.PrivateKey, &uid); err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:     "key.attach_private",
			TargetType: "key",
			TargetID:   strconv.FormatUint(uint64(id), 10),
			Success:    false,
			ErrorMsg:   err.Error(),
		})
		response.FromError(w, err)
		return
	}
	h.audit.Record(r.Context(), audit.Event{
		Action:     "key.attach_private",
		TargetType: "key",
		TargetID:   strconv.FormatUint(uint64(id), 10),
		Success:    true,
	})
	response.NoContent(w)
}

// SetDefault handles POST /api/keys/{id}/default.
//
// @Summary     Mark this key as the caller's default
// @Description Default keys are auto-attached at provision time when the request
// @Description body doesn't specify a key explicitly.
// @Tags        keys
// @Security    cookieAuth
// @Param       id  path     int true "Key ID"
// @Success     204 "No Content"
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Router      /keys/{id}/default [post]
func (h *Keys) SetDefault(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	if err := h.svc.SetDefault(r.Context(), id, &uid); err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:     "key.set_default",
			TargetType: "key",
			TargetID:   strconv.FormatUint(uint64(id), 10),
			Success:    false,
			ErrorMsg:   err.Error(),
		})
		response.FromError(w, err)
		return
	}
	h.audit.Record(r.Context(), audit.Event{
		Action:     "key.set_default",
		TargetType: "key",
		TargetID:   strconv.FormatUint(uint64(id), 10),
		Success:    true,
	})
	response.NoContent(w)
}

// Delete handles DELETE /api/keys/{id}.
//
// @Summary     Delete an SSH key
// @Tags        keys
// @Security    cookieAuth
// @Param       id  path     int true "Key ID"
// @Success     204 "No Content"
// @Failure     400 {object} EnvelopeError
// @Failure     401 {object} EnvelopeError
// @Failure     404 {object} EnvelopeError
// @Router      /keys/{id} [delete]
func (h *Keys) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	uid, ok := requesterID(w, r)
	if !ok {
		return
	}
	// Pre-lookup for hostname-style label so the audit row carries
	// the key's name, not just its DB row id. Best-effort.
	var keyName string
	if row, err := h.svc.Get(r.Context(), id, &uid); err == nil && row != nil {
		keyName = row.Name
	}
	if err := h.svc.Delete(r.Context(), id, &uid); err != nil {
		h.audit.Record(r.Context(), audit.Event{
			Action:      "key.delete",
			TargetType:  "key",
			TargetID:    strconv.FormatUint(uint64(id), 10),
			TargetLabel: keyName,
			Success:     false,
			ErrorMsg:    err.Error(),
		})
		response.FromError(w, err)
		return
	}
	h.audit.Record(r.Context(), audit.Event{
		Action:      "key.delete",
		TargetType:  "key",
		TargetID:    strconv.FormatUint(uint64(id), 10),
		TargetLabel: keyName,
		Success:     true,
	})
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
