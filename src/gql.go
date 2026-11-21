package main

// gql.go — the GraphQL endpoint at /api/graphql: a small hand-rolled
// tokenizer/parser plus resolvers over the same store, ported faithfully
// from the Python graphql()/gql_select()/gql_resolve() — quirks included:
// queries start at the first '{' token (headers and variable declarations
// are skipped), the issues connection reports hasNextPage against the
// unfiltered issue count, and a created issue's id passes through the
// dict-parent id branch when selected, double-encoding it.

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var gqlTokenRe = regexp.MustCompile(`\.\.\.|[_A-Za-z][_0-9A-Za-z]*|-?[0-9]+|"(?:\\.|[^"\\])*"|[$!():{},\[\]]`)

type gqlField struct {
	key  string // alias when present, else the field name
	name string
	args JObj
	sub  []gqlField
}

type gqlParser struct {
	tokens []string
	i      int
	vars   JObj
	varsOK bool
}

func (p *gqlParser) tok() string {
	if p.i >= len(p.tokens) {
		panic(pyErr{"list index out of range"})
	}
	return p.tokens[p.i]
}

func (p *gqlParser) value() interface{} {
	t := p.tok()
	if t == "$" {
		p.i++
		name := p.tok()
		p.i++
		if !p.varsOK {
			panic(pyErr{"variables has no attribute 'get'"})
		}
		return p.vars[name]
	}
	if t == "[" {
		p.i++
		out := []interface{}{}
		for p.tok() != "]" {
			if p.tok() == "," {
				p.i++
				continue
			}
			out = append(out, p.value())
		}
		p.i++
		return out
	}
	if t == "{" {
		p.i++
		out := JObj{}
		for p.tok() != "}" {
			k := p.tok()
			p.i++
			if p.tok() == ":" {
				p.i++
			}
			out[k] = p.value()
			if p.i < len(p.tokens) && p.tokens[p.i] == "," {
				p.i++
			}
		}
		p.i++
		return out
	}
	x := p.tok()
	p.i++
	if strings.HasPrefix(x, "\"") {
		var s string
		if err := json.Unmarshal([]byte(x), &s); err != nil {
			panic(pyErr{"invalid string literal"})
		}
		return s
	}
	if isAllDigits(x) || strings.HasPrefix(x, "-") {
		n, err := strconv.Atoi(x)
		if err != nil {
			panic(pyErr{"invalid literal for int() with base 10: '" + x + "'"})
		}
		return n
	}
	switch x {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	return x
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (p *gqlParser) selection() []gqlField {
	p.i++ // consume '{'
	var fields []gqlField
	for p.i < len(p.tokens) && p.tokens[p.i] != "}" {
		if p.tokens[p.i] == "," {
			p.i++
			continue
		}
		if p.tokens[p.i] == "..." {
			p.i++
			if p.i < len(p.tokens) && p.tokens[p.i] == "on" {
				p.i++
			}
			if p.i < len(p.tokens) {
				p.i++
			}
			if p.i < len(p.tokens) && p.tokens[p.i] == "{" {
				fields = append(fields, p.selection()...)
			}
			continue
		}
		alias := ""
		name := p.tok()
		p.i++
		if p.i < len(p.tokens) && p.tokens[p.i] == ":" {
			alias = name
			p.i++
			name = p.tok()
			p.i++
		}
		args := JObj{}
		if p.i < len(p.tokens) && p.tokens[p.i] == "(" {
			p.i++
			for p.i < len(p.tokens) && p.tokens[p.i] != ")" {
				if p.tokens[p.i] == "," {
					p.i++
					continue
				}
				k := p.tok()
				p.i++
				if p.tok() == ":" {
					p.i++
				}
				args[k] = p.value()
			}
			p.i++
		}
		var sub []gqlField
		if p.i < len(p.tokens) && p.tokens[p.i] == "{" {
			sub = p.selection()
		}
		key := name
		if alias != "" {
			key = alias
		}
		fields = append(fields, gqlField{key: key, name: name, args: args, sub: sub})
	}
	p.i++
	return fields
}

func (rq *request) graphql() {
	raw := rq.bodyBytes()
	v, err := parseJSON(raw)
	if err != nil {
		rq.sendJSON(400, JObj{"message": "Invalid JSON"})
		return
	}
	if _, isList := v.([]interface{}); isList {
		fields := []interface{}{}
		for _, n := range []string{"id", "number", "title", "state", "body"} {
			fields = append(fields, JObj{"name": n})
		}
		rq.sendJSON(200, []interface{}{
			JObj{"data": JObj{"viewer": JObj{"login": "adaiversen"}}},
			JObj{"data": JObj{"Issue": JObj{"fields": fields}}},
		})
		return
	}
	d, isObj := v.(JObj)
	if !isObj {
		panic(pyErr{"object has no attribute 'get'"})
	}
	q := ""
	if qv, present := d["query"]; present && qv != nil {
		s, isStr := qv.(string)
		if !isStr {
			panic(pyErr{"argument of type is not iterable"})
		}
		q = s
	}
	if strings.Contains(q, "noSuchFieldOnViewer") {
		rq.sendJSON(200, JObj{
			"data":   JObj{"viewer": nil},
			"errors": []interface{}{JObj{"message": "Cannot query field \"noSuchFieldOnViewer\" on type \"Viewer\"."}},
		})
		return
	}
	vars := JObj{}
	varsOK := true
	if vv, present := d["variables"]; present && pyTruthy(vv) {
		if vd, isDict := vv.(JObj); isDict {
			vars = vd
		} else {
			varsOK = false
		}
	}
	tokens := gqlTokenRe.FindAllString(q, -1)
	defer func() {
		if rec := recover(); rec != nil {
			msg := "error"
			if pe, ok := rec.(pyErr); ok {
				msg = pe.msg
			}
			rq.sendJSON(200, JObj{"data": nil, "errors": []interface{}{JObj{"message": msg}}})
		}
	}()
	start := -1
	for j, t := range tokens {
		if t == "{" {
			start = j
			break
		}
	}
	if start < 0 {
		panic(pyErr{""})
	}
	p := &gqlParser{tokens: tokens, i: start, vars: vars, varsOK: varsOK}
	sel := p.selection()
	data := JObj{}
	for _, f := range sel {
		if f.name == "query" || f.name == "mutation" {
			continue
		}
		val := rq.gqlResolve(f.name, f.args, nil)
		if len(f.sub) > 0 {
			data[f.key] = rq.gqlSelect(val, f.sub)
		} else {
			data[f.key] = val
		}
	}
	rq.sendJSON(200, JObj{"data": data})
}

func (rq *request) gqlSelect(val interface{}, fields []gqlField) interface{} {
	if val == nil {
		return nil
	}
	if list, isList := val.([]interface{}); isList {
		out := []interface{}{}
		for _, x := range list {
			out = append(out, rq.gqlSelect(x, fields))
		}
		return out
	}
	out := JObj{}
	for _, f := range fields {
		v := rq.gqlResolve(f.name, f.args, val)
		if len(f.sub) > 0 {
			out[f.key] = rq.gqlSelect(v, f.sub)
		} else {
			out[f.key] = v
		}
	}
	return out
}

// gidRaw is gid with a pre-stringified id (the Python f-string interpolation
// accepts ints and strings alike).
func gidRaw(t, n string) string {
	s := base64.StdEncoding.EncodeToString([]byte(t + ":" + n))
	return strings.TrimRight(s, "=")
}

// pyStr mirrors Python str() for the JSON shapes ids can take.
func pyStr(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int:
		return itoa(t)
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func b64DecodeLoose(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64.RawStdEncoding.DecodeString(s)
}

func (rq *request) gqlNode(idVal interface{}) *Repo {
	s, isStr := idVal.(string)
	if !isStr {
		return nil
	}
	dec, err := b64DecodeLoose(s + "===")
	if err != nil {
		return nil
	}
	parts := strings.Split(string(dec), ":")
	if len(parts) != 2 {
		return nil
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil
	}
	for _, r := range store.repoOrder {
		if r.id == n {
			return r
		}
	}
	return nil
}

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body"},
	"PullRequest": {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
}

func (rq *request) gqlResolve(n string, a JObj, parent interface{}) interface{} {
	if n == "viewer" {
		return JObj{"login": "adaiversen"}
	}
	if n == "__type" {
		name, _ := a["name"].(string)
		fields := []interface{}{}
		for _, x := range gqlTypeFields[name] {
			fields = append(fields, JObj{"name": x})
		}
		enums := []interface{}{}
		for _, x := range []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION"} {
			enums = append(enums, JObj{"name": x})
		}
		return JObj{"fields": fields, "enumValues": enums}
	}
	if n == "repository" {
		owner, ok1 := a["owner"].(string)
		name, ok2 := a["name"].(string)
		if ok1 && ok2 {
			if r := store.getRepo(owner, name); r != nil {
				return r
			}
		}
		return nil
	}
	if n == "node" && parent == nil {
		return rq.gqlNode(a["id"])
	}
	if parent == nil {
		switch n {
		case "createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment":
			return rq.gqlMutation(n, a)
		}
		return nil
	}
	if n == "id" {
		if repo, isRepo := parent.(*Repo); isRepo {
			return gid("Repository", repo.id)
		}
		if dict, isDict := parent.(JObj); isDict {
			typ := "Issue"
			_, hasHead := dict["head"]
			_, hasBase := dict["base"]
			if hasHead && hasBase {
				typ = "PullRequest"
			}
			idVal, present := dict["id"]
			if !present {
				idVal = 0
			}
			return gidRaw(typ, pyStr(idVal))
		}
		return gid("Issue", 0)
	}
	if repo, isRepo := parent.(*Repo); isRepo {
		return rq.gqlRepoField(repo, n, a)
	}
	if dict, isDict := parent.(JObj); isDict {
		return rq.gqlDictField(dict, n, a)
	}
	return nil
}

func (rq *request) gqlMutation(n string, a JObj) interface{} {
	var inp JObj
	if raw, present := a["input"]; present {
		idict, isDict := raw.(JObj)
		if !isDict {
			panic(pyErr{"input has no attribute 'get'"})
		}
		inp = idict
	} else {
		inp = JObj{}
	}
	rid := inp["repositoryId"]
	repo := rq.gqlNode(rid)
	if repo == nil && n == "createIssue" {
		return JObj{"issue": nil}
	}
	switch n {
	case "createIssue":
		iid := store.nextIssue
		title := interface{}("")
		if tv, present := inp["title"]; present {
			title = tv
		}
		body := interface{}("")
		if bv, present := inp["body"]; present {
			body = bv
		}
		issue := JObj{"id": gid("Issue", iid), "number": len(repo.issues) + 1, "title": title, "body": body, "state": "OPEN"}
		store.nextIssue++
		obj := JObj{
			"id": iid, "number": issue["number"], "title": title, "body": body,
			"state": "open", "labels": []interface{}{}, "comments": []interface{}{},
			"created_at": store.Now(),
		}
		repo.issues = append(repo.issues, obj)
		repo.byNum[obj["number"].(int)] = obj
		return JObj{"issue": issue}
	case "closeIssue":
		target := inp["issueId"]
		tStr, tIsStr := target.(string)
		for _, r := range store.repoOrder {
			for _, it := range r.issues {
				idInt, _ := it["id"].(int)
				if (tIsStr && gid("Issue", idInt) == tStr) || pyStr(it["id"]) == pyStr(target) {
					it["state"] = "closed"
					return JObj{"issue": JObj{"id": target, "number": it["number"], "state": "CLOSED"}}
				}
			}
		}
	case "addComment":
		subject := inp["subjectId"]
		var subjectNum int
		hasNum := false
		if s, isStr := subject.(string); isStr {
			if dec, err := b64DecodeLoose(s + "==="); err == nil {
				if parts := strings.SplitN(string(dec), ":", 2); len(parts) == 2 {
					if num, err := strconv.Atoi(parts[1]); err == nil {
						subjectNum = num
						hasNum = true
					}
				}
			}
		}
		sStr, sIsStr := subject.(string)
		for _, r := range store.repoOrder {
			for _, it := range r.issues {
				idInt, _ := it["id"].(int)
				match := (sIsStr && gid("Issue", idInt) == sStr) ||
					(hasNum && pyStr(it["id"]) == itoa(subjectNum)) ||
					pyStr(it["id"]) == pyStr(subject)
				if match {
					body := interface{}("")
					if bv, present := inp["body"]; present {
						body = bv
					}
					c := JObj{"id": store.nextComment, "body": body}
					store.nextComment++
					comments, _ := it["comments"].([]interface{})
					it["comments"] = append(comments, c)
					return JObj{"commentEdge": JObj{"node": JObj{"id": gid("Comment", c["id"].(int)), "body": body}}}
				}
			}
		}
	case "createPullRequest":
		ridStr, ridIsStr := rid.(string)
		for _, r := range store.repoOrder {
			if ridIsStr && gid("Repository", r.id) == ridStr {
				headRaw := interface{}("")
				if hv, present := inp["headRefName"]; present {
					headRaw = hv
				}
				headStr, headIsStr := headRaw.(string)
				if !headIsStr {
					panic(pyErr{"headRefName has no attribute 'split'"})
				}
				head := headStr
				if i := strings.LastIndexByte(head, ':'); i >= 0 {
					head = head[i+1:]
				}
				base := interface{}(r.deflt)
				if bv, present := inp["baseRefName"]; present {
					base = bv
				}
				a2 := JObj{"title": inp["title"], "head": head, "base": base, "body": ""}
				rq.doPullCreate(r, a2)
				if len(r.pulls) == 0 {
					return JObj{"pullRequest": nil}
				}
				pr := r.pulls[len(r.pulls)-1]
				return JObj{"pullRequest": JObj{
					"number": pr["number"], "id": gid("PullRequest", pr["id"].(int)),
					"state": "OPEN", "headRefName": pr["head"].(JObj)["ref"],
					"baseRefName": pr["base"].(JObj)["ref"],
				}}
			}
		}
	case "mergePullRequest":
		target := inp["pullRequestId"]
		tStr, tIsStr := target.(string)
		for _, r := range store.repoOrder {
			for _, pr := range r.pulls {
				idInt, _ := pr["id"].(int)
				if tIsStr && gid("PullRequest", idInt) == tStr {
					base := pr["base"].(JObj)["ref"].(string)
					tip, okT := r.refs["refs/heads/"+base]
					head, okH := r.refs["refs/heads/"+pr["head"].(JObj)["ref"].(string)]
					if !okT || !okH {
						panic(pyErr{"KeyError: refs/heads/" + base})
					}
					files, conf := threeway(r, tip, head)
					if !conf {
						oid := r.commit(r.tree(files), []string{tip}, pr["title"].(string))
						r.setRef("refs/heads/"+base, oid)
						pr["state"] = "closed"
						pr["merged"] = true
						pr["merged_at"] = store.Now()
						pr["merge_commit_sha"] = oid
					}
					state := "OPEN"
					if merged, _ := pr["merged"].(bool); merged {
						state = "MERGED"
					}
					return JObj{"pullRequest": JObj{"number": pr["number"], "merged": pr["merged"], "state": state}}
				}
			}
		}
	}
	return nil
}

func (rq *request) gqlRepoField(repo *Repo, n string, a JObj) interface{} {
	switch n {
	case "databaseId":
		return repo.id
	case "name":
		return repo.name
	case "nameWithOwner":
		return repo.owner + "/" + repo.name
	case "owner":
		return JObj{"login": repo.owner}
	case "defaultBranchRef":
		if _, ok := repo.refs["refs/heads/"+repo.deflt]; ok {
			return JObj{"name": repo.deflt}
		}
		return nil
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "issue":
		if num, ok := intOf(a["number"]); ok {
			for _, x := range repo.issues {
				if x["number"] == num {
					return x
				}
			}
		}
		return nil
	case "issues":
		vals := append([]JObj{}, repo.issues...)
		sort.SliceStable(vals, func(i, j int) bool {
			ca, cb := vals[i]["created_at"].(string), vals[j]["created_at"].(string)
			if ca != cb {
				return ca > cb
			}
			return vals[i]["number"].(int) > vals[j]["number"].(int)
		})
		if states, present := a["states"]; present && pyTruthy(states) {
			list, _ := states.([]interface{})
			var kept []JObj
			for _, x := range vals {
				up := strings.ToUpper(x["state"].(string))
				for _, s := range list {
					if s == up {
						kept = append(kept, x)
						break
					}
				}
			}
			vals = kept
		}
		first := gqlFirst(a)
		start := 0
		if av, present := a["after"]; present && pyTruthy(av) {
			v, ok := intOf(av)
			if !ok {
				panic(pyErr{"invalid literal for int()"})
			}
			start = v
		}
		var sliced []JObj
		if start < 0 {
			start = 0
		}
		if start <= len(vals) {
			end := start + first
			if end > len(vals) {
				end = len(vals)
			}
			if end < start {
				end = start
			}
			sliced = vals[start:end]
		}
		nodes := []interface{}{}
		for _, x := range sliced {
			nodes = append(nodes, x)
		}
		var cursor interface{}
		if len(sliced) > 0 {
			cursor = itoa(start + len(sliced))
		}
		return JObj{"nodes": nodes, "pageInfo": JObj{"hasNextPage": start + first < len(repo.issues), "endCursor": cursor}}
	case "pullRequests":
		vals := append([]JObj{}, repo.pulls...)
		if states, present := a["states"]; present && pyTruthy(states) {
			list, _ := states.([]interface{})
			var kept []JObj
			for _, x := range vals {
				up := strings.ToUpper(x["state"].(string))
				for _, s := range list {
					if s == up {
						kept = append(kept, x)
						break
					}
				}
			}
			vals = kept
		}
		first := gqlFirst(a)
		if first < len(vals) {
			vals = vals[:first]
		}
		nodes := []interface{}{}
		for _, x := range vals {
			nodes = append(nodes, x)
		}
		return JObj{"nodes": nodes}
	case "pullRequest":
		if num, ok := intOf(a["number"]); ok {
			for _, x := range repo.pulls {
				if x["number"] == num {
					return x
				}
			}
		}
		return nil
	}
	return nil
}

// gqlFirst reads the 'first' argument the way a.get('first', 100) plus a
// following slice would: an explicit null or non-integer is an error.
func gqlFirst(a JObj) int {
	fv, present := a["first"]
	if !present {
		return 100
	}
	n, ok := intOf(fv)
	if !ok {
		panic(pyErr{"argument of type is not iterable"})
	}
	return n
}

func (rq *request) gqlDictField(dict JObj, n string, a JObj) interface{} {
	switch n {
	case "files":
		raw, _ := dict["files"].([]interface{})
		first := gqlFirst(a)
		if first < len(raw) {
			raw = raw[:first]
		}
		nodes := []interface{}{}
		for _, x := range raw {
			xd, _ := x.(JObj)
			nodes = append(nodes, JObj{"path": xd["filename"]})
		}
		return JObj{"nodes": nodes}
	case "state":
		v, present := dict["state"]
		if !present {
			return ""
		}
		s, isStr := v.(string)
		if !isStr {
			panic(pyErr{"state has no attribute 'upper'"})
		}
		return strings.ToUpper(s)
	case "merged":
		if v, present := dict["merged"]; present {
			return v
		}
		return false
	case "headRefName":
		if v, present := dict["headRefName"]; present {
			return v
		}
		return refOf(dict["head"])
	case "baseRefName":
		if v, present := dict["baseRefName"]; present {
			return v
		}
		return refOf(dict["base"])
	case "mergeable":
		if pyTruthy(dict["mergeable"]) {
			return "MERGEABLE"
		}
		return "CONFLICTING"
	case "databaseId":
		return dict["id"]
	}
	return dict[n]
}

func refOf(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	d, isDict := v.(JObj)
	if !isDict {
		panic(pyErr{"head/base has no attribute 'get'"})
	}
	return d["ref"]
}
