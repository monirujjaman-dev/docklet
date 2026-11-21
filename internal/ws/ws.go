// Package ws is a small server-side WebSocket (RFC 6455) implementation:
// enough for the browser terminal, without a dependency.
package ws

import (
	"bufio"
	"crypto/sha1" //nolint:gosec // required by RFC 6455 for the handshake, not for security
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Message types.
const (
	Text   = 1
	Binary = 2
	close_ = 8
	ping   = 9
	pong   = 10
)

// MaxMessage bounds one incoming message.
const MaxMessage = 1 << 20

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Conn is a server-side WebSocket connection.
type Conn struct {
	c  net.Conn
	br *bufio.Reader
	wm sync.Mutex
}

// Upgrade completes the handshake. The caller checks the Origin first.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !headerHas(r.Header, "Connection", "upgrade") || !headerHas(r.Header, "Upgrade", "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, errors.New("ws: not an upgrade request")
	}
	if r.Header.Get("Sec-Websocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, errors.New("ws: bad version")
	}
	key := r.Header.Get("Sec-Websocket-Key")
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return nil, errors.New("ws: missing key")
	}
	c, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})      // clear the server's read timeout
	sum := sha1.Sum([]byte(key + guid)) //nolint:gosec // RFC 6455
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return &Conn{c: c, br: brw.Reader}, nil
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// Read returns the next data message, answering pings on the way.
func (c *Conn) Read() (typ int, data []byte, err error) {
	var msg []byte
	msgType := 0
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case ping:
			if err := c.write(pong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case pong:
			continue
		case close_:
			_ = c.write(close_, payload)
			return 0, nil, io.EOF
		case 0: // continuation
			if msgType == 0 {
				return 0, nil, errors.New("ws: unexpected continuation")
			}
		case Text, Binary:
			if msgType != 0 {
				return 0, nil, errors.New("ws: interleaved message")
			}
			msgType = int(op)
		default:
			return 0, nil, errors.New("ws: unknown opcode")
		}
		if len(msg)+len(payload) > MaxMessage {
			return 0, nil, errors.New("ws: message too large")
		}
		msg = append(msg, payload...)
		if fin {
			return msgType, msg, nil
		}
	}
}

func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0f
	if h[0]&0x70 != 0 {
		return false, 0, nil, errors.New("ws: reserved bits set")
	}
	masked := h[1]&0x80 != 0
	if !masked {
		return false, 0, nil, errors.New("ws: client frames must be masked")
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if op >= 8 && (n > 125 || !fin) {
		return false, 0, nil, errors.New("ws: bad control frame")
	}
	if n > MaxMessage {
		return false, 0, nil, errors.New("ws: frame too large")
	}
	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return fin, op, payload, nil
}

// Write sends one message.
func (c *Conn) Write(typ int, data []byte) error { return c.write(byte(typ), data) }

func (c *Conn) write(op byte, data []byte) error {
	c.wm.Lock()
	defer c.wm.Unlock()
	hdr := make([]byte, 0, 10)
	hdr = append(hdr, 0x80|op)
	switch n := len(data); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	_ = c.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.c.Write(append(hdr, data...)); err != nil {
		return err
	}
	return nil
}

// Ping sends a ping (keeps proxies from closing an idle connection).
func (c *Conn) Ping() error { return c.write(ping, nil) }

// Close sends a close frame with code and closes the connection.
func (c *Conn) Close(code int, reason string) error {
	b := binary.BigEndian.AppendUint16(nil, uint16(code)) //nolint:gosec // close codes fit
	if len(reason) > 120 {
		reason = reason[:120]
	}
	_ = c.write(close_, append(b, reason...))
	return c.c.Close()
}
