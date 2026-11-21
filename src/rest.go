package main

// rest.go — the REST v3 API under /api/v3: root, meta, user, repositories,
// branches, commits, git refs, contents, labels, issues + comments, and pull
// requests with squash merges. A faithful port of the Python rest() /
// rest_repo() / pulls() functions, including their response shapes and
// quirks (the path owner must be the viewer, PATCH merges body keys
// wholesale, and so on).

import (
	"encoding/base64"
	"regexp"
	"sort"
	"strings"
)

func (rq *request) ok(code int, v interface{}, headers ...[2]string) {
	rq.sendJSONH(code, v, headers)
}

func (rq *request) errJSON(code int, msg string) {
	rq.sendJSON(code, JObj{"message": msg})
}

func (rq *request) repoJSON(r *Repo) JObj {
	return JObj{
		"id": r.id, "node_id": gid("Repository", r.id), "name": r.name,
		"full_name": r.owner + "/" + r.name, "private": r.private,
		"owner": JObj{"login": r.owner, "id": 1000, "type": "User"},
		"description": r.desc, "default_branch": r.deflt, "has_issues": true,
		"permissions": JObj{"admin": true, "push": true, "pull": true},
		"created_at": store.Now(), "updated_at": store.Now(),
	}
}

func gid(t string, n int) string {
	s := base64.StdEncoding.EncodeToString([]byte(t + ":" + itoa(n)))
	return strings.TrimRight(s, "=")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func splitPath(path string) []string {
	t := strings.Trim(path, "/")
	if t == "" {
		return nil
	}
	return strings.Split(t, "/")
}

func (rq *request) rest(path string, q map[string][]string) {
	method := rq.method
	parts := splitPath(path)
	repoAt := func(i int) *Repo {
		if len(parts) <= i+1 {
			return nil
		}
		o, n := parts[i], parts[i+1]
		r := store.getRepo(o, n)
		if o == "adaiversen" {
			return r
		}
		return nil
	}
	if len(parts) == 0 {
		rq.ok(200, JObj{"current_user_url": "/user", "user_url": "/users/{user}", "repository_url": "/repos/{owner}/{repo}"}, [2]string{"X-OAuth-Scopes", "repo, read:org"})
		return
	}
	if len(parts) == 1 && parts[0] == "meta" {
		rq.ok(200, JObj{"installed_version": "1.0.0"})
		return
	}
	if len(parts) == 1 && parts[0] == "user" {
		rq.ok(200, JObj{"login": "adaiversen", "id": 1000, "name": "ada iversen", "email": "adaiversen@example.invalid", "type": "User"})
		return
	}
	if len(parts) == 2 && parts[0] == "user" && parts[1] == "repos" && method == "POST" {
		d := rq.bodyObj()
		if d == nil {
			rq.errJSON(400, "Invalid JSON")
			return
		}
		r := store.create("adaiversen", getStr(d, "name", ""))
		if v, present := d["private"]; present {
			r.private = pyTruthy(v)
		}
		r.desc = getStr(d, "description", "")
		rq.ok(201, rq.repoJSON(r))
		return
	}
	if len(parts) == 2 && parts[0] == "user" && parts[1] == "repos" && method == "GET" {
		var rs []*Repo
		for _, r := range store.repoOrder {
			if r.owner == "adaiversen" {
				rs = append(rs, r)
			}
		}
		per, err := pyInt(firstQ(q, "per_page", "30"))
		if err != nil {
			panic(pyErr{"per_page"})
		}
		page, err := pyInt(firstQ(q, "page", "1"))
		if err != nil {
			panic(pyErr{"page"})
		}
		var vals []*Repo
		lo := (page - 1) * per
		hi := page * per
		if lo < 0 {
			lo = 0
		}
		if hi > len(rs) {
			hi = len(rs)
		}
		if lo <= hi && lo <= len(rs) {
			vals = rs[lo:hi]
		}
		out := []interface{}{}
		for _, r := range vals {
			out = append(out, rq.repoJSON(r))
		}
		if page*per < len(rs) {
			host := rq.headers["host"]
			if host == "" {
				host = "localhost"
			}
			link := "<http://" + host + strings.SplitN(rq.rawTarget, "?", 2)[0] + "?per_page=" + itoa(per) + "&page=" + itoa(page+1) + ">; rel=\"next\""
			rq.ok(200, out, [2]string{"Link", link})
			return
		}
		rq.ok(200, out)
		return
	}
	r := repoAt(1)
	if len(parts) >= 3 && parts[0] == "repos" && r == nil {
		rq.errJSON(404, "Not Found")
		return
	}
	if len(parts) == 3 && parts[0] == "repos" {
		switch method {
		case "GET":
			rq.ok(200, rq.repoJSON(r))
			return
		case "PATCH":
			d := rq.bodyObj()
			if d == nil {
				rq.errJSON(400, "Invalid JSON")
				return
			}
			if v, present := d["description"]; present {
				s, isStr := v.(string)
				if !isStr {
					panic(pyErr{"description type"})
				}
				r.desc = s
			}
			rq.ok(200, rq.repoJSON(r))
			return
		case "DELETE":
			store.deleteRepo(r)
			rq.sendJSON(204, nil)
			return
		}
	}
	var rest []string
	if len(parts) > 3 {
		rest = parts[3:]
	}
	rq.restRepo(r, rest, q)
}

var knownHeads = map[string]bool{
	"branches": true, "commits": true, "git": true, "contents": true,
	"labels": true, "issues": true, "pulls": true,
}

func (rq *request) restRepo(r *Repo, p []string, q map[string][]string) {
	m := rq.method
	if r == nil {
		// The Python code falls through to a 404 for unknown tails, but
		// dereferences the missing repo (500) for known resource heads.
		if len(p) > 0 && knownHeads[p[0]] {
			panic(pyErr{"repo is None"})
		}
		rq.errJSON(404, "Not Found")
		return
	}
	if len(p) == 1 && p[0] == "branches" {
		out := []interface{}{}
		for _, ref := range r.refsOrd {
			if strings.HasPrefix(ref, "refs/heads/") {
				out = append(out, JObj{"name": lastSeg(ref), "commit": JObj{"sha": r.refs[ref]}})
			}
		}
		rq.ok(200, out)
		return
	}
	if len(p) == 2 && p[0] == "branches" {
		oid, found := r.refs["refs/heads/"+p[1]]
		if !found {
			rq.errJSON(404, "Not Found")
			return
		}
		rq.ok(200, JObj{"name": p[1], "commit": JObj{"sha": oid}})
		return
	}
	if len(p) == 1 && p[0] == "commits" {
		branch := firstQ(q, "sha", r.deflt)
		tip, found := r.refs["refs/heads/"+branch]
		out := []interface{}{}
		if found {
			for _, oid := range r.walkCommits(tip) {
				obj, _ := r.get(oid)
				// Python: data.split(b'\n\n',1)[-1] — the whole payload
				// when there is no blank-line separator.
				s := string(obj.data)
				if i := strings.Index(s, "\n\n"); i >= 0 {
					s = s[i+2:]
				}
				msg := strings.TrimRight(s, " \t\r\n\v\f")
				out = append(out, JObj{"sha": oid, "commit": JObj{"message": msg}})
			}
		}
		rq.ok(200, out)
		return
	}
	if len(p) >= 2 && p[0] == "git" && p[1] == "refs" {
		if m == "POST" && len(p) == 2 {
			d := rq.bodyObj()
			if d == nil {
				panic(pyErr{"ref body"})
			}
			ref := getStr(d, "ref", "")
			oid := getStr(d, "sha", "")
			if dv, present := d["ref"]; present && dv == nil {
				ref = ""
			}
			if dv, present := d["sha"]; present && dv == nil {
				oid = ""
			}
			r.setRef(ref, oid)
			rq.ok(201, JObj{"ref": ref, "object": JObj{"sha": oid}})
			return
		}
		ref := "refs/" + strings.Join(p[2:], "/")
		oid, found := r.refs[ref]
		if !found {
			rq.errJSON(404, "Not Found")
			return
		}
		rq.ok(200, JObj{"ref": ref, "object": JObj{"sha": oid}})
		return
	}
	if len(p) >= 1 && p[0] == "contents" {
		rq.contents(r, p, q)
		return
	}
	if len(p) >= 1 && p[0] == "labels" {
		rq.labels(r, p)
		return
	}
	if len(p) >= 1 && p[0] == "issues" {
		rq.issues(r, p, q)
		return
	}
	if len(p) >= 1 && p[0] == "pulls" {
		rq.pulls(r, p[1:], q)
		return
	}
	rq.errJSON(404, "Not Found")
}

func (rq *request) contents(r *Repo, p []string, q map[string][]string) {
	m := rq.method
	path := strings.Join(p[1:], "/")
	var d JObj
	if m != "GET" {
		d = rq.bodyObj()
		if d == nil {
			rq.errJSON(400, "Invalid JSON")
			return
		}
	}
	branch := r.deflt
	if v := q["ref"]; len(v) > 0 {
		branch = v[0]
	} else if d != nil {
		branch = getStr(d, "branch", r.deflt)
	}
	tip, hasTip := r.refs["refs/heads/"+branch]
	files := map[string]string{}
	if hasTip {
		files = r.filesAt(tip)
	}
	switch m {
	case "GET":
		if oid, found := files[path]; found {
			obj, _ := r.get(oid)
			name := path
			if i := strings.LastIndexByte(path, '/'); i >= 0 {
				name = path[i+1:]
			}
			rq.ok(200, JObj{"name": name, "path": path, "sha": oid, "size": len(obj.data), "type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString(obj.data)})
			return
		}
		prefix := ""
		if path != "" {
			prefix = strings.TrimRight(path, "/") + "/"
		}
		nameSet := map[string]bool{}
		var names []string
		for f := range files {
			if strings.HasPrefix(f, prefix) {
				seg := strings.SplitN(f[len(prefix):], "/", 2)[0]
				if !nameSet[seg] {
					nameSet[seg] = true
					names = append(names, seg)
				}
			}
		}
		sort.Strings(names)
		if len(names) > 0 {
			out := []interface{}{}
			for _, x := range names {
				isDir := false
				for f := range files {
					if strings.HasPrefix(f, prefix+x+"/") {
						isDir = true
						break
					}
				}
				typ := "file"
				if isDir {
					typ = "dir"
				}
				out = append(out, JObj{"name": x, "path": prefix + x, "type": typ, "sha": files[prefix+x]})
			}
			rq.ok(200, out)
			return
		}
		rq.errJSON(404, "Not Found")
		return
	case "PUT":
		if v, present := d["sha"]; present {
			s, _ := v.(string)
			if files[path] != s {
				rq.errJSON(409, "sha does not match")
				return
			}
		}
		data, derr := base64.StdEncoding.DecodeString(getStr(d, "content", ""))
		if derr != nil {
			panic(pyErr{"base64"})
		}
		blob := r.put("blob", data)
		files[path] = blob
		tree := r.tree(files)
		var parents []string
		if hasTip {
			parents = []string{tip}
		}
		oid := r.commit(tree, parents, getStr(d, "message", "update"))
		branch = getStr(d, "branch", branch)
		r.setRef("refs/heads/"+branch, oid)
		code := 201
		if _, present := d["sha"]; present {
			code = 200
		}
		rq.ok(code, JObj{"content": JObj{"sha": blob, "encoding": "base64"}, "commit": JObj{"sha": oid}})
		return
	case "DELETE":
		if _, found := files[path]; !found {
			rq.errJSON(404, "Not Found")
			return
		}
		delete(files, path)
		tree := r.tree(files)
		var parents []string
		if hasTip {
			parents = []string{tip}
		}
		oid := r.commit(tree, parents, getStr(d, "message", "delete"))
		r.setRef("refs/heads/"+r.deflt, oid)
		rq.ok(200, JObj{"commit": JObj{"sha": oid}})
		return
	}
	rq.errJSON(404, "Not Found")
}

func (rq *request) labels(r *Repo, p []string) {
	m := rq.method
	if len(p) == 1 && m == "POST" {
		d := rq.bodyObj()
		name, present := d["name"]
		if !present {
			panic(pyErr{"name missing"})
		}
		ns, _ := name.(string)
		item := JObj{"id": len(r.labels) + 1, "name": ns, "color": getStr(d, "color", "ededed")}
		r.labels = append(r.labels, item)
		rq.ok(201, item)
		return
	}
	if len(p) == 1 {
		out := []interface{}{}
		for _, l := range r.labels {
			out = append(out, l)
		}
		rq.ok(200, out)
		return
	}
	if m == "DELETE" {
		var kept []JObj
		for _, l := range r.labels {
			if l["name"] != p[1] {
				kept = append(kept, l)
			}
		}
		r.labels = kept
		rq.sendJSON(204, nil)
		return
	}
	rq.errJSON(404, "Not Found")
}

func (rq *request) issues(r *Repo, p []string, q map[string][]string) {
	m := rq.method
	if len(p) == 1 && m == "POST" {
		d := rq.bodyObj()
		if d == nil {
			panic(pyErr{"issue body"})
		}
		var labelNames []string
		for _, l := range r.labels {
			labelNames = append(labelNames, l["name"].(string))
		}
		rawLabels, _ := d["labels"].([]interface{})
		for _, x := range rawLabels {
			var nm interface{}
			if xd, isDict := x.(JObj); isDict {
				nm = xd["name"]
			} else {
				nm = x
			}
			found := false
			for _, n := range labelNames {
				if nm == n {
					found = true
				}
			}
			if !found {
				rq.errJSON(422, "Validation Failed")
				return
			}
		}
		stored := []interface{}{}
		for _, l := range r.labels {
			ln := l["name"]
			inList := false
			for _, x := range rawLabels {
				if x == ln {
					inList = true
				}
				if xd, isDict := x.(JObj); isDict {
					if eqJSON(xd, l) {
						inList = true
					}
				}
			}
			if inList {
				stored = append(stored, l)
			}
		}
		issue := JObj{
			"id": store.nextIssue, "number": len(r.issues) + 1,
			"title": getStr(d, "title", ""), "body": getStr(d, "body", ""),
			"state": "open", "labels": stored, "comments": []interface{}{},
			"created_at": store.Now(), "updated_at": store.Now(),
		}
		store.nextIssue++
		r.issues = append(r.issues, issue)
		r.byNum[issue["number"].(int)] = issue
		rq.ok(201, issue)
		return
	}
	if len(p) == 1 {
		state := firstQ(q, "state", "open")
		var vals []JObj
		for _, x := range r.issues {
			if state == "all" || x["state"] == state {
				vals = append(vals, x)
			}
		}
		desc := firstQ(q, "direction", "asc") == "desc"
		sort.SliceStable(vals, func(i, j int) bool {
			ca, cb := vals[i]["created_at"].(string), vals[j]["created_at"].(string)
			if ca != cb {
				if desc {
					return ca > cb
				}
				return ca < cb
			}
			ia, ib := vals[i]["id"].(int), vals[j]["id"].(int)
			if desc {
				return ia > ib
			}
			return ia < ib
		})
		out := []interface{}{}
		for _, v := range vals {
			out = append(out, v)
		}
		rq.ok(200, out)
		return
	}
	num, err := pyInt(p[1])
	if err != nil {
		panic(pyErr{"issue number"})
	}
	issue, found := r.byNum[num]
	if !found {
		for _, x := range r.issues {
			if x["number"] == num {
				issue = x
				found = true
				break
			}
		}
	}
	if !found {
		rq.errJSON(404, "Not Found")
		return
	}
	if len(p) == 2 && m == "GET" {
		rq.ok(200, issue)
		return
	}
	if len(p) == 2 && m == "PATCH" {
		d := rq.bodyObj()
		if d == nil {
			panic(pyErr{"patch body"})
		}
		for k, v := range d {
			issue[k] = v
		}
		issue["updated_at"] = store.Now()
		rq.ok(200, issue)
		return
	}
	if p[len(p)-1] == "comments" {
		if m == "POST" {
			d := rq.bodyObj()
			if d == nil {
				panic(pyErr{"comment body"})
			}
			c := JObj{"id": store.nextComment, "body": getStr(d, "body", "")}
			store.nextComment++
			issue["comments"] = append(issue["comments"].([]interface{}), c)
			rq.ok(201, c)
			return
		}
		rq.ok(200, issue["comments"])
		return
	}
	rq.errJSON(404, "Not Found")
}

// eqJSON is Python == for JSON values (used by the issue-label filter, where
// a request may carry label objects rather than names).
func eqJSON(a, b interface{}) bool {
	switch av := a.(type) {
	case JObj:
		bv, ok := b.(JObj)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !eqJSON(v, bv[k]) {
				return false
			}
		}
		return true
	case []interface{}:
		bv, ok := b.([]interface{})
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !eqJSON(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// doPullCreate is the POST branch of the Python pulls() helper, shared by
// the REST endpoint and the MCP/GraphQL callers that drive it indirectly.
// It reports whether the head/base refs resolved (the Python helper answers
// 422 in that case; indirect callers just observe that no PR appeared).
func (rq *request) doPullCreate(r *Repo, d JObj) bool {
	headFull := getStr(d, "head", "")
	head := headFull
	if i := strings.LastIndexByte(head, ':'); i >= 0 {
		head = head[i+1:]
	}
	base := getStr(d, "base", r.deflt)
	ho, okH := r.refs["refs/heads/"+head]
	bo, okB := r.refs["refs/heads/"+base]
	if !okH || !okB {
		return false
	}
	headFiles := r.filesOrdered(ho)
	baseFiles := r.filesAt(bo)
	headMap := r.filesAt(ho)
	changed := []interface{}{}
	for _, po := range headFiles {
		if bv, inBase := baseFiles[po.path]; !inBase {
			changed = append(changed, JObj{"filename": po.path, "status": "added", "additions": 1, "deletions": 0, "changes": 1})
		} else if bv != po.oid {
			changed = append(changed, JObj{"filename": po.path, "status": "modified", "additions": 1, "deletions": 0, "changes": 1})
		}
	}
	for _, po := range r.filesOrdered(bo) {
		if _, inHead := headMap[po.path]; !inHead {
			changed = append(changed, JObj{"filename": po.path, "status": "removed", "additions": 0, "deletions": 1, "changes": 1})
		}
	}
	pr := JObj{
		"id": store.nextPull, "number": len(r.pulls) + 1,
		"title": getStr(d, "title", ""), "body": getStr(d, "body", ""),
		"state": "open", "merged": false,
		"head": JObj{"ref": head, "sha": ho}, "base": JObj{"ref": base, "sha": bo},
		"user": JObj{"login": "adaiversen"}, "mergeable": true, "mergeable_state": "clean",
		"changed_files": len(changed), "files": changed, "merged_at": nil,
	}
	store.nextPull++
	r.pulls = append(r.pulls, pr)
	return true
}

func (rq *request) pulls(r *Repo, p []string, q map[string][]string) {
	m := rq.method
	if len(p) == 0 && m == "POST" {
		d := rq.bodyObj()
		if d == nil {
			panic(pyErr{"pull body"})
		}
		if !rq.doPullCreate(r, d) {
			rq.errJSON(422, "head or base ref not found")
			return
		}
		rq.ok(201, r.pulls[len(r.pulls)-1])
		return
	}
	if len(p) == 0 {
		state := firstQ(q, "state", "open")
		out := []interface{}{}
		for _, x := range r.pulls {
			if state == "all" || x["state"] == state {
				out = append(out, x)
			}
		}
		rq.ok(200, out)
		return
	}
	num, err := pyInt(p[0])
	if err != nil {
		rq.errJSON(404, "Not Found")
		return
	}
	var pr JObj
	for _, x := range r.pulls {
		if x["number"] == num {
			pr = x
			break
		}
	}
	if pr == nil {
		rq.errJSON(404, "Not Found")
		return
	}
	if len(p) == 1 {
		rq.ok(200, pr)
		return
	}
	if p[1] == "files" {
		rq.ok(200, pr["files"])
		return
	}
	if p[1] == "merge" && m == "PUT" {
		base := pr["base"].(JObj)["ref"].(string)
		tip, okT := r.refs["refs/heads/"+base]
		head, okH := r.refs["refs/heads/"+pr["head"].(JObj)["ref"].(string)]
		if !okT || !okH {
			rq.errJSON(405, "Pull request is not mergeable")
			return
		}
		merged, conf := threeway(r, tip, head)
		if conf {
			rq.errJSON(405, "Pull request is not mergeable")
			return
		}
		tree := r.tree(merged)
		oid := r.commit(tree, []string{tip}, pr["title"].(string)+" (#"+itoa(num)+")")
		r.setRef("refs/heads/"+base, oid)
		pr["state"] = "closed"
		pr["merged"] = true
		pr["merged_at"] = store.Now()
		pr["merge_commit_sha"] = oid
		rq.ok(200, JObj{"sha": oid, "merged": true, "message": "Pull request successfully merged"})
		return
	}
	rq.errJSON(404, "Not Found")
}

var parentLooseRe = regexp.MustCompile(`parent ([0-9a-f]{40})`)

// threeway merges head into base over their common ancestor's file map,
// reporting a conflict when both sides changed the same path differently.
func threeway(r *Repo, base, head string) (map[string]string, bool) {
	parentsOf := func(oid string) []string {
		obj, ok := r.get(oid)
		if !ok {
			return nil
		}
		// The Python threeway uses an UNANCHORED findall and no type
		// check, unlike store.parents.
		var out []string
		for _, m := range parentLooseRe.FindAllSubmatch(obj.data, -1) {
			out = append(out, string(m[1]))
		}
		return out
	}
	ba := map[string]int{}
	todo := [][2]interface{}{}
	todo = append(todo, [2]interface{}{base, 0})
	for len(todo) > 0 {
		cur := todo[0]
		todo = todo[1:]
		oid, d := cur[0].(string), cur[1].(int)
		if _, done := ba[oid]; done {
			continue
		}
		ba[oid] = d
		for _, p := range parentsOf(oid) {
			todo = append(todo, [2]interface{}{p, d + 1})
		}
	}
	var common string
	hasCommon := false
	queue := []string{head}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if _, in := ba[oid]; in {
			common = oid
			hasCommon = true
			break
		}
		queue = append(queue, parentsOf(oid)...)
	}
	var b map[string]string
	if hasCommon {
		b = r.filesAt(common)
	} else {
		b = map[string]string{}
	}
	left := r.filesAt(base)
	right := r.filesAt(head)
	out := map[string]string{}
	for k, v := range left {
		out[k] = v
	}
	conf := false
	pathSet := map[string]bool{}
	for k := range b {
		pathSet[k] = true
	}
	for k := range left {
		pathSet[k] = true
	}
	for k := range right {
		pathSet[k] = true
	}
	for path := range pathSet {
		oldV, inB := b[path]
		x, inL := left[path]
		y, inR := right[path]
		eqXY := inL == inR && (!inL || x == y)
		if eqXY {
			continue
		}
		eqXO := inL == inB && (!inL || x == oldV)
		if eqXO {
			if !inR {
				delete(out, path)
			} else {
				out[path] = y
			}
			continue
		}
		eqYO := inR == inB && (!inR || y == oldV)
		if eqYO {
			continue
		}
		conf = true
	}
	return out, conf
}

func firstQ(q map[string][]string, key, def string) string {
	if v, ok := q[key]; ok && len(v) > 0 {
		return v[0]
	}
	return def
}
