package ws

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func clientFrame(op byte, payload []byte) []byte {
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	b := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		b = append(b, 0x80|byte(n))
	case n <= 0xffff:
		b = binary.BigEndian.AppendUint16(append(b, 0x80|126), uint16(n)) //nolint:gosec // n <= 0xffff
	default:
		b = append(b, 0x80|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	b = append(b, mask[:]...)
	for i, c := range payload {
		b = append(b, c^mask[i%4])
	}
	return b
}

func readServerFrame(t *testing.T, br *bufio.Reader) (byte, []byte) {
	t.Helper()
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		t.Fatal(err)
	}
	n := int(h[1] & 0x7f)
	switch n {
	case 126:
		var x [2]byte
		_, _ = io.ReadFull(br, x[:])
		n = int(binary.BigEndian.Uint16(x[:]))
	case 127:
		var x [8]byte
		_, _ = io.ReadFull(br, x[:])
		n = int(binary.BigEndian.Uint64(x[:])) //nolint:gosec // test frames are small
	}
	p := make([]byte, n)
	_, _ = io.ReadFull(br, p)
	return h[0] & 0x0f, p
}

func TestEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r)
		if err != nil {
			return
		}
		for {
			typ, data, err := c.Read()
			if err != nil {
				_ = c.Close(1000, "bye")
				return
			}
			_ = c.Write(typ, append([]byte("echo:"), data...))
		}
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil) //nolint:bodyclose // upgraded connection
	if err != nil || res.StatusCode != 101 || res.Header.Get("Sec-Websocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("handshake: %v %v %v", res.StatusCode, res.Header, err)
	}
	// A fragmented text message, a ping in between, then a large binary one.
	frag1 := clientFrame(Text, []byte("hel"))
	frag1[0] = 0x01 // not final
	_, _ = conn.Write(frag1)
	_, _ = conn.Write(clientFrame(ping, []byte("p")))
	_, _ = conn.Write(clientFrame(0, []byte("lo")))
	if op, p := readServerFrame(t, br); op != pong || string(p) != "p" {
		t.Fatalf("pong = %d %q", op, p)
	}
	if op, p := readServerFrame(t, br); op != Text || string(p) != "echo:hello" {
		t.Fatalf("text = %d %q", op, p)
	}
	big := make([]byte, 70000)
	_, _ = conn.Write(clientFrame(Binary, big))
	if op, p := readServerFrame(t, br); op != Binary || len(p) != 70005 {
		t.Fatalf("binary = %d %d", op, len(p))
	}
	_, _ = conn.Write(clientFrame(close_, []byte{0x03, 0xe8}))
	if op, _ := readServerFrame(t, br); op != close_ {
		t.Fatalf("close = %d", op)
	}
}
