package handlers

// EnvelopeOK is the wire shape of a successful API response. Swaggo references
// it via composition — `EnvelopeOK{data=keyView}` in @Success annotations
// produces a typed schema in the OpenAPI spec without needing one wrapper
// struct per data shape.
//
// The Data field is interface{} only because swag rewrites it during
// composition; the runtime envelope is response.Response, not this type.
type EnvelopeOK struct {
	Success bool        `json:"success" example:"true"`
	Data    interface{} `json:"data"`
}

// EnvelopeError is the wire shape of a failed API response.
type EnvelopeError struct {
	Success bool   `json:"success" example:"false"`
	Error   string `json:"error" example:"invalid id"`
}
