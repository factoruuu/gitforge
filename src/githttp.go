package main

// githttp.go — the git smart-HTTP endpoint: info/refs advertisement,
// protocol-v2 ls-refs and fetch, v0 upload-pack, and receive-pack. Ports
// H.git from the Python server, response caches included.

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

var peeledRe = regexp.MustCompile(`object ([0-9a-f]{40})`)

// upCache is guarded by the global store lock, like the Python GIL guarded
// the module-level dict.
var upCache = map[[32]byte][]byte{}

func upCacheKey(r *Repo, body []byte) [32]byte {
	h := sha1.New()
	h.Write([]byte(strconv.Itoa(len(r.objects))))
	h.Write([]byte(strings.Join(sortedRefPairs(r), "\x00")))
	h.Write(body)
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

func sortedRefPairs(r *Repo) []string {
	var out []string
	for _, name := range sortedRefNames(r.refs) {
		out = append(out, name+"="+r.refs[name])
	}
	return out
}

func (rq *request) git(owner, name, action string) {
	r := store.getRepo(owner, name)
	if r == nil {
		rq.sendJSON(404, JObj{"message": "Not Found"})
		return
	}
	service := firstQ(rq.query, "service", "")
	if action == "info/refs" {
		recv := service == "git-receive-pack"
		typ := "upload-pack"
		if recv {
			typ = "receive-pack"
		}
		ct := "application/x-git-" + typ + "-advertisement"
		capStr := "multi_ack_detailed side-band-64k ofs-delta shallow deepen-since deepen-not deepen-relative no-progress include-tag ofs-delta agent=forge"
		if recv {
			capStr = "report-status side-band-64k delete-refs ofs-delta"
		}
		if !recv && rq.headers["git-protocol"] == "version=2" {
			var b []byte
			b = append(b, pktStr("version 2\n")...)
			b = append(b, pktStr("agent=forge\n")...)
			b = append(b, pktStr("ls-refs=unborn\n")...)
			b = append(b, pktStr("fetch=shallow wait-for-done\n")...)
			b = append(b, pktStr("object-format=sha1\n")...)
			b = append(b, flushPkt()...)
			rq.sendRaw(200, ct, b, nil)
			return
		}
		var b []byte
		b = append(b, pktStr("# service=git-"+typ+"\n")...)
		b = append(b, flushPkt()...)
		names := sortedRefNames(r.refs)
		head, hasHead := r.refs["refs/heads/"+r.deflt]
		if len(names) == 0 {
			b = append(b, pktStr(zeroSHA+" capabilities^{}\x00"+capStr+" agent=git/2.43.0\n")...)
		} else {
			for i, ref := range names {
				line := r.refs[ref] + " " + ref
				if i == 0 {
					line += "\x00" + capStr
				}
				b = append(b, pktStr(line+"\n")...)
			}
			if hasHead {
				b = append(b, pktStr(head+" HEAD\n")...)
			}
		}
		b = append(b, flushPkt()...)
		rq.sendRaw(200, ct, b, nil)
		return
	}
	body := rq.bodyBytes()
	if action == "git-receive-pack" {
		rq.receivePack(r, body)
		return
	}
	packets, _ := unpkt(body)
	if action == "git-upload-pack" {
		rq.uploadPack(r, body, packets)
		return
	}
	rq.sendJSON(404, JObj{"message": "Not Found"})
}

func bytePackets(packets []pktItem) [][]byte {
	var out [][]byte
	for _, p := range packets {
		if p.data != nil {
			out = append(out, p.data)
		}
	}
	return out
}

func (rq *request) uploadPack(r *Repo, body []byte, packets []pktItem) {
	raw := bytePackets(packets)
	firstIs := func(prefix string) bool {
		return len(packets) > 0 && packets[0].data != nil && bytes.HasPrefix(packets[0].data, []byte(prefix))
	}
	if firstIs("command=ls-refs") {
		var args []string
		for _, p := range raw {
			args = append(args, strings.TrimSpace(string(p)))
		}
		var prefixes []string
		for _, a := range args {
			if strings.HasPrefix(a, "ref-prefix ") {
				prefixes = append(prefixes, a[11:])
			}
		}
		var out []byte
		head, hasHead := r.refs["refs/heads/"+r.deflt]
		if hasHead {
			show := len(prefixes) == 0
			for _, p := range prefixes {
				if strings.HasPrefix("HEAD", p) {
					show = true
				}
			}
			if show {
				out = append(out, pktStr(head+" HEAD symref-target:refs/heads/"+r.deflt+"\n")...)
			}
		}
		for _, ref := range sortedRefNames(r.refs) {
			oid := r.refs[ref]
			show := len(prefixes) == 0
			for _, p := range prefixes {
				if strings.HasPrefix(ref, p) {
					show = true
				}
			}
			if !show {
				continue
			}
			line := oid + " " + ref
			if strings.HasPrefix(ref, "refs/tags/") {
				if obj, ok := r.get(oid); ok && obj.typ == "tag" {
					if m := peeledRe.FindSubmatch(obj.data); m != nil {
						line += " peeled:" + string(m[1])
					}
				}
			}
			out = append(out, pktStr(line+"\n")...)
		}
		out = append(out, flushPkt()...)
		rq.sendRaw(200, "application/x-git-upload-pack-result", out, nil)
		return
	}
	if firstIs("command=fetch") {
		ck := upCacheKey(r, body)
		if resp, ok := upCache[ck]; ok {
			rq.sendRaw(200, "application/x-git-upload-pack-result", resp, nil)
			return
		}
		var args []string
		for _, p := range raw {
			args = append(args, strings.TrimSpace(string(p)))
		}
		var wants, haves, boundaries []string
		var depth, deepenSince *int
		deepenNot := map[string]bool{}
		for _, a := range args {
			switch {
			case strings.HasPrefix(a, "want "):
				wants = append(wants, a[5:])
			case strings.HasPrefix(a, "have "):
				haves = append(haves, a[5:])
			case strings.HasPrefix(a, "shallow "):
				boundaries = append(boundaries, a[8:])
			case strings.HasPrefix(a, "deepen "):
				if depth == nil {
					if v, err := pyInt(a[7:]); err == nil {
						depth = &v
					} else {
						panic(pyErr{"int parse"})
					}
				}
			case strings.HasPrefix(a, "deepen-since "):
				if deepenSince == nil {
					if v, err := pyInt(a[13:]); err == nil {
						deepenSince = &v
					} else {
						panic(pyErr{"int parse"})
					}
				}
			case strings.HasPrefix(a, "deepen-not "):
				deepenNot[a[11:]] = true
			}
		}
		// Closure of what the client already has.
		haveClosure := map[string]bool{}
		boundarySet := strSet(boundaries)
		for _, h := range haves {
			todo := []string{h}
			for len(todo) > 0 {
				oid := todo[len(todo)-1]
				todo = todo[:len(todo)-1]
				if haveClosure[oid] {
					continue
				}
				haveClosure[oid] = true
				obj, ok := r.get(oid)
				if !ok {
					continue
				}
				switch obj.typ {
				case "commit":
					if m := treeRe.FindSubmatch(obj.data); m != nil {
						todo = append(todo, string(m[1]))
					}
					if !boundarySet[oid] {
						todo = append(todo, r.parents(oid)...)
					}
				case "tree":
					raw := obj.data
					i := 0
					for i < len(raw) {
						j := indexByte(raw[i:], 0)
						if j < 0 {
							break
						}
						j += i
						todo = append(todo, hex.EncodeToString(raw[j+1:j+21]))
						i = j + 21
					}
				case "tag":
					if m := tagObjRe.FindSubmatch(obj.data); m != nil {
						todo = append(todo, string(m[1]))
					}
				}
			}
		}
		var exclude []string
		for oid := range haveClosure {
			exclude = append(exclude, oid)
		}
		stopCommits := map[string]bool{}
		for ref := range deepenNot {
			oid, ok := r.refs[ref]
			if !ok {
				oid, ok = r.refs["refs/heads/"+ref]
			}
			if !ok {
				oid, ok = r.refs["refs/tags/"+ref]
			}
			if ok {
				for _, c := range r.walkCommits(oid) {
					stopCommits[c] = true
				}
			}
		}
		var stopList []string
		for c := range stopCommits {
			stopList = append(stopList, c)
		}
		var pack []byte
		var newBoundaries, unshallow []string
		if len(wants) > 0 {
			if depth != nil {
				pack = makepack(r, wants[:1], depth, exclude, boundaries, len(boundaries) > 0, deepenSince, stopList)
				newBoundaries = append([]string{}, r.shallowBnd...)
				for _, bnd := range boundaries {
					if !contains(newBoundaries, bnd) {
						unshallow = append(unshallow, bnd)
					}
				}
			} else {
				pack = makepack(r, wants, nil, exclude, boundaries, false, deepenSince, stopList)
				if deepenSince != nil || len(stopList) > 0 {
					newBoundaries = append([]string{}, r.shallowBnd...)
				}
				for _, bnd := range boundaries {
					if !contains(newBoundaries, bnd) {
						unshallow = append(unshallow, bnd)
					}
				}
			}
		} else {
			pack = makepack(r, nil, nil, exclude, nil, false, nil, nil)
		}
		var out []byte
		out = append(out, pktStr("acknowledgments\n")...)
		for _, h := range haves {
			if _, ok := r.get(h); ok {
				out = append(out, pktStr("ACK "+h+" common\n")...)
			}
		}
		if len(haves) > 0 {
			out = append(out, pktStr("ready\n")...)
		} else {
			out = append(out, pktStr("NAK\n")...)
		}
		out = append(out, flushPkt()...)
		out = append(out, pktStr("shallow-info\n")...)
		for _, oid := range unshallow {
			out = append(out, pktStr("unshallow "+oid+"\n")...)
		}
		for _, oid := range newBoundaries {
			out = append(out, pktStr("shallow "+oid+"\n")...)
		}
		out = append(out, flushPkt()...)
		out = append(out, pktStr("wanted-refs\n")...)
		out = append(out, flushPkt()...)
		out = append(out, pktStr("packfile\n")...)
		for i := 0; i < len(pack); i += 65515 {
			end := i + 65515
			if end > len(pack) {
				end = len(pack)
			}
			chunk := append([]byte{1}, pack[i:end]...)
			out = append(out, pkt(chunk)...)
		}
		resp := append(out, flushPkt()...)
		if len(upCache) >= 64 {
			upCache = map[[32]byte][]byte{}
		}
		upCache[ck] = resp
		rq.sendRaw(200, "application/x-git-upload-pack-result", resp, nil)
		return
	}
	// v0 upload-pack: wants are the pkt-line payloads.
	var wants []string
	for _, p := range raw {
		if bytes.HasPrefix(p, []byte("want ")) {
			s := string(p)
			parts := strings.SplitN(s, " ", 3)
			if len(parts) < 2 {
				panic(pyErr{"index"})
			}
			w := parts[1]
			if i := strings.IndexByte(w, 0); i >= 0 {
				w = w[:i]
			}
			wants = append(wants, strings.TrimSpace(w))
		}
	}
	ck := upCacheKey(r, body)
	if resp, ok := upCache[ck]; ok {
		rq.sendRaw(200, "application/x-git-upload-pack-result", resp, nil)
		return
	}
	var resp []byte
	resp = append(resp, pktStr("NAK\n")...)
	if len(wants) > 0 {
		pack := makepack(r, wants, nil, nil, nil, false, nil, nil)
		if len(pack) > 0 {
			resp = append(resp, pkt(append([]byte{1}, pack...))...)
		}
	}
	resp = append(resp, flushPkt()...)
	if len(upCache) >= 64 {
		upCache = map[[32]byte][]byte{}
	}
	upCache[ck] = resp
	rq.sendRaw(200, "application/x-git-upload-pack-result", resp, nil)
}

func (rq *request) receivePack(r *Repo, body []byte) {
	parsed, split := unpkt(body)
	packdata := body[split:]
	type cmd struct{ old, new, ref string }
	var cmds []cmd
	for _, p := range parsed {
		if p.data == nil {
			continue
		}
		s := strings.TrimSpace(string(p.data))
		parts := strings.Split(s, " ")
		if len(parts) >= 3 {
			ref := parts[2]
			if i := strings.IndexByte(ref, 0); i >= 0 {
				ref = ref[:i]
			}
			cmds = append(cmds, cmd{parts[0], parts[1], ref})
		}
	}
	if bytes.HasPrefix(packdata, []byte("PACK")) {
		readpack(packdata, r)
	}
	var statuses [][]byte
	for _, c := range cmds {
		ref := c.ref
		if i := strings.IndexByte(ref, 0); i >= 0 {
			ref = ref[:i]
		}
		if i := strings.IndexByte(ref, ' '); i >= 0 {
			ref = ref[:i]
		}
		current, ok := r.refs[ref]
		if !ok {
			current = zeroSHA
		}
		if c.old != current {
			statuses = append(statuses, []byte("ng "+ref+" stale info\n"))
			continue
		}
		if c.new == zeroSHA {
			r.delRef(ref)
			statuses = append(statuses, []byte("ok "+ref+"\n"))
			continue
		}
		if c.old != zeroSHA && !r.isAncestor(c.old, c.new) {
			statuses = append(statuses, []byte("ng "+ref+" non-fast-forward\n"))
			continue
		}
		r.setRef(ref, c.new)
		if strings.HasPrefix(ref, "refs/heads/") && len(current) > 0 && current[0] != '0' {
			if r.deflt == "" {
				r.deflt = lastSeg(ref)
			}
		}
		if strings.HasPrefix(ref, "refs/heads/") {
			n := 0
			var only string
			for k := range r.refs {
				if strings.HasPrefix(k, "refs/heads/") {
					n++
					only = k
				}
			}
			if n == 1 {
				r.deflt = lastSeg(only)
			}
		}
		statuses = append(statuses, []byte("ok "+ref+"\n"))
	}
	var report []byte
	report = append(report, pktStr("unpack ok\n")...)
	for _, s := range statuses {
		report = append(report, pkt(s)...)
	}
	report = append(report, flushPkt()...)
	out := append(pkt(append([]byte{1}, report...)), flushPkt()...)
	rq.sendRaw(200, "application/x-git-receive-pack-result", out, nil)
}

// lastSeg is Python's s.split('/')[-1].
func lastSeg(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func contains(list []string, x string) bool {
	for _, v := range list {
		if v == x {
			return true
		}
	}
	return false
}
