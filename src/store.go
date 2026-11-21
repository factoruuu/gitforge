package main

// store.go — the forge's in-memory state: users, repositories, git objects,
// refs, issues, labels and pull requests. A faithful port of the Python
// store.py semantics, including its clock: when MAKEFUN_TRIAL_NOW is set,
// every Now() call advances a per-store counter by one second from the
// trial's base instant, so timestamps are deterministic for a given request
// sequence; otherwise Now() is the wall clock.

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const zeroSHA = "0000000000000000000000000000000000000000"

type userInfo struct {
	id    int
	name  string
	typ   string
}

var users = map[string]userInfo{
	"adaiversen":       {1000, "ada iversen", "User"},
	"quinnkeller":      {1001, "quinn keller", "User"},
	"olgabauer":        {1002, "olga bauer", "User"},
	"faint-meadow-org": {1003, "faint meadow org", "Organization"},
}

type gitObject struct {
	typ  string
	data []byte
}

type Repo struct {
	owner   string
	name    string
	id      int
	private bool
	desc    string
	deflt   string
	objects map[string]gitObject
	refs    map[string]string
	refsOrd []string
	issues  []JObj
	byNum   map[int]JObj
	pulls   []JObj
	labels  []JObj

	packCache  map[string]packCacheEntry
	packOrder  []string
	shallowBnd []string
}

// setRef assigns a ref, remembering first-insertion order the way a Python
// dict would (observable in the branches listing).
func (r *Repo) setRef(ref, oid string) {
	if _, ok := r.refs[ref]; !ok {
		r.refsOrd = append(r.refsOrd, ref)
	}
	r.refs[ref] = oid
}

func (r *Repo) delRef(ref string) {
	if _, ok := r.refs[ref]; !ok {
		return
	}
	delete(r.refs, ref)
	for i, x := range r.refsOrd {
		if x == ref {
			r.refsOrd = append(r.refsOrd[:i], r.refsOrd[i+1:]...)
			break
		}
	}
}

type packCacheEntry struct {
	pack       []byte
	boundaries []string
}

func newRepo(owner, name string, id int) *Repo {
	return &Repo{
		owner: owner, name: name, id: id, deflt: "main",
		objects: map[string]gitObject{}, refs: map[string]string{},
		byNum: map[int]JObj{}, packCache: map[string]packCacheEntry{},
	}
}

// put stores a git object and returns its sha1 id ("<type> <len>\0<data>").
func (r *Repo) put(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	oid := hex.EncodeToString(h.Sum(nil))
	if _, ok := r.objects[oid]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		r.objects[oid] = gitObject{typ, cp}
	}
	return oid
}

func (r *Repo) get(oid string) (gitObject, bool) {
	o, ok := r.objects[oid]
	return o, ok
}

// commit builds a commit object with the forge's fixed author/committer and
// the fixed timestamp the Python implementation used (1700000000, +0000).
func (r *Repo) commit(tree string, parents []string, msg string) string {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	stamp := "1700000000 +0000"
	author := "ada iversen <adaiversen@example.invalid>"
	b.WriteString("author " + author + " " + stamp + "\n")
	b.WriteString("committer " + author + " " + stamp + "\n")
	b.WriteString("\n")
	b.WriteString(strings.TrimRight(msg, " \t\r\n") + "\n")
	return r.put("commit", []byte(b.String()))
}

// tree builds (nested) tree objects from a flat path -> blob-oid map, using
// git's tree entry ordering (names sort with directories as name+"/").
func (r *Repo) tree(files map[string]string) string {
	type node struct {
		dirs  map[string]*node
		blobs map[string]string
	}
	root := &node{map[string]*node{}, map[string]string{}}
	for path, blob := range files {
		parts := strings.Split(path, "/")
		n := root
		for _, p := range parts[:len(parts)-1] {
			if n.dirs[p] == nil {
				n.dirs[p] = &node{map[string]*node{}, map[string]string{}}
			}
			n = n.dirs[p]
		}
		n.blobs[parts[len(parts)-1]] = blob
	}
	var build func(n *node) string
	build = func(n *node) string {
		type entry struct {
			name, mode, oid, key string
		}
		var entries []entry
		for name, sub := range n.dirs {
			entries = append(entries, entry{name, "40000", build(sub), name + "/"})
		}
		for name, oid := range n.blobs {
			entries = append(entries, entry{name, "100644", oid, name})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
		var buf []byte
		for _, e := range entries {
			buf = append(buf, e.mode...)
			buf = append(buf, ' ')
			buf = append(buf, e.name...)
			buf = append(buf, 0)
			raw, _ := hex.DecodeString(e.oid)
			buf = append(buf, raw...)
		}
		return r.put("tree", buf)
	}
	return build(root)
}

type pathOid struct {
	path string
	oid  string
}

// filesOrdered flattens the tree of a commit into (path, blob-oid) pairs in
// tree-walk order — the insertion order of the Python files_at dict, which
// the pull-request file list inherits.
func (r *Repo) filesOrdered(commit string) []pathOid {
	var out []pathOid
	obj, ok := r.get(commit)
	if !ok {
		return out
	}
	first := obj.data
	if i := indexByte(obj.data, '\n'); i >= 0 {
		first = obj.data[:i]
	}
	fields := strings.Fields(string(first))
	if len(fields) < 2 {
		return out
	}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		o, ok := r.get(oid)
		if !ok {
			return
		}
		raw := o.data
		i := 0
		for i < len(raw) {
			j := indexByte(raw[i:], 0)
			if j < 0 {
				return
			}
			j += i
			hdr := string(raw[i:j])
			sp := strings.IndexByte(hdr, ' ')
			if sp < 0 {
				return
			}
			mode, name := hdr[:sp], hdr[sp+1:]
			child := hex.EncodeToString(raw[j+1 : j+21])
			i = j + 21
			if mode == "40000" {
				walk(child, prefix+name+"/")
			} else {
				out = append(out, pathOid{prefix + name, child})
			}
		}
	}
	walk(fields[1], "")
	return out
}

// filesAt flattens the tree of a commit into path -> blob-oid.
func (r *Repo) filesAt(commit string) map[string]string {
	out := map[string]string{}
	for _, po := range r.filesOrdered(commit) {
		out[po.path] = po.oid
	}
	return out
}

var parentRe = regexp.MustCompile(`(?m)^parent ([0-9a-f]{40})$`)

func (r *Repo) parents(oid string) []string {
	obj, ok := r.get(oid)
	if !ok || obj.typ != "commit" {
		return nil
	}
	var out []string
	for _, m := range parentRe.FindAllSubmatch(obj.data, -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// walkCommits yields commits breadth-first from a tip (queue order, parents
// in listed order), each once — the Python walk_commits generator.
func (r *Repo) walkCommits(tip string) []string {
	var out []string
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if seen[c] {
			continue
		}
		seen[c] = true
		obj, ok := r.get(c)
		if ok && obj.typ == "commit" {
			out = append(out, c)
			queue = append(queue, r.parents(c)...)
		}
	}
	return out
}

func (r *Repo) isAncestor(old, newTip string) bool {
	for _, oid := range r.walkCommits(newTip) {
		if oid == old {
			return true
		}
	}
	return false
}

type Store struct {
	repos       map[[2]string]*Repo
	repoOrder   []*Repo
	nextID      int
	nextIssue   int
	nextPull    int
	nextComment int
	clock       int64
	base        *int64
}

func newStore() *Store {
	return &Store{repos: map[[2]string]*Repo{}, nextID: 1, nextIssue: 1, nextPull: 1, nextComment: 1}
}

// Now mirrors store.now(): with MAKEFUN_TRIAL_NOW set, the base instant is
// parsed once (1700000000 on parse failure) and every call returns base+clock
// seconds, clock incrementing per call; otherwise the current UTC time.
func (s *Store) Now() string {
	raw := os.Getenv("MAKEFUN_TRIAL_NOW")
	if raw != "" {
		if s.base == nil {
			v := int64(1700000000)
			if t, err := time.Parse(time.RFC3339Nano, strings.Replace(raw, "Z", "+00:00", 1)); err == nil {
				v = t.Unix()
			}
			s.base = &v
		}
		s.clock++
		return time.Unix(*s.base+s.clock, 0).UTC().Format("2006-01-02T15:04:05Z")
	}
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

func (s *Store) create(owner, name string) *Repo {
	k := [2]string{owner, name}
	if r, ok := s.repos[k]; ok {
		return r
	}
	r := newRepo(owner, name, s.nextID)
	s.nextID++
	s.repos[k] = r
	s.repoOrder = append(s.repoOrder, r)
	return r
}

func (s *Store) deleteRepo(r *Repo) {
	k := [2]string{r.owner, r.name}
	delete(s.repos, k)
	for i, x := range s.repoOrder {
		if x == r {
			s.repoOrder = append(s.repoOrder[:i], s.repoOrder[i+1:]...)
			break
		}
	}
}

func (s *Store) getRepo(owner, name string) *Repo {
	return s.repos[[2]string{owner, name}]
}

var store = newStore()

// helpers shared with the protocol code

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// pyInt mimics Python's int(str): trims whitespace, optional sign, digits.
func pyInt(s string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(s))
}

// sortedRefs returns ref names in sorted order (Python sorted() on strings).
func sortedRefNames(refs map[string]string) []string {
	names := make([]string, 0, len(refs))
	for k := range refs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
