package main

// mcp.go — the MCP endpoint at /mcp: JSON-RPC with server/discover,
// initialize, tools/list and tools/call over six tools. A faithful port of
// the Python mcp()/tool_list()/tool_call(), including its error shapes:
// unknown tools answer -32602, tool failures answer isError text results,
// and successful calls carry both a text content and structuredContent.

import (
	"strings"
)

type mcpToolSpec struct {
	name  string
	props []string
}

var mcpToolSpecs = []mcpToolSpec{
	{"create_repo", []string{"name"}},
	{"get_repo", []string{"owner", "repo"}},
	{"create_issue", []string{"owner", "repo", "title"}},
	{"list_issues", []string{"owner", "repo"}},
	{"create_pull_request", []string{"owner", "repo", "title", "head", "base"}},
	{"merge_pull_request", []string{"owner", "repo", "pull_number"}},
}

func (rq *request) mcp() {
	v, err := parseJSON(rq.bodyBytes())
	if err != nil {
		rq.sendJSON(400, JObj{"jsonrpc": "2.0", "id": nil, "error": JObj{"code": -32700, "message": "Parse error"}})
		return
	}
	d, isObj := v.(JObj)
	if !isObj {
		panic(pyErr{"object has no attribute 'get'"})
	}
	ident := d["id"]
	method, _ := d["method"].(string)
	paramsRaw, hasParams := d["params"]
	var p JObj
	paramsBad := false
	if !hasParams {
		p = JObj{}
	} else if pd, ok := paramsRaw.(JObj); ok {
		p = pd
	} else {
		paramsBad = true
	}
	params := func() JObj {
		if paramsBad {
			panic(pyErr{"params has no attribute 'get'"})
		}
		return p
	}
	var res interface{}
	var rpcErr JObj
	switch method {
	case "server/discover":
		res = JObj{
			"supportedVersions": []interface{}{"2026-07-28"},
			"serverInfo":          JObj{"name": "python-forge", "version": "1.0.0"},
			"capabilities":        JObj{"tools": JObj{}},
			"protocolVersion":     "2026-07-28",
		}
	case "initialize":
		pv := interface{}("2025-11-25")
		if val, present := params()["protocolVersion"]; present {
			pv = val
		}
		res = JObj{
			"protocolVersion": pv,
			"serverInfo":      JObj{"name": "python-forge", "version": "1.0.0"},
			"capabilities":    JObj{"tools": JObj{}},
		}
	case "tools/list":
		res = JObj{"tools": mcpToolList()}
	case "notifications/initialized":
		rq.sendJSON(202, nil)
		return
	case "tools/call":
		pp := params()
		nameRaw, _ := pp["name"].(string)
		if nv, present := pp["name"]; present {
			if _, isStr := nv.(string); !isStr {
				panic(pyErr{"name has no attribute 'split'"})
			}
			nameRaw = nv.(string)
		}
		parts := strings.Split(nameRaw, "__")
		name := strings.ToLower(parts[len(parts)-1])
		var a JObj
		if av, present := pp["arguments"]; present {
			ad, isDict := av.(JObj)
			if !isDict {
				panic(pyErr{"arguments has no attribute 'get'"})
			}
			a = ad
		} else {
			a = JObj{}
		}
		result, isErr, errText, handled := rq.mcpToolCall(name, a)
		switch {
		case !handled:
			rpcErr = JObj{"code": -32602, "message": "Unknown tool or missing arguments"}
		case isErr:
			res = JObj{"isError": true, "content": []interface{}{JObj{"type": "text", "text": errText}}}
		default:
			res = JObj{
				"content":           []interface{}{JObj{"type": "text", "text": string(marshalJSON(result))}},
				"structuredContent": result,
				"isError":           false,
			}
		}
	default:
		rpcErr = JObj{"code": -32601, "message": "Method not found"}
	}
	out := JObj{"jsonrpc": "2.0", "id": ident}
	if rpcErr != nil {
		out["error"] = rpcErr
	} else {
		out["result"] = res
	}
	rq.sendJSON(200, out)
}

func mcpToolList() []interface{} {
	out := []interface{}{}
	for _, spec := range mcpToolSpecs {
		props := JObj{}
		for _, k := range spec.props {
			props[k] = JObj{"type": "string"}
		}
		req := []interface{}{}
		for _, k := range spec.props {
			req = append(req, k)
		}
		out = append(out, JObj{
			"name":        spec.name,
			"description": "GitHub " + strings.ReplaceAll(spec.name, "_", " "),
			"inputSchema": JObj{"type": "object", "properties": props, "required": req},
		})
	}
	return out
}

// mcpToolCall returns (result, isErrorTuple, errorText, handled). handled is
// false where the Python tool_call returns None.
func (rq *request) mcpToolCall(n string, a JObj) (interface{}, bool, string, bool) {
	if n == "create_repo" && pyTruthy(a["name"]) {
		name, isStr := a["name"].(string)
		if !isStr {
			panic(pyErr{"name is not a string"})
		}
		return rq.repoJSON(store.create("adaiversen", name)), false, "", true
	}
	switch n {
	case "get_repo", "create_issue", "list_issues", "create_pull_request", "merge_pull_request":
		owner, ok1 := a["owner"].(string)
		repoName, ok2 := a["repo"].(string)
		var r *Repo
		if ok1 && ok2 {
			r = store.getRepo(owner, repoName)
		}
		if r == nil {
			return nil, true, "Repository not found", true
		}
		switch n {
		case "get_repo":
			return rq.repoJSON(r), false, "", true
		case "create_issue":
			if pyTruthy(a["title"]) {
				issue := JObj{
					"id": store.nextIssue, "number": len(r.issues) + 1,
					"title": a["title"], "body": a["body"], "state": "open",
					"labels": []interface{}{}, "comments": []interface{}{},
					"created_at": store.Now(),
				}
				if _, present := a["body"]; !present {
					issue["body"] = ""
				}
				store.nextIssue++
				r.issues = append(r.issues, issue)
				return issue, false, "", true
			}
		case "list_issues":
			out := []interface{}{}
			for _, x := range r.issues {
				out = append(out, x)
			}
			return out, false, "", true
		case "create_pull_request":
			if pyTruthy(a["title"]) && pyTruthy(a["head"]) && pyTruthy(a["base"]) {
				d := JObj{"title": a["title"], "head": a["head"], "base": a["base"], "body": ""}
				rq.doPullCreate(r, d)
				if len(r.pulls) == 0 {
					return nil, true, "Invalid branches", true
				}
				return r.pulls[len(r.pulls)-1], false, "", true
			}
		case "merge_pull_request":
			var num int
			if nv, present := a["pull_number"]; present {
				v, ok := intOf(nv)
				if !ok {
					panic(pyErr{"pull_number is not an int"})
				}
				num = v
			}
			var pr JObj
			for _, x := range r.pulls {
				if x["number"] == num {
					pr = x
					break
				}
			}
			if pr == nil {
				return nil, true, "Pull request not found", true
			}
			base := pr["base"].(JObj)["ref"].(string)
			tip := r.refs["refs/heads/"+base]
			head := r.refs["refs/heads/"+pr["head"].(JObj)["ref"].(string)]
			merged, conf := threeway(r, tip, head)
			if conf {
				return nil, true, "Conflicting changes", true
			}
			oid := r.commit(r.tree(merged), []string{tip}, pr["title"].(string))
			r.setRef("refs/heads/"+base, oid)
			pr["state"] = "closed"
			pr["merged"] = true
			pr["merged_at"] = store.Now()
			pr["merge_commit_sha"] = oid
			return JObj{"number": pr["number"], "merged": true, "sha": oid}, false, "", true
		}
	}
	return nil, false, "", false
}
