// Package console relays a browser WebSocket to a Proxmox VM's
// noVNC/RFB stream so the SPA can render the graphical console in the
// browser without exposing Proxmox to the public internet.
//
// Why noVNC and not xterm.js: PVE 9.x rejects API token auth at
// termproxy's in-band handshake. vncproxy bypasses that — the RFB
// handshake is end-to-end between the browser's noVNC client and
// PVE's VNC server, so Nimbus stays a pure byte pump and the existing
// API token works.
//
// The SPA does two requests:
//  1. POST /api/vms/{id}/console/session — Nimbus calls VNCProxy and
//     returns {ticket, port} to the browser.
//  2. GET  /api/vms/{id}/console/ws?port=X&vncticket=Y — Nimbus opens
//     the upstream WS to PVE with the supplied port+ticket and pumps
//     bytes both ways. The browser's noVNC uses the same ticket as
//     the VNC password during the RFB auth phase.
package console

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"nimbus/internal/proxmox"
)

// upgrader accepts the browser-side WebSocket. CheckOrigin returns true:
// the wrapping handler enforces auth (session cookie + VM ownership),
// so origin gating doesn't add security and would block legitimate
// reverse-proxied deployments. Subprotocol "binary" matches what PVE
// expects on the upstream so frames pass through unchanged.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
	Subprotocols:    []string{"binary"},
}

// proxmoxClient is the slice of *proxmox.Client this package needs.
type proxmoxClient interface {
	VNCProxy(ctx context.Context, node string, vmid int) (*proxmox.VNCProxyTicket, error)
	DialConsoleWS(ctx context.Context, node string, vmid, port int, ticket string) (*websocket.Conn, error)
}

// Relay opens and proxies noVNC sessions.
type Relay struct {
	px proxmoxClient
}

// New constructs a Relay. The relay holds no DB or auth state — those
// are the handler's job.
func New(px proxmoxClient) *Relay {
	return &Relay{px: px}
}

// IssueSession calls VNCProxy on the PVE node and returns the ticket
// + port the SPA needs. Owner / auth checks happen in the handler.
func (r *Relay) IssueSession(ctx context.Context, node string, vmid int) (*proxmox.VNCProxyTicket, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.px.VNCProxy(ctx, node, vmid)
}

// Stream upgrades the inbound HTTP request to a WebSocket and proxies
// it to PVE's vncwebsocket bridge with the supplied port + ticket.
// Blocks until either side closes.
//
// Pre-upgrade errors (upstream dial failed) write an HTTP error; after
// upgrade, errors close the browser side with a reason that the SPA's
// onclose handler can render.
//
// Nimbus is a transparent byte pump after the upgrade. The RFB
// protocol — version negotiation, security types, VNC Auth challenge —
// flows end-to-end between the browser's noVNC client and PVE's VNC
// server. Nimbus does not parse or modify any frames.
func (r *Relay) Stream(ctx context.Context, w http.ResponseWriter, req *http.Request, node string, vmid, port int, ticket string) {
	// Upgrade the browser side first so a failed upstream dial can
	// surface via a close frame the SPA can render (HTTP errors after
	// the upgrade headers are sent get lost). The browser's noVNC
	// will tolerate the immediate close.
	browser, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		// Upgrader already wrote an HTTP error.
		return
	}
	defer browser.Close() //nolint:errcheck

	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	upstream, err := r.px.DialConsoleWS(dialCtx, node, vmid, port, ticket)
	dialCancel()
	if err != nil {
		writeBrowserClose(browser, websocket.CloseInternalServerErr,
			"upstream dial: "+err.Error())
		return
	}
	defer upstream.Close() //nolint:errcheck

	// Bidirectional pump. Each direction in its own goroutine; first
	// error closes both conns.
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = browser.Close()
			_ = upstream.Close()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer closeBoth()
		pump(browser, upstream, "browser->pve")
	}()
	go func() {
		defer wg.Done()
		defer closeBoth()
		pump(upstream, browser, "pve->browser")
	}()
	wg.Wait()
}

// pump forwards WS frames src → dst until either side errors. Normal
// close codes are silent; unexpected closes get logged.
func pump(src, dst *websocket.Conn, label string) {
	for {
		mt, data, err := src.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived,
			) {
				log.Printf("console relay: %s read: %v", label, err)
			}
			return
		}
		if err := dst.WriteMessage(mt, data); err != nil {
			if !websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
			) {
				log.Printf("console relay: %s write: %v", label, err)
			}
			return
		}
	}
}

// writeBrowserClose sends a close frame so the SPA's onclose can
// render a useful reason. 123-byte reason cap is the WS spec.
func writeBrowserClose(c *websocket.Conn, code int, reason string) {
	if len(reason) > 123 {
		reason = reason[:123]
	}
	msg := websocket.FormatCloseMessage(code, reason)
	_ = c.WriteControl(websocket.CloseMessage, msg, time.Now().Add(2*time.Second))
	log.Printf("console relay: closing browser ws: %s", reason)
}
