package main

// main.go — the HTTP/1.1 server and request routing: a port of the Python
// BaseHTTPRequestHandler-based front end, including its lenient request-line
// and header parsing, its keep-alive rules, the /api/v3 GET response cache,
// the upload-pack response cache's sibling semantics, Bearer auth for /api/*
// and /mcp, and the MAKEFUN_ENV_URL register + /done polling lifecycle.
// A single mutex serializes request handling, mirroring the GIL.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	routeMu  sync.Mutex
	getCache = map[string]cacheEntry{}
	authOnce sync.Once
	authTok  string
)

type cacheEntry struct {
	ctype   string
	body    []byte
	headers [][2]string
}

func authToken() string {
	authOnce.Do(func() {
		tok := os.Getenv("MAKEFUN_SHOP_TOKEN")
		if tok == "" {
			tok = "dev-token"
		}
		authTok = "Bearer " + tok
	})
	return authTok
}

type request struct {
	method    string
	version   string
	rawTarget string
	headers   map[string]string
	query     map[string][]string
	conn      net.Conn
	reader    *bufio.Reader
	bodyCache []byte
	bodyRead  bool
	close     bool
}

func (rq *request) bodyBytes() []byte {
	if rq.bodyRead {
		return rq.bodyCache
	}
	rq.bodyRead = true
	cl, present := rq.headers["content-length"]
	if !present {
		rq.bodyCache = nil
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(cl))
	if err != nil {
		panic(pyErr{"invalid literal for int() with base 10: '" + cl + "'"})
	}
	if n < 0 {
		b, _ := io.ReadAll(rq.reader)
		rq.bodyCache = b
		return b
	}
	if n > 100_000_000 {
		n = 100_000_000
	}
	buf := make([]byte, n)
	got, _ := io.ReadFull(rq.reader, buf)
	rq.bodyCache = buf[:got]
	return rq.bodyCache
}

var reasonPhrases = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict",
	422: "Unprocessable Entity", 500: "Internal Server Error",
}

// send writes one response in a single socket write, exactly like the
// Python send(): status line, Content-Type, Content-Length,
// X-GitHub-Request-Id (hex of the nanosecond clock), any extra headers,
// then the body. A nil body means an empty body.
func (rq *request) send(status int, ctype string, body []byte, headers [][2]string) {
	reason := reasonPhrases[status]
	var head bytes.Buffer
	fmt.Fprintf(&head, "HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nX-GitHub-Request-Id: %x\r\n",
		status, reason, ctype, len(body), time.Now().UnixNano())
	for _, h := range headers {
		head.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	head.WriteString("\r\n")
	out := append(head.Bytes(), body...)
	_, _ = rq.conn.Write(out)
	// The GET cache store, mirroring the tail of the Python send().
	if rq.method == "GET" && status == 200 && strings.HasPrefix(rq.rawTarget, "/api/v3/") &&
		len(body) < 200000 {
		if _, cached := getCache[rq.rawTarget]; !cached {
			if len(getCache) >= 512 {
				getCache = map[string]cacheEntry{}
			}
			hcopy := append([][2]string{}, headers...)
			getCache[rq.rawTarget] = cacheEntry{ctype: ctype, body: append([]byte{}, body...), headers: hcopy}
		}
	}
}

func (rq *request) sendJSON(status int, v interface{}) {
	if v == nil {
		rq.send(status, "application/json", nil, nil)
		return
	}
	rq.send(status, "application/json", marshalJSON(v), nil)
}

func (rq *request) sendJSONH(status int, v interface{}, headers [][2]string) {
	if v == nil {
		rq.send(status, "application/json", nil, headers)
		return
	}
	rq.send(status, "application/json", marshalJSON(v), headers)
}

func (rq *request) sendRaw(status int, ctype string, body []byte, headers [][2]string) {
	rq.send(status, ctype, body, headers)
}

// parseQuery mirrors urllib.parse.parse_qs with default options: blank
// values are dropped, as are bare keys.
func parseQuery(qs string) map[string][]string {
	out := map[string][]string{}
	if qs == "" {
		return out
	}
	vals, _ := url.ParseQuery(qs)
	for k, vs := range vals {
		var kept []string
		for _, v := range vs {
			if v != "" {
				kept = append(kept, v)
			}
		}
		if len(kept) > 0 {
			out[k] = kept
		}
	}
	return out
}

var gitPathRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

func (rq *request) route() {
	defer func() {
		if rec := recover(); rec != nil {
			msg := fmt.Sprint(rec)
			if pe, ok := rec.(pyErr); ok {
				msg = pe.msg
			}
			if len(msg) > 300 {
				msg = msg[:300]
			}
			func() {
				defer func() { _ = recover() }()
				rq.sendJSON(500, JObj{"message": "Internal Server Error", "error": msg})
			}()
		}
	}()
	if rq.method != "GET" {
		getCache = map[string]cacheEntry{}
	}
	if rq.method == "POST" || rq.method == "PUT" || rq.method == "PATCH" || rq.method == "DELETE" {
		rq.bodyBytes()
	}
	rawPath := rq.rawTarget
	queryStr := ""
	hasQuery := false
	if i := strings.IndexByte(rq.rawTarget, '?'); i >= 0 {
		rawPath = rq.rawTarget[:i]
		queryStr = rq.rawTarget[i+1:]
		hasQuery = true
	}
	path := rawPath
	if decoded, err := url.PathUnescape(rawPath); err == nil {
		path = decoded
	}
	var q map[string][]string
	if hasQuery {
		q = parseQuery(queryStr)
	} else {
		q = map[string][]string{}
	}
	rq.query = q
	if path == "/healthz" {
		rq.sendJSON(200, JObj{"ok": true})
		return
	}
	if strings.HasPrefix(path, "/api/") || path == "/mcp" {
		if rq.headers["authorization"] != authToken() {
			rq.bodyBytes()
			rq.sendJSON(401, JObj{"message": "Bad credentials"})
			return
		}
	}
	if rq.method == "GET" && strings.HasPrefix(path, "/api/v3/") {
		if hit, ok := getCache[rq.rawTarget]; ok {
			rq.send(200, hit.ctype, hit.body, hit.headers)
			return
		}
	}
	if path == "/mcp" {
		rq.mcp()
		return
	}
	if path == "/api/graphql" {
		rq.graphql()
		return
	}
	if strings.HasPrefix(path, "/api/v3/") {
		rq.rest(path[len("/api/v3"):], q)
		return
	}
	if m := gitPathRe.FindStringSubmatch(path); m != nil {
		rq.git(m[1], m[2], m[3])
		return
	}
	rq.sendJSON(404, JObj{"message": "Not Found"})
}

func handleConn(conn net.Conn) {
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	reader := bufio.NewReader(conn)
	for {
		rq, ok := readRequest(conn, reader)
		if !ok {
			return
		}
		routeMu.Lock()
		rq.route()
		routeMu.Unlock()
		if rq.close {
			return
		}
	}
}

// readRequest parses one request the way the Python parse_request override
// does: whitespace-split request line, colon-split headers (lines without a
// colon are ignored), and its keep-alive rules.
func readRequest(conn net.Conn, reader *bufio.Reader) (*request, bool) {
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return nil, false
	}
	words := strings.Fields(line)
	rq := &request{headers: map[string]string{}, conn: conn, reader: reader}
	switch len(words) {
	case 3:
		rq.method, rq.rawTarget, rq.version = words[0], words[1], words[2]
		if !strings.HasPrefix(rq.version, "HTTP/") {
			sendErrorPage(conn, 400, "Bad Request")
			return nil, false
		}
	case 2:
		rq.method, rq.rawTarget = words[0], words[1]
		rq.version = "HTTP/0.9"
	default:
		sendErrorPage(conn, 400, "Bad Request")
		return nil, false
	}
	for {
		hl, err := reader.ReadString('\n')
		if hl == "\r\n" || hl == "\n" || (err != nil && len(hl) == 0) {
			break
		}
		if i := strings.IndexByte(hl, ':'); i >= 0 {
			k := strings.ToLower(strings.TrimSpace(hl[:i]))
			v := strings.TrimSpace(hl[i+1:])
			rq.headers[k] = v
		}
		if err != nil {
			break
		}
	}
	connHeader := strings.ToLower(rq.headers["connection"])
	switch {
	case rq.version >= "HTTP/1.1":
		rq.close = connHeader == "close"
	case rq.version == "HTTP/1.0":
		rq.close = connHeader != "keep-alive"
	default:
		rq.close = true
	}
	if strings.ToLower(rq.headers["expect"]) == "100-continue" {
		_, _ = conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
	}
	return rq, true
}

func sendErrorPage(conn net.Conn, status int, reason string) {
	body := "<html><head><title>" + strconv.Itoa(status) + " " + reason + "</title></head><body><h1>" + reason + "</h1></body></html>"
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: text/html;charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, reason, len(body), body)
}

func main() {
	envURL := os.Getenv("MAKEFUN_ENV_URL")
	port := 0
	if envURL == "" {
		port = 8080
		if p := os.Getenv("PORT"); p != "" {
			if v, err := strconv.Atoi(p); err == nil {
				port = v
			}
		}
	}
	ln, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	if envURL != "" {
		actual := ln.Addr().(*net.TCPAddr).Port
		if err := register(envURL, actual); err != nil {
			fmt.Fprintln(os.Stderr, "register:", err)
			os.Exit(1)
		}
		go pollDone(envURL)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleConn(conn)
	}
}

func register(envURL string, port int) error {
	payload, _ := json.Marshal(map[string]int{"port": port})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(strings.TrimRight(envURL, "/")+"/register", "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func pollDone(envURL string) {
	client := &http.Client{Timeout: 2 * time.Second}
	url := strings.TrimRight(envURL, "/") + "/done"
	for {
		func() {
			resp, err := client.Get(url)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var v map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&v); err == nil {
				if done, _ := v["done"].(bool); done {
					os.Exit(0)
				}
			}
		}()
		time.Sleep(250 * time.Millisecond)
	}
}
