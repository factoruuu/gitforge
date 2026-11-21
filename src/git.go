package main

// git.go — git smart HTTP: info/refs advertisement (v0 and protocol v2),
// ls-refs, fetch (v2, with shallow/deepen support) and v0 upload-pack, and
// receive-pack v0 with report-status. A faithful port of the Python
// implementation's wire behaviour, including its pack cache and its
// readpack resolution passes (ref-deltas resolve against stored objects;
// ofs-delta entries carry an "offset:" base that never resolves, exactly as
// in the Python original).

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func pkt(data []byte) []byte {
	return []byte(fmt.Sprintf("%04x", len(data)+4) + string(data))
}

func pktStr(s string) []byte { return pkt([]byte(s)) }

func flushPkt() []byte { return []byte("0000") }

// unpkt mirrors the Python unpkt: returns the packet payloads (nil entries
// for flush packets, special markers for lengths 1..3) and the number of
// bytes consumed. Lengths 1..3 do not consume payload bytes, as in Python.
type pktItem struct {
	data    []byte
	flush   bool
	special int
}

func unpkt(b []byte) ([]pktItem, int) {
	var out []pktItem
	i := 0
	for i+4 <= len(b) {
		n, err := strconv.ParseUint(string(b[i:i+4]), 16, 32)
		if err != nil {
			break
		}
		i += 4
		if n == 0 {
			out = append(out, pktItem{flush: true})
			continue
		}
		if n >= 1 && n <= 3 {
			out = append(out, pktItem{special: int(n)})
			continue
		}
		if n < 4 || i+int(n)-4 > len(b) {
			break
		}
		out = append(out, pktItem{data: b[i : i+int(n)-4]})
		i += int(n) - 4
	}
	return out, i
}

func applyDelta(src, d []byte) []byte {
	i := 0
	readVar := func() int {
		n, s := 0, 0
		for {
			c := d[i]
			i++
			n |= int(c&127) << s
			s += 7
			if c < 128 {
				return n
			}
		}
	}
	_ = readVar() // source size
	_ = readVar() // result size
	var out []byte
	for i < len(d) {
		op := d[i]
		i++
		if op&128 != 0 {
			off, ln := 0, 0
			for _, bs := range [][2]int{{1, 0}, {2, 8}, {4, 16}, {8, 24}} {
				if op&byte(bs[0]) != 0 {
					off |= int(d[i]) << bs[1]
					i++
				}
			}
			for _, bs := range [][2]int{{16, 0}, {32, 8}, {64, 16}} {
				if op&byte(bs[0]) != 0 {
					ln |= int(d[i]) << bs[1]
					i++
				}
			}
			if ln == 0 {
				ln = 65536
			}
			// Python slicing clamps out-of-range bounds instead of failing.
			lo := off
			if lo > len(src) {
				lo = len(src)
			}
			hi := off + ln
			if hi > len(src) {
				hi = len(src)
			}
			if hi < lo {
				hi = lo
			}
			out = append(out, src[lo:hi]...)
		} else if op != 0 {
			end := i + int(op)
			if end > len(d) {
				end = len(d)
			}
			out = append(out, d[i:end]...)
			i += int(op)
		} else {
			break
		}
	}
	return out
}

type pendingObj struct {
	typ  string // object type name, or "delta"
	data []byte
	base string // "" for plain objects; "offset:<n>" or a hex oid for deltas
	hasB bool
}

func readpack(data []byte, repo *Repo) {
	if len(data) < 32 || !bytes.Equal(data[:4], []byte("PACK")) {
		return
	}
	b := data[:len(data)-20]
	count := int(binary.BigEndian.Uint32(b[8:12]))
	pos := 12
	var pending []pendingObj
	for k := 0; k < count; k++ {
		c := b[pos]
		pos++
		typ := (c >> 4) & 7
		size := int(c & 15)
		shift := 4
		for c&128 != 0 {
			c = b[pos]
			pos++
			size |= int(c&127) << shift
			shift += 7
		}
		_ = size
		base := ""
		hasB := false
		if typ == 6 {
			c = b[pos]
			pos++
			dist := int(c & 127)
			for c&128 != 0 {
				c = b[pos]
				pos++
				dist = ((dist + 1) << 7) | int(c&127)
			}
			base = "offset:" + strconv.Itoa(pos-dist)
			hasB = true
		} else if typ == 7 {
			base = hex.EncodeToString(b[pos : pos+20])
			pos += 20
			hasB = true
		}
		// Decompress one object and advance pos past its zlib stream, the
		// equivalent of Python's decompressobj + unused_data accounting.
		// bytes.Reader implements io.ByteReader, so the flate decoder reads
		// exactly the stream's bytes (no buffered over-read) and the
		// remainder length gives the consumed count precisely.
		br := bytes.NewReader(b[pos:])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return
		}
		content, err := io.ReadAll(zr)
		if err != nil {
			return
		}
		zr.Close()
		pos += (len(b) - pos) - br.Len()
		typname := map[byte]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}[typ]
		if typname != "" {
			pending = append(pending, pendingObj{typ: typname, data: content})
		} else {
			pending = append(pending, pendingObj{typ: "delta", data: content, base: base, hasB: hasB})
		}
	}
	for round := 0; round < count+1; round++ {
		var rest []pendingObj
		for _, p := range pending {
			if p.typ != "delta" {
				repo.put(p.typ, p.data)
				continue
			}
			var oid string
			have := false
			if p.hasB && !strings.HasPrefix(p.base, "offset:") {
				oid = p.base
				have = true
			}
			if !have {
				rest = append(rest, p)
				continue
			}
			baseObj, ok := repo.get(oid)
			if !ok {
				rest = append(rest, p)
				continue
			}
			repo.put(baseObj.typ, applyDelta(baseObj.data, p.data))
		}
		pending = rest
		if len(pending) == 0 {
			break
		}
	}
}

var (
	treeRe      = regexp.MustCompile(`(?m)^tree ([0-9a-f]{40})$`)
	tagObjRe    = regexp.MustCompile(`(?m)^object ([0-9a-f]{40})$`)
	committerRe = regexp.MustCompile(`(?m)^committer .* (\d+) [+-]\d{4}$`)
)

// makepack builds a packfile for the closure of tips, honouring shallow
// depth/since/stop exclusions. Mirrors the Python makepack, including its
// result cache and the _shallow_boundaries side effect.
func makepack(repo *Repo, tips []string, depth *int, exclude []string, shallow []string, deepen bool, since *int, stopCommits []string) []byte {
	key := strings.Join(tips, "\x00") + "|" + intKey(depth) + "|" +
		strings.Join(sortedCopy(exclude), "\x00") + "|" + strings.Join(sortedCopy(shallow), "\x00") + "|" +
		boolKey(deepen) + "|" + intKey(since) + "|" + strings.Join(sortedCopy(stopCommits), "\x00")
	if ent, ok := repo.packCache[key]; ok {
		if depth != nil || since != nil || len(stopCommits) > 0 {
			repo.shallowBnd = append([]string{}, ent.boundaries...)
		}
		return ent.pack
	}
	excluded := strSet(exclude)
	oldShallow := strSet(shallow)
	stopped := strSet(stopCommits)
	levels := map[string]int{}
	var levelOrder []string
	var extraRoots []string
	type item struct {
		oid   string
		level int
	}
	todo := []item{}
	for _, t := range tips {
		todo = append(todo, item{t, 0})
	}
	for len(todo) > 0 {
		it := todo[0]
		todo = todo[1:]
		oid, level := it.oid, it.level
		if _, done := levels[oid]; done {
			continue
		}
		if excluded[oid] && !oldShallow[oid] {
			continue
		}
		obj, ok := repo.get(oid)
		if !ok {
			continue
		}
		if obj.typ == "tag" {
			extraRoots = append(extraRoots, oid)
			continue
		}
		if obj.typ != "commit" {
			continue
		}
		if stopped[oid] {
			continue
		}
		if since != nil {
			if m := committerRe.FindSubmatch(obj.data); m != nil {
				if ts, _ := strconv.Atoi(string(m[1])); ts < *since {
					continue
				}
			}
		}
		levels[oid] = level
		levelOrder = append(levelOrder, oid)
		parents := repo.parents(oid)
		if depth == nil || level+1 < *depth || (deepen && oldShallow[oid]) {
			for _, p := range parents {
				todo = append(todo, item{p, level + 1})
			}
		}
	}
	objs := map[string]gitObject{}
	var objOrder []string
	addObj := func(oid string, o gitObject) {
		if _, dup := objs[oid]; !dup {
			objs[oid] = o
			objOrder = append(objOrder, oid)
		}
	}
	var todo2 []string
	for _, oid := range levelOrder {
		obj, _ := repo.get(oid)
		if m := treeRe.FindSubmatch(obj.data); m != nil {
			todo2 = append(todo2, string(m[1]))
		}
		if !excluded[oid] {
			addObj(oid, obj)
		}
	}
	todo2 = append(todo2, extraRoots...)
	filtered := todo2[:0]
	for _, x := range todo2 {
		if _, inLevels := levels[x]; !inLevels && !excluded[x] {
			filtered = append(filtered, x)
		}
	}
	todo2 = filtered
	seen := map[string]bool{}
	for oid := range objs {
		seen[oid] = true
	}
	for len(todo2) > 0 {
		oid := todo2[len(todo2)-1]
		todo2 = todo2[:len(todo2)-1]
		if seen[oid] || (excluded[oid] && !oldShallow[oid]) {
			continue
		}
		seen[oid] = true
		obj, ok := repo.get(oid)
		if !ok {
			continue
		}
		addObj(oid, obj)
		if obj.typ == "tree" {
			raw := obj.data
			i := 0
			for i < len(raw) {
				j := indexByte(raw[i:], 0)
				if j < 0 {
					break
				}
				j += i
				child := hex.EncodeToString(raw[j+1 : j+21])
				i = j + 21
				if !seen[child] {
					todo2 = append(todo2, child)
				}
			}
		} else if obj.typ == "tag" {
			if m := tagObjRe.FindSubmatch(obj.data); m != nil {
				todo2 = append(todo2, string(m[1]))
			}
		}
	}
	var boundaries []string
	if depth != nil || since != nil || len(stopped) > 0 {
		for _, oid := range levelOrder {
			for _, p := range repo.parents(oid) {
				if _, in := levels[p]; !in {
					boundaries = append(boundaries, oid)
					break
				}
			}
		}
		repo.shallowBnd = boundaries
	} else {
		repo.shallowBnd = nil
	}
	var out bytes.Buffer
	out.WriteString("PACK")
	writeU32(&out, 2)
	writeU32(&out, uint32(len(objOrder)))
	typnum := map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
	for _, oid := range objOrder {
		obj := objs[oid]
		n := len(obj.data)
		c := typnum[obj.typ]<<4 | byte(n&15)
		n >>= 4
		hdr := []byte{c}
		for n > 0 {
			hdr[len(hdr)-1] |= 128
			hdr = append(hdr, byte(n&127))
			if n>>7 != 0 {
				hdr[len(hdr)-1] |= 128
			}
			n >>= 7
		}
		out.Write(hdr)
		var zb bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&zb, zlib.DefaultCompression)
		zw.Write(obj.data)
		zw.Close()
		out.Write(zb.Bytes())
	}
	sum := sha1.Sum(out.Bytes())
	out.Write(sum[:])
	pack := out.Bytes()
	if len(repo.packCache) >= 32 {
		old := repo.packOrder[0]
		repo.packOrder = repo.packOrder[1:]
		delete(repo.packCache, old)
	}
	bnd := []string{}
	if depth != nil || since != nil || len(stopCommits) > 0 {
		bnd = append(bnd, repo.shallowBnd...)
	}
	repo.packCache[key] = packCacheEntry{pack: pack, boundaries: bnd}
	repo.packOrder = append(repo.packOrder, key)
	return pack
}

func writeU32(b *bytes.Buffer, v uint32) {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	b.Write(tmp[:])
}

func intKey(p *int) string {
	if p == nil {
		return "-"
	}
	return strconv.Itoa(*p)
}

func boolKey(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func strSet(in []string) map[string]bool {
	out := map[string]bool{}
	for _, x := range in {
		out[x] = true
	}
	return out
}
