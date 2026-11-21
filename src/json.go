package main

// json.go — JSON value model shared by the REST, GraphQL and MCP layers.
// Values are the encoding/json generic shapes with UseNumber, so integers
// round-trip exactly the way Python's json module kept them.

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// JObj is a JSON object.
type JObj = map[string]interface{}

// pyErr is raised wherever the Python original would have thrown (and its
// route wrapper would have answered 500).
type pyErr struct{ msg string }

func (e pyErr) Error() string { return e.msg }

// parseJSON decodes a request body the way json.loads would: any JSON value,
// or an error.
func parseJSON(b []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// bodyObj mirrors the REST layer's body() helper: nil when the body does not
// parse, the object when it does, and a pyErr when it parses to a non-object
// (the Python code would fail on .get and answer 500).
func (rq *request) bodyObj() JObj {
	raw := rq.bodyBytes()
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	v, err := parseJSON(raw)
	if err != nil {
		return nil
	}
	if v == nil {
		// json.loads('null') is None, which the Python handlers treat the
		// same as an unparseable body.
		return nil
	}
	obj, ok := v.(JObj)
	if !ok {
		panic(pyErr{"body is not an object"})
	}
	return obj
}

// strOf extracts a string the way Python code paths expect: ok=false when
// the value is not a JSON string.
func strOf(v interface{}) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// pyTruthy is Python truthiness for JSON values.
func pyTruthy(v interface{}) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return len(t) > 0
	case json.Number:
		f, _ := t.Float64()
		return f != 0
	case float64:
		return t != 0
	case int:
		return t != 0
	case []interface{}:
		return len(t) > 0
	case JObj:
		return len(t) > 0
	}
	return true
}

// intOf mirrors Python int(value) for the shapes the forge feeds it: JSON
// numbers and numeric strings.
func intOf(v interface{}) (int, bool) {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i), true
		}
		if f, err := t.Float64(); err == nil {
			return int(f), true
		}
	case float64:
		return int(t), true
	case int:
		return t, true
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return i, true
		}
	}
	return 0, false
}

// getStr is d.get(key, def) restricted to strings: a present non-string
// value behaves the way the Python code would when it then treats it as one
// (the caller's next string operation would fail), so we raise pyErr.
func getStr(d JObj, key, def string) string {
	v, ok := d[key]
	if !ok || v == nil {
		return def
	}
	s, ok := v.(string)
	if !ok {
		panic(pyErr{"expected string for " + key})
	}
	return s
}

// marshalJSON is the compact encoding the Python send() produced with
// separators=(',',':') — encoding/json's default compact form, except that
// Python does not escape <, > and &; the suite parses JSON, so the escape
// difference is inert, and HTML escaping is disabled here to stay closest.
func marshalJSON(v interface{}) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}
