// Package console relays a browser WebSocket to a Proxmox VM's serial-
// console WebSocket. The browser opens an xterm.js terminal at
// /vms/{id}/console; the page connects WS to /api/vms/{id}/console/ws;
// this package upgrades that, calls Proxmox termproxy for a per-session
// ticket + port, opens the upstream WS to vncwebsocket, completes the
// PVE auth handshake, and pumps bytes both ways until either side closes.
//
// Authentication is split: the browser → Nimbus hop is gated by Nimbus's
// session cookie + per-VM ownership check (handler-side); the Nimbus →
// Proxmox hop is gated by the API token. The user logs into the VM
// itself at the serial-console getty prompt using the one-time noVNC
// console password Nimbus generated at provision time.
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
// the wrapping handler enforces auth (session cookie + ownership), so
// origin gating doesn't add security here and would block legitimate
// reverse-proxied deployments.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
	Subprotocols:    []string{"binary"},
}

// proxmoxClient is the slice of *proxmox.Client this package needs.
// Defined at the consumer per the codebase's "accept interfaces" idiom
// so tests can stub without standing up the whole PVE surface.
type proxmoxClient interface {
	TermProxy(ctx context.Context, node string, vmid int) (*proxmox.TermProxyTicket, error)
	DialConsoleWS(ctx context.Context, node string, vmid, port int, ticket string) (*websocket.Conn, error)
}

// Relay opens and proxies serial-console sessions.
type Relay struct {
	px proxmoxClient
}

// New constructs a Relay. Caller passes the live *proxmox.Client; the
// relay never holds DB or auth state — those are the handler's job.
func New(px proxmoxClient) *Relay {
	return &Relay{px: px}
}

// Stream upgrades the inbound HTTP request to a WebSocket and proxies it
// to the VM's serial console. Blocks until either side disconnects.
//
// Errors before the upgrade are written as HTTP responses; errors after
// the upgrade are surfaced to the browser via a close frame and logged.
// The handler is responsible for any auth / ownership checks before
// calling Stream.
func (r *Relay) Stream(ctx context.Context, w http.ResponseWriter, req *http.Request, node string, vmid int) {
	// Get the per-session ticket BEFORE the upgrade so a Proxmox-side
	// failure (VM not running, missing permission, agent disabled) can
	// surface as a normal HTTP error to the browser's onerror handler
	// instead of an opaque WS close.
	ticketCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ticket, err := r.px.TermProxy(ticketCtx, node, vmid)
	cancel()
	if err != nil {
		http.Error(w, "open console session: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Upgrade the browser-side WS. After this point, errors must go
	// through the WS, not the HTTP response — chi has already started
	// streaming the upgrade handshake.
	browser, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		// Upgrader already wrote an HTTP error; nothing else to do.
		return
	}
	defer browser.Close() //nolint:errcheck

	// Open the upstream WS to PVE.
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	upstream, err := r.px.DialConsoleWS(dialCtx, node, vmid, ticket.Port, ticket.Ticket)
	dialCancel()
	if err != nil {
		writeBrowserClose(browser, websocket.CloseInternalServerErr,
			"upstream dial: "+err.Error())
		return
	}
	defer upstream.Close() //nolint:errcheck

	// PVE's serial-console handshake: text frame "<user>:<ticket>\n",
	// expect "OK" back. Without this the upstream WS is open but no
	// bytes flow.
	if err := proxmox.ConsoleAuthHandshake(upstream, ticket.User, ticket.Ticket); err != nil {
		writeBrowserClose(browser, websocket.CloseInternalServerErr,
			"upstream auth: "+err.Error())
		return
	}

	// Bidirectional pump. Each direction runs in its own goroutine; the
	// first one to error closes both conns and signals the other side
	// via the wait group.
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

// pump reads frames from src and writes them to dst until either side
// errors. Logs unexpected errors (not normal close) at the package log.
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

// writeBrowserClose sends a close frame to the browser-side WS so the
// xterm page's onclose can render a useful message. Best-effort.
// 123-byte reason cap is the WebSocket spec.
func writeBrowserClose(c *websocket.Conn, code int, reason string) {
	if len(reason) > 123 {
		reason = reason[:123]
	}
	msg := websocket.FormatCloseMessage(code, reason)
	_ = c.WriteControl(websocket.CloseMessage, msg, time.Now().Add(2*time.Second))
	log.Printf("console relay: closing browser ws: %s", reason)
}
