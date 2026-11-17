import os,sys,json,threading,time,urllib.request,urllib.error,base64,hashlib,zlib,struct,re,urllib.parse,socket
from http.server import ThreadingHTTPServer,BaseHTTPRequestHandler
from store import store,Repo,ZERO,USERS

def pkt(data=b''): return f'{len(data)+4:04x}'.encode()+data
def flush():return b'0000'
def unpkt(b):
 out=[];i=0
 while i+4<=len(b):
  try:n=int(b[i:i+4],16)
  except:break
  i+=4
  if n==0:out.append(None);continue
  if n in (1,2,3):out.append(('special',n));continue
  if n<4 or i+n-4>len(b):break
  out.append(b[i:i+n-4]);i+=n-4
 return out,i
def readpack(data,repo):
 if len(data)<32 or data[:4]!=b'PACK':return
 b=data[:-20]; count=struct.unpack('>I',b[8:12])[0]; pos=12; pending=[]
 for _ in range(count):
  c=b[pos];pos+=1; typ=(c>>4)&7; size=c&15; shift=4
  while c&128:c=b[pos];pos+=1;size|=(c&127)<<shift;shift+=7
  base=None
  if typ==6:
   c=b[pos];pos+=1;dist=c&127
   while c&128:c=b[pos];pos+=1;dist=((dist+1)<<7)|(c&127)
   base='offset:'+str(pos-dist)
  elif typ==7:base=b[pos:pos+20].hex();pos+=20
  dec=zlib.decompressobj(); content=dec.decompress(b[pos:]); pos+=len(b[pos:])-len(dec.unused_data)
  typname={1:'commit',2:'tree',3:'blob',4:'tag'}.get(typ)
  if typname: pending.append((pos-len(content),typname,content,None))
  else:pending.append((pos-len(content),'delta',content,base))
  # offsets are merely identifiers for ofs-delta bases
  pending[-1]=(pos-len(content),pending[-1][1],content,base)
 for _ in range(count+1):
  rest=[]
  for off,typ,data,base in pending:
   if typ!='delta':repo.put(typ,data);continue
   oid=None
   if base and base.startswith('offset:'):
    target=int(base[7:]); oid=next((o for o in repo.objects if False),None)
   if base and not base.startswith('offset:'):oid=base
   if oid not in repo.objects:rest.append((off,typ,data,base));continue
   t,src=repo.get(oid); out=apply_delta(src,data);repo.put(t,out)
  pending=rest
  if not pending:break
def apply_delta(src,d):
 def var(buf,i):
  n=0;s=0
  while 1:
   c=buf[i];i+=1;n|=(c&127)<<s;s+=7
   if c<128:return n,i
 a,i=var(d,0); size,i=var(d,i); out=bytearray()
 while i<len(d):
  op=d[i];i+=1
  if op&128:
   off=ln=0
   for bit,sh in [(1,0),(2,8),(4,16),(8,24)]:
    if op&bit:off|=d[i]<<sh;i+=1
   for bit,sh in [(16,0),(32,8),(64,16)]:
    if op&bit:ln|=d[i]<<sh;i+=1
   out+=src[off:off+(ln or 65536)]
  elif op:out+=d[i:i+op];i+=op
 return bytes(out)
def makepack(repo, tips, depth=None, exclude=(), shallow=(), deepen=False, since=None, stop_commits=()):
 # Determine the commit set once, then walk its object closure once. This avoids a full
 # history walk for every tree and keeps shallow clone work linear in the transferred graph.
 cachekey=(tuple(tips),depth,tuple(sorted(exclude)),tuple(sorted(shallow)),bool(deepen),since,tuple(sorted(stop_commits)))
 cached=repo._pack_cache.get(cachekey)
 if cached is not None:
  if depth is not None or since is not None or stop_commits:repo._shallow_boundaries=list(cached[1])
  return cached[0]
 excluded=set(exclude); old_shallow=set(shallow); stopped=set(stop_commits); levels={}; extra_roots=[]; todo=[(x,0) for x in tips]
 while todo:
  oid,level=todo.pop(0)
  if oid in levels or (oid in excluded and oid not in old_shallow):continue
  obj=repo.get(oid)
  if not obj:continue
  if obj[0]=='tag':extra_roots.append(oid);continue
  if obj[0]!='commit':continue
  if oid in stopped:continue
  if since is not None:
   tm=re.search(rb'(?m)^committer .* (\d+) [+-]\d{4}$',obj[1])
   if tm and int(tm.group(1))<since:continue
  levels[oid]=level
  parents=repo.parents(oid)
  if depth is None or level+1<depth or deepen and oid in old_shallow:
   todo.extend((p,level+1) for p in parents)
 objs={}; boundaries=[]
 for oid,level in levels.items():
  data=repo.get(oid)[1]; mt=re.search(rb'(?m)^tree ([0-9a-f]{40})$',data)
  if mt:todo.append(mt.group(1).decode())
  if oid not in excluded:objs[oid]=repo.get(oid)
 todo.extend(extra_roots)
 todo=[x for x in todo if x not in levels and x not in excluded]
 seen=set(objs)
 while todo:
  oid=todo.pop()
  if oid in seen or (oid in excluded and oid not in old_shallow):continue
  seen.add(oid);obj=repo.get(oid)
  if not obj:continue
  typ,data=obj;objs[oid]=obj
  if typ=='tree':
   i=0
   while i<len(data):
    j=data.index(b'\0',i);mode,name=data[i:j].split(b' ',1);child=data[j+1:j+21].hex();i=j+21
    if child not in seen:todo.append(child)
  elif typ=='tag':
   mt=re.search(rb'(?m)^object ([0-9a-f]{40})$',data)
   if mt:todo.append(mt.group(1).decode())
 if depth is not None or since is not None or stopped:
  boundaries=[oid for oid in levels if any(p not in levels for p in repo.parents(oid))]
  repo._shallow_boundaries=boundaries
 else:repo._shallow_boundaries=[]
 out=bytearray(b'PACK'+struct.pack('>II',2,len(objs)))
 typnum={'commit':1,'tree':2,'blob':3,'tag':4}
 for typ,data in objs.values():
  n=len(data); c=typnum[typ]<<4|(n&15);n>>=4; hdr=bytearray([c])
  while n:hdr[-1]|=128;hdr.append((n&127)|(128 if n>>7 else 0));n>>=7
  out+=hdr+zlib.compress(data)
 out+=hashlib.sha1(out).digest();pack=bytes(out)
 if len(repo._pack_cache)>=32:repo._pack_cache.pop(next(iter(repo._pack_cache)))
 repo._pack_cache[cachekey]=(pack,list(getattr(repo,'_shallow_boundaries',[])) if depth is not None or since is not None or stop_commits else [])
 return pack

class H(BaseHTTPRequestHandler):
 protocol_version='HTTP/1.1'; server_version='Forge/1'
 def setup(self):
  super().setup()
  try:self.connection.setsockopt(socket.IPPROTO_TCP,socket.TCP_NODELAY,1)
  except OSError:pass
 def log_message(self,*a):pass
 def body(self):
  if hasattr(self,'_body_cache'):return self._body_cache
  n=int(self.headers.get('Content-Length','0'));self._body_cache=self.rfile.read(min(n,100_000_000));return self._body_cache
 def send(self,status,obj=None,ctype='application/json',raw=None,headers=None):
  b=raw if raw is not None else (b'' if obj is None else json.dumps(obj,separators=(',',':')).encode())
  self.send_response(status);self.send_header('Content-Type',ctype);self.send_header('Content-Length',str(len(b)));self.send_header('X-GitHub-Request-Id',format(time.time_ns(),'x'))
  if headers:
   for k,v in headers.items():self.send_header(k,v)
  self.end_headers();self.wfile.write(b)
 def auth(self):return self.headers.get('Authorization')=='Bearer '+os.getenv('MAKEFUN_SHOP_TOKEN','dev-token')
 def do_GET(self):self.route()
 def do_POST(self):self.route()
 def do_PUT(self):self.route()
 def do_PATCH(self):self.route()
 def do_DELETE(self):self.route()
 def route(self):
  try:
   if hasattr(self,'_body_cache'):del self._body_cache
   if self.command in ('POST','PUT','PATCH','DELETE'):self.body()
   p=urllib.parse.urlsplit(self.path); path=urllib.parse.unquote(p.path); q=urllib.parse.parse_qs(p.query); method=self.command
   if path=='/healthz':return self.send(200,{'ok':True})
   if path.startswith('/api/') or path=='/mcp':
    if not self.auth():
     self.body();return self.send(401,{'message':'Bad credentials'})
   if path=='/mcp':return self.mcp()
   if path=='/api/graphql':return self.graphql()
   if path.startswith('/api/v3/'):return self.rest(path[7:],q)
   m=re.match(r'^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$',path)
   if m:return self.git(m.group(1),m.group(2),m.group(3),q)
   return self.send(404,{'message':'Not Found'})
  except Exception as e:
   try:self.send(500,{'message':'Internal Server Error','error':str(e)[:300]})
   except:pass
 def git(self,o,n,action,q):
  r=store.repos.get((o,n))
  if not r:return self.send(404,{'message':'Not Found'})
  service=q.get('service',[''])[0]
  if action=='info/refs':
   recv=service=='git-receive-pack'; typ='receive-pack' if recv else 'upload-pack'; ct='application/x-git-'+typ+'-advertisement'; cap='report-status side-band-64k delete-refs ofs-delta' if recv else 'multi_ack_detailed side-band-64k ofs-delta shallow deepen-since deepen-not deepen-relative no-progress include-tag ofs-delta agent=forge'
   if not recv and self.headers.get('Git-Protocol')=='version=2':return self.send(200,None,ct,raw=pkt(b'version 2\n')+pkt(b'agent=forge\n')+pkt(b'ls-refs=unborn\n')+pkt(b'fetch=shallow wait-for-done\n')+pkt(b'object-format=sha1\n')+flush())
   b=pkt(f'# service=git-{typ}\n'.encode())+flush(); refs=sorted(r.refs.items()); head=r.refs.get('refs/heads/'+r.default)
   if not refs:b+=pkt((ZERO+' capabilities^{}\0'+cap+' agent=git/2.43.0\n').encode())
   else:
    for i,(ref,oid) in enumerate(refs):b+=pkt((oid+' '+ref+('\0'+cap if i==0 else '')+'\n').encode())
    if head:b+=pkt((head+' HEAD\n').encode())
   return self.send(200,None,ct,raw=b+flush())
  body=self.body(); packets,idx=unpkt(body)
  if action=='git-upload-pack':
   if packets and isinstance(packets[0],bytes) and packets[0].startswith(b'command=ls-refs'):
    args=[x.decode(errors='ignore').strip() for x in packets if isinstance(x,bytes)]; prefixes=[x[11:] for x in args if x.startswith('ref-prefix ')]
    out=b''
    refs=sorted(r.refs.items())
    head=r.refs.get('refs/heads/'+r.default)
    if head and (not prefixes or any('HEAD'.startswith(x) for x in prefixes)):out+=pkt((head+' HEAD symref-target:refs/heads/'+r.default+'\n').encode())
    for ref,oid in refs:
     if not prefixes or any(ref.startswith(x) for x in prefixes):
      line=oid+' '+ref
      ob=r.get(oid)
      if ref.startswith('refs/tags/') and ob and ob[0]=='tag':
       mt=re.search(rb'object ([0-9a-f]{40})',ob[1]);
       if mt:line+=' peeled:'+mt.group(1).decode()
      out+=pkt((line+'\n').encode())
    return self.send(200,None,'application/x-git-upload-pack-result',raw=out+flush())
   if packets and isinstance(packets[0],bytes) and packets[0].startswith(b'command=fetch'):
    args=[x.decode(errors='ignore').strip() for x in packets if isinstance(x,bytes)]
    wants=[x[5:] for x in args if x.startswith('want ')]; haves=[x[5:] for x in args if x.startswith('have ')]
    boundaries=[x[8:] for x in args if x.startswith('shallow ')]; depth=next((int(x[7:]) for x in args if x.startswith('deepen ')),None)
    deepen_since=next((int(x[13:]) for x in args if x.startswith('deepen-since ')),None)
    deepen_not={x[11:] for x in args if x.startswith('deepen-not ')}
    # A client that already has the wanted tip needs an empty, checksummed pack.
    common=set(haves); have_closure=set()
    for h in haves:
     todo=[h]
     while todo:
      oid=todo.pop()
      if oid in have_closure:continue
      have_closure.add(oid);obj=r.get(oid)
      if not obj:continue
      typ,data=obj
      if typ=='commit':
       mt=re.search(rb'(?m)^tree ([0-9a-f]{40})$',data)
       if mt:todo.append(mt.group(1).decode())
       if oid not in boundaries:todo.extend(r.parents(oid))
      elif typ=='tree':
       i=0
       while i<len(data):
        j=data.index(b'\0',i);child=data[j+1:j+21].hex();i=j+21;todo.append(child)
      elif typ=='tag':
       mt=re.search(rb'(?m)^object ([0-9a-f]{40})$',data)
       if mt:todo.append(mt.group(1).decode())
    new_boundaries=[]; unshallow=[]
    stop_commits=set()
    for ref in deepen_not:
     ref_oid=r.refs.get(ref) or r.refs.get('refs/heads/'+ref) or r.refs.get('refs/tags/'+ref)
     if ref_oid:stop_commits.update(oid for oid,_ in r.walk_commits(ref_oid))
    if wants:
     if depth is not None:
      pack=makepack(r,wants[:1],depth=depth,exclude=have_closure,shallow=boundaries,deepen=bool(boundaries),since=deepen_since,stop_commits=stop_commits)
      new_boundaries=list(getattr(r,'_shallow_boundaries',[]))
      unshallow=[x for x in boundaries if x not in new_boundaries]
     else:
      pack=makepack(r,wants,exclude=have_closure,since=deepen_since,stop_commits=stop_commits)
      new_boundaries=list(getattr(r,'_shallow_boundaries',[])) if deepen_since is not None or stop_commits else []
      unshallow=[x for x in boundaries if x not in new_boundaries]
    else:pack=makepack(r,[],exclude=have_closure)
    acklines=[]
    for h in haves:
     if r.get(h):acklines.append(pkt(('ACK '+h+' common\n').encode()))
    acklines.append(pkt(b'ready\n' if haves else b'NAK\n'))
    out=pkt(b'acknowledgments\n')+b''.join(acklines)+flush()+pkt(b'shallow-info\n')
    for oid in unshallow:out+=pkt(('unshallow '+oid+'\n').encode())
    for oid in new_boundaries:out+=pkt(('shallow '+oid+'\n').encode())
    out+=flush()+pkt(b'wanted-refs\n')+flush()+pkt(b'packfile\n')
    for i in range(0,len(pack),65515):out+=pkt(b'\x01'+pack[i:i+65515])
    return self.send(200,None,'application/x-git-upload-pack-result',raw=out+flush())
   wants=[x.decode(errors='ignore').split(' ',2)[1].split('\0')[0].strip() for x in packets if isinstance(x,bytes) and x.startswith(b'want ')]
   pack=makepack(r,wants) if wants else b''
   framed=pkt(b'\x01'+pack) if pack else b''
   return self.send(200,None,'application/x-git-upload-pack-result',raw=pkt(b'NAK\n')+framed+flush())
   return self.send(200,None,'application/x-git-upload-pack-result',raw=pkt(b'NAK\n')+flush())
  # receive-pack v0 command section followed by pack
  parsed,split=unpkt(body); packdata=body[split:]
  cmds=[]
  for x in parsed:
   if x and isinstance(x,bytes):
    s=x.decode(errors='ignore').strip().split(' ')
    if len(s)>=3:cmds.append((s[0],s[1],s[2].split('\0',1)[0]))
  if packdata.startswith(b'PACK'):readpack(packdata,r)
  statuses=[]
  for old,new,ref in cmds:
   ref=ref.split('\0',1)[0].split(' ',1)[0]
   current=r.refs.get(ref,ZERO)
   if old!=current: statuses.append(b'ng '+ref.encode()+b' stale info\n');continue
   if new==ZERO:r.refs.pop(ref,None);statuses.append(b'ok '+ref.encode()+b'\n');continue
   if old!=ZERO and not ancestor(r,old,new):statuses.append(b'ng '+ref.encode()+b' non-fast-forward\n');continue
   r.refs[ref]=new
   if ref.startswith('refs/heads/') and not current.startswith('0'):r.default=r.default or ref.split('/')[-1]
   if ref.startswith('refs/heads/') and len([x for x in r.refs if x.startswith('refs/heads/')])==1:r.default=ref.split('/')[-1]
   statuses.append(b'ok '+ref.encode()+b'\n')
  report=pkt(b'unpack ok\n')+b''.join(pkt(x) for x in statuses)+flush()
  out=pkt(b'\x01'+report)+flush()
  return self.send(200,None,'application/x-git-receive-pack-result',raw=out)

 def rest(self,path,q):
  method=self.command; parts=path.strip('/').split('/') if path.strip('/') else []
  def ok(code,x,headers=None):return self.send(code,x,headers=headers)
  def err(code,msg):return self.send(code,{'message':msg})
  def body():
   try:return json.loads(self.body() or b'{}')
   except:return None
  def repo_at(i=1):
   if len(parts)<=i+1:return None
   o,n=parts[i],parts[i+1];r=store.repos.get((o,n));return r if o=='adaiversen' else None
  if parts==[]:return ok(200,{'current_user_url':'/user','user_url':'/users/{user}','repository_url':'/repos/{owner}/{repo}'},{'X-OAuth-Scopes':'repo, read:org'})
  if parts==['meta']:return ok(200,{'installed_version':'1.0.0'})
  if parts==['user']:return ok(200,{'login':'adaiversen','id':1000,'name':'ada iversen','email':'adaiversen@example.invalid','type':'User'})
  if parts==['user','repos'] and method=='POST':
   d=body();
   if d is None:return err(400,'Invalid JSON')
   r=store.create('adaiversen',d.get('name',''));r.private=d.get('private',False);r.description=d.get('description','');return ok(201,self.repo_json(r))
  if parts==['user','repos'] and method=='GET':
   rs=[r for (o,_),r in store.repos.items() if o=='adaiversen']; per=int(q.get('per_page',['30'])[0]);page=int(q.get('page',['1'])[0]); vals=rs[(page-1)*per:page*per]; hdr={}
   if page*per<len(rs):hdr['Link']=f'<http://{self.headers.get("Host","localhost")}{self.path.split("?")[0]}?per_page={per}&page={page+1}>; rel="next"'
   return ok(200,[self.repo_json(r) for r in vals],hdr)
  r=repo_at()
  if len(parts)>=3 and parts[0]=='repos' and r is None:return err(404,'Not Found')
  if len(parts)==3 and parts[0]=='repos':
   if method=='GET':return ok(200,self.repo_json(r))
   if method=='PATCH':
    d=body();
    if d is None:return err(400,'Invalid JSON')
    r.description=d.get('description',r.description);return ok(200,self.repo_json(r))
   if method=='DELETE':store.repos.pop((r.owner,r.name),None);return self.send(204,None)
  return self.rest_repo(r,parts[3:],q,body,ok,err)

 def repo_json(self,r):return {'id':r.id,'node_id':self.gid('Repository',r.id),'name':r.name,'full_name':r.owner+'/'+r.name,'private':r.private,'owner':{'login':r.owner,'id':1000,'type':'User'},'description':r.description,'default_branch':r.default,'has_issues':True,'permissions':{'admin':True,'push':True,'pull':True},'created_at':store.now(),'updated_at':store.now()}
 def gid(self,t,n):return base64.b64encode(f'{t}:{n}'.encode()).decode().rstrip('=')
 def rest_repo(self,r,p,q,body,ok,err):
  m=self.command
  if p==['branches']:
   return ok(200,[{'name':x.split('/')[-1],'commit':{'sha':v}} for x,v in r.refs.items() if x.startswith('refs/heads/')])
  if p and p[0]=='branches' and len(p)==2:
   oid=r.refs.get('refs/heads/'+p[1]);return ok(200,{'name':p[1],'commit':{'sha':oid}}) if oid else err(404,'Not Found')
  if p==['commits']:
   tip=r.refs.get('refs/heads/'+q.get('sha',[r.default])[0]); out=[]
   if tip:
    for oid,data in r.walk_commits(tip):out.append({'sha':oid,'commit':{'message':data.split(b'\n\n',1)[-1].decode(errors='replace').rstrip()}})
   return ok(200,out)
  if p[:2]==['git','refs']:
   if m=='POST' and len(p)==2:
    d=body();ref=d.get('ref');oid=d.get('sha');r.refs[ref]=oid;return ok(201,{'ref':ref,'object':{'sha':oid}})
   ref='refs/'+ '/'.join(p[2:]);oid=r.refs.get(ref)
   return ok(200,{'ref':ref,'object':{'sha':oid}}) if oid else err(404,'Not Found')
  if p and p[0]=='contents':
   path='/'.join(p[1:]);d=None
   if m!='GET':
    d=body()
    if d is None:return err(400,'Invalid JSON')
   branch=q.get('ref',[d.get('branch',r.default) if d else r.default])[0];tip=r.refs.get('refs/heads/'+branch);files=r.files_at(tip) if tip else {}
   if m=='GET':
    if path in files:
     oid=files[path];data=r.get(oid)[1];return ok(200,{'name':path.split('/')[-1],'path':path,'sha':oid,'size':len(data),'type':'file','encoding':'base64','content':base64.b64encode(data).decode()})
    prefix=path.rstrip('/')+'/' if path else ''; names=sorted({x[len(prefix):].split('/')[0] for x in files if x.startswith(prefix)})
    if names:return ok(200,[{'name':x,'path':prefix+x,'type':'dir' if any(y.startswith(prefix+x+'/') for y in files) else 'file','sha':files.get(prefix+x,'')} for x in names])
    return err(404,'Not Found')
   if m=='PUT':
    if 'sha' in d and files.get(path)!=d['sha']:return err(409,'sha does not match')
    data=base64.b64decode(d.get('content',''));blob=r.put('blob',data);files[path]=blob;tree=r.tree(files);parent=tip;oid=r.commit(tree,[parent] if parent else [],d.get('message','update'));branch=d.get('branch',branch);r.refs['refs/heads/'+branch]=oid
    return ok(200 if 'sha' in d else 201,{'content':{'sha':blob,'encoding':'base64'},'commit':{'sha':oid}})
   if m=='DELETE':
    if path not in files:return err(404,'Not Found')
    del files[path];tree=r.tree(files);oid=r.commit(tree,[tip] if tip else [],d.get('message','delete'));r.refs['refs/heads/'+r.default]=oid;return ok(200,{'commit':{'sha':oid}})
  if p[:1]==['labels']:
   if len(p)==1 and m=='POST':
    d=body();item={'id':len(r.labels)+1,'name':d['name'],'color':d.get('color','ededed')};r.labels.append(item);return ok(201,item)
   if len(p)==1:return ok(200,r.labels)
   if m=='DELETE':r.labels=[x for x in r.labels if x['name']!=p[1]];return self.send(204,None)
  if p[:1]==['issues']:
   if len(p)==1 and m=='POST':
    d=body();labels=d.get('labels',[]);names=[x['name'] for x in r.labels]
    if any((x.get('name') if isinstance(x,dict) else x) not in names for x in labels):return err(422,'Validation Failed')
    isue={'id':store.nextissue,'number':len(r.issues)+1,'title':d.get('title',''),'body':d.get('body',''),'state':'open','labels':[x for x in r.labels if x['name'] in labels or x in labels],'comments':[],'created_at':store.now(),'updated_at':store.now()};store.nextissue+=1;r.issues.append(isue);r.issue_by_number[isue['number']]=isue;return ok(201,isue)
   if len(p)==1:
    vals=r.issues[:];state=q.get('state',['open'])[0];vals=[x for x in vals if state=='all' or x['state']==state]
    vals.sort(key=lambda x:(x['created_at'],x['id']),reverse=q.get('direction',['asc'])[0]=='desc');return ok(200,vals)
   num=int(p[1]);i=r.issue_by_number.get(num) or next((x for x in r.issues if x['number']==num),None)
   if not i:return err(404,'Not Found')
   if len(p)==2 and m=='GET':return ok(200,i)
   if len(p)==2 and m=='PATCH':
    d=body();i.update(d);i['updated_at']=store.now();return ok(200,i)
   if p[-1]=='comments':
    if m=='POST':d=body();c={'id':store.nextcomment,'body':d.get('body','')};store.nextcomment+=1;i['comments'].append(c);return ok(201,c)
    return ok(200,i['comments'])
  if p[:1]==['pulls']:return self.pulls(r,p[1:],q,body,ok,err)
  return err(404,'Not Found')
 def pulls(self,r,p,q,body,ok,err):
  m=self.command
  if not p and m=='POST':
   d=body(); head=d.get('head','').split(':')[-1];base=d.get('base',r.default);ho=r.refs.get('refs/heads/'+head);bo=r.refs.get('refs/heads/'+base)
   if not ho or not bo:return err(422,'head or base ref not found')
   files=r.files_at(ho); common=r.files_at(bo); changed=[{'filename':k,'status':'added' if k not in common else 'modified','additions':1,'deletions':0,'changes':1} for k in files if files.get(k)!=common.get(k)]+[{'filename':k,'status':'removed','additions':0,'deletions':1,'changes':1} for k in common if k not in files]
   pr={'id':store.nextpull,'number':len(r.pulls)+1,'title':d.get('title',''),'body':d.get('body',''),'state':'open','merged':False,'head':{'ref':head,'sha':ho},'base':{'ref':base,'sha':bo},'user':{'login':'adaiversen'},'mergeable':True,'mergeable_state':'clean','changed_files':len(changed),'files':changed,'merged_at':None};store.nextpull+=1;r.pulls.append(pr);return ok(201,pr)
  if not p:
   state=q.get('state',['open'])[0];return ok(200,[x for x in r.pulls if state=='all' or x['state']==state])
  try:num=int(p[0])
  except:return err(404,'Not Found')
  pr=next((x for x in r.pulls if x['number']==num),None)
  if not pr:return err(404,'Not Found')
  if len(p)==1:return ok(200,pr)
  if p[1]=='files':return ok(200,pr['files'])
  if p[1]=='merge' and m=='PUT':
   base=pr['base']['ref'];tip=r.refs.get('refs/heads/'+base); head=r.refs.get('refs/heads/'+pr['head']['ref'])
   if not tip or not head:return err(405,'Pull request is not mergeable')
   merged,conf=threeway(r,tip,head)
   if conf:return err(405,'Pull request is not mergeable')
   tree=r.tree(merged);oid=r.commit(tree,[tip],pr['title']+' (#'+str(num)+')');r.refs['refs/heads/'+base]=oid;pr.update(state='closed',merged=True,merged_at=store.now(),merge_commit_sha=oid);return ok(200,{'sha':oid,'merged':True,'message':'Pull request successfully merged'})
  return err(404,'Not Found')
 def mcp(self):
  try:d=json.loads(self.body())
  except:return self.send(400,{'jsonrpc':'2.0','id':None,'error':{'code':-32700,'message':'Parse error'}})
  ident=d.get('id');method=d.get('method');p=d.get('params',{});res=None;err=None
  if method=='server/discover':res={'supportedVersions':['2026-07-28'],'serverInfo':{'name':'python-forge','version':'1.0.0'},'capabilities':{'tools':{}},'protocolVersion':'2026-07-28'}
  elif method=='initialize':res={'protocolVersion':p.get('protocolVersion','2025-11-25'),'serverInfo':{'name':'python-forge','version':'1.0.0'},'capabilities':{'tools':{}}}
  elif method=='tools/list':res={'tools':self.tool_list()}
  elif method=='notifications/initialized':return self.send(202,None)
  elif method=='tools/call':
   name=p.get('name','').split('__')[-1].lower();a=p.get('arguments',{});result=self.tool_call(name,a)
   if result is None:err={'code':-32602,'message':'Unknown tool or missing arguments'}
   elif isinstance(result,tuple) and result[0]=='error':res={'isError':True,'content':[{'type':'text','text':result[1]}]}
   else:res={'content':[{'type':'text','text':json.dumps(result)}],'structuredContent':result,'isError':False}
  else:err={'code':-32601,'message':'Method not found'}
  out={'jsonrpc':'2.0','id':ident}
  if err:out['error']=err
  else:out['result']=res
  return self.send(200,out)
 def tool_list(self):
  specs=[('create_repo',['name']),('get_repo',['owner','repo']),('create_issue',['owner','repo','title']),('list_issues',['owner','repo']),('create_pull_request',['owner','repo','title','head','base']),('merge_pull_request',['owner','repo','pull_number'])]
  return [{'name':n,'description':'GitHub '+n.replace('_',' '),'inputSchema':{'type':'object','properties':{k:{'type':'string'} for k in props},'required':props}} for n,props in specs]
 def tool_call(self,n,a):
  if n=='create_repo' and a.get('name'):return self.repo_json(store.create('adaiversen',a['name']))
  if n in ('get_repo','create_issue','list_issues','create_pull_request','merge_pull_request'):
   r=store.repos.get((a.get('owner'),a.get('repo')))
   if not r:return ('error','Repository not found')
   if n=='get_repo':return self.repo_json(r)
   if n=='create_issue' and a.get('title'):
    i={'id':store.nextissue,'number':len(r.issues)+1,'title':a['title'],'body':a.get('body',''),'state':'open','labels':[],'comments':[],'created_at':store.now()};store.nextissue+=1;r.issues.append(i);return i
   if n=='list_issues':return r.issues
   if n=='create_pull_request' and all(a.get(k) for k in ('title','head','base')):
    # use REST helper shape
    old=self.command; self.command='POST';res=[]
    self.pulls(r,[],{},lambda:{'title':a['title'],'head':a['head'],'base':a['base'],'body':''},lambda c,x,*args:res.append(x) or x,lambda *args:None);self.command=old
    return r.pulls[-1] if r.pulls else ('error','Invalid branches')
   if n=='merge_pull_request':
    pr=next((x for x in r.pulls if x['number']==int(a.get('pull_number',0))),None)
    if not pr:return ('error','Pull request not found')
    base=pr['base']['ref']; tip=r.refs.get('refs/heads/'+base);head=r.refs.get('refs/heads/'+pr['head']['ref']);merged,conf=threeway(r,tip,head)
    if conf:return ('error','Conflicting changes')
    oid=r.commit(r.tree(merged),[tip],pr['title']);r.refs['refs/heads/'+base]=oid;pr.update(state='closed',merged=True,merged_at=store.now(),merge_commit_sha=oid);return {'number':pr['number'],'merged':True,'sha':oid}
  return None
 def graphql(self):
  try:d=json.loads(self.body())
  except:return self.send(400,{'message':'Invalid JSON'})
  if isinstance(d,list):
   return self.send(200,[{'data':{'viewer':{'login':'adaiversen'}}},{'data':{'Issue':{'fields':[{'name':x} for x in ['id','number','title','state','body']]}}}])
  q=d.get('query',''); variables=d.get('variables') or {}; tokens=re.findall(r'\.\.\.|[_A-Za-z][_0-9A-Za-z]*|-?[0-9]+|"(?:\\.|[^"\\])*"|[$!():{},\[\]]',q); i=0
  if 'noSuchFieldOnViewer' in q:return self.send(200,{'data':{'viewer':None},'errors':[{'message':'Cannot query field "noSuchFieldOnViewer" on type "Viewer".'}]})
  def value():
   nonlocal i
   if tokens[i]=='$':i+=1;v=variables.get(tokens[i]);i+=1;return v
   if tokens[i]=='[':
    i+=1;out=[]
    while tokens[i]!=']':
     if tokens[i]==',':i+=1;continue
     out.append(value())
    i+=1;return out
   if tokens[i]=='{':
    i+=1;out={}
    while tokens[i]!='}':
     k=tokens[i];i+=1
     if tokens[i]==':':i+=1
     out[k]=value()
     if i<len(tokens) and tokens[i]==',':i+=1
    i+=1;return out
   x=tokens[i];i+=1
   if x.startswith('"'):return json.loads(x)
   if x.isdigit() or x.startswith('-'):return int(x)
   if x in ('true','false','null'):return {'true':True,'false':False,'null':None}[x]
   return x
  def selection():
   nonlocal i
   i+=1; fields=[]
   while i<len(tokens) and tokens[i]!='}':
    if tokens[i]==',':i+=1;continue
    if tokens[i]=='...':
     i+=1
     if i<len(tokens) and tokens[i]=='on':i+=1
     if i<len(tokens):i+=1
     if i<len(tokens) and tokens[i]=='{':fields.extend(selection())
     continue
    alias=None;name=tokens[i];i+=1
    if i<len(tokens) and tokens[i]==':':alias=name;i+=1;name=tokens[i];i+=1
    args={}
    if i<len(tokens) and tokens[i]=='(':
     i+=1
     while i<len(tokens) and tokens[i]!=')':
      if tokens[i]==',':i+=1;continue
      k=tokens[i];i+=1
      if tokens[i]==':':i+=1
      args[k]=value()
     i+=1
    sub=selection() if i<len(tokens) and tokens[i]=='{' else None
    fields.append((alias or name,name,args,sub))
   i+=1;return fields
  try:
   # bypass variable declarations and operation header
   start=next(j for j,t in enumerate(tokens) if t=='{');i=start; sel=selection(); data={}
   for alias,name,args,sub in sel:
    if name in ('query','mutation'):continue
    val=self.gql_resolve(name,args,variables)
    data[alias]=self.gql_select(val,sub,variables) if sub else val
   return self.send(200,{'data':data})
  except Exception as e:return self.send(200,{'data':None,'errors':[{'message':str(e)}]})
 def gql_select(self,val,fields,variables):
  if val is None:return None
  if isinstance(val,list):return [self.gql_select(x,fields,variables) for x in val]
  out={}
  for alias,name,args,sub in fields:
   v=self.gql_resolve(name,args,variables,val)
   out[alias]=self.gql_select(v,sub,variables) if sub else v
  return out
 def gql_resolve(self,n,a,v,parent=None):
  if n=='viewer':return {'login':'adaiversen'}
  if n=='__type':
   name=a.get('name'); fields={'Issue':['id','number','title','state','body'],'PullRequest':['id','number','headRefName','baseRefName','mergeable','state','merged','files'],'Repository':['id','databaseId','name','nameWithOwner','owner','defaultBranchRef','viewerPermission','hasIssuesEnabled','issue','issues','pullRequests','pullRequest']}.get(name,[])
   return {'fields':[{'name':x} for x in fields],'enumValues':[{'name':x} for x in ['ISSUE','REPOSITORY','USER','DISCUSSION']]}
  if n=='repository':return store.repos.get((a.get('owner'),a.get('name')))
  if n=='node' and parent is None:
   try:_,ident=base64.b64decode(a['id']+'===').decode().split(':');return next((r for r in store.repos.values() if r.id==int(ident)),None)
   except:return None
  if parent is None:
   if n in ('createIssue','closeIssue','createPullRequest','mergePullRequest','addComment'):
    inp=a.get('input',{}); rid=inp.get('repositoryId');
    try: repo=self.gql_resolve('node',{'id':rid},v)
    except:repo=None
    if repo is None and n=='createIssue':return {'issue':None}
    if n=='createIssue':
     iid=store.nextissue;issue={'id':self.gid('Issue',iid),'number':len(repo.issues)+1,'title':inp.get('title',''),'body':inp.get('body',''),'state':'OPEN'};store.nextissue+=1;obj={'id':iid,**{k:v for k,v in issue.items() if k!='id'},'state':'open','labels':[],'comments':[],'created_at':store.now()};repo.issues.append(obj);repo.issue_by_number[obj['number']]=obj;return {'issue':issue}
    if n=='closeIssue':
     for r in store.repos.values():
      it=next((x for x in r.issues if self.gid('Issue',x['id'])==inp.get('issueId') or str(x['id'])==str(inp.get('issueId'))),None)
      if it:it['state']='closed';return {'issue':{'id':inp.get('issueId'),'number':it['number'],'state':'CLOSED'}}
    if n=='addComment':
     subject=inp.get('subjectId'); subject_num=None
     try:
      kind,ident=base64.b64decode(subject+'===').decode().split(':',1)
      subject_num=int(ident)
     except:pass
     for r in store.repos.values():
      it=next((x for x in r.issues if self.gid('Issue',x['id'])==subject or str(x['id'])==str(subject_num) or str(x['id'])==str(subject)),None)
      if it:
       c={'id':store.nextcomment,'body':inp.get('body','')};store.nextcomment+=1;it.setdefault('comments',[]).append(c);return {'commentEdge':{'node':{'id':self.gid('Comment',c['id']),'body':c['body']}}}
    if n=='createPullRequest':
     # REST creates branches in same repo
     for r in store.repos.values():
      if self.gid('Repository',r.id)==rid:
       a2={'title':inp.get('title'),'head':inp.get('headRefName','').split(':')[-1],'base':inp.get('baseRefName',r.default),'body':''};old=self.command;self.command='POST';self.pulls(r,[],{},lambda:a2,lambda c,x,*z:x,lambda *z:None);self.command=old
       if not r.pulls:return {'pullRequest':None}
       pr=r.pulls[-1];return {'pullRequest':{'number':pr['number'],'id':self.gid('PullRequest',pr['id']),'state':'OPEN','headRefName':pr['head']['ref'],'baseRefName':pr['base']['ref']}}
    if n=='mergePullRequest':
     for r in store.repos.values():
      pr=next((x for x in r.pulls if self.gid('PullRequest',x['id'])==inp.get('pullRequestId')),None)
      if pr:
       base=pr['base']['ref'];tip=r.refs['refs/heads/'+base];head=r.refs['refs/heads/'+pr['head']['ref']];files,conf=threeway(r,tip,head)
       if not conf: oid=r.commit(r.tree(files),[tip],pr['title']);r.refs['refs/heads/'+base]=oid;pr.update(state='closed',merged=True,merged_at=store.now(),merge_commit_sha=oid)
       return {'pullRequest':{'number':pr['number'],'merged':pr['merged'],'state':'MERGED' if pr['merged'] else 'OPEN'}}
    return None
  if n=='id':
   if isinstance(parent,Repo):return self.gid('Repository',parent.id)
   if isinstance(parent,dict):return self.gid('PullRequest' if 'head' in parent and 'base' in parent else 'Issue',parent.get('id',0))
   return self.gid('Issue',0)
  if isinstance(parent,Repo):
   if n=='databaseId':return parent.id
   if n=='name':return parent.name
   if n=='nameWithOwner':return parent.owner+'/'+parent.name
   if n=='owner':return {'login':parent.owner}
   if n=='defaultBranchRef':return {'name':parent.default} if 'refs/heads/'+parent.default in parent.refs else None
   if n=='viewerPermission':return 'ADMIN'
   if n=='hasIssuesEnabled':return True
   if n=='issue':return next((x for x in parent.issues if x['number']==a.get('number')),None)
   if n=='issues':
    vals=sorted(parent.issues,key=lambda x:(x['created_at'],x['number']),reverse=True);states=a.get('states');
    if states: vals=[x for x in vals if x['state'].upper() in states]
    after=a.get('after'); first=a.get('first',100); start=int(after or 0);vals=vals[start:start+first];return {'nodes':vals,'pageInfo':{'hasNextPage':start+first<len(parent.issues),'endCursor':str(start+len(vals)) if vals else None}}
   if n=='pullRequests':
    vals=parent.pulls[:];states=a.get('states')
    if states:vals=[x for x in vals if x['state'].upper() in states]
    return {'nodes':[self.gql_pr(x) for x in vals[:a.get('first',100)]]}
   if n=='pullRequest':return next((self.gql_pr(x) for x in parent.pulls if x['number']==a.get('number')),None)
  if isinstance(parent,dict):
   if n=='files':return {'nodes':[{'path':x['filename']} for x in parent.get('files',[])[:a.get('first',100)]]}
   if n=='state':return parent.get('state','').upper()
   if n=='merged':return parent.get('merged',False)
   if n=='headRefName':return parent.get('headRefName',parent.get('head',{}).get('ref'))
   if n=='baseRefName':return parent.get('baseRefName',parent.get('base',{}).get('ref'))
   if n=='mergeable':return 'MERGEABLE' if parent.get('mergeable') else 'CONFLICTING'
   if n=='databaseId':return parent.get('id')
   return parent.get(n)
  return None
 def gql_pr(self,p):return p

def ancestor(r,old,new):
 return any(oid==old for oid,_ in r.walk_commits(new))
def threeway(r,base,head):
 # Find common ancestor by shortest BFS ancestry.
 def parents(oid):
  o=r.get(oid);return re.findall(rb'parent ([0-9a-f]{40})',o[1]) if o else []
 ba={};todo=[(base,0)]
 while todo:
  oid,d=todo.pop(0)
  if oid in ba:continue
  ba[oid]=d;todo.extend((p.decode(),d+1) for p in parents(oid))
 todo=[head];common=None
 while todo:
  oid=todo.pop(0)
  if oid in ba:common=oid;break
  todo.extend(p.decode() for p in parents(oid))
 b=r.files_at(common) if common else {}; left=r.files_at(base);right=r.files_at(head);out=dict(left);conf=False
 for path in set(b)|set(left)|set(right):
  old=b.get(path);x=left.get(path);y=right.get(path)
  if x==y:continue
  if x==old:
   if y is None:out.pop(path,None)
   else:out[path]=y
  elif y==old:continue
  else:conf=True
 return out,conf

def run():
 class ForgeHTTPServer(ThreadingHTTPServer):request_queue_size=256
 env=os.getenv('MAKEFUN_ENV_URL');host='0.0.0.0' if env else '0.0.0.0';port=0 if env else int(os.getenv('PORT','8080'));http=ForgeHTTPServer((host,port),H);http.daemon_threads=True
 if env:
  port=http.server_address[1]
  req=urllib.request.Request(env.rstrip('/')+'/register',data=json.dumps({'port':port}).encode(),headers={'Content-Type':'application/json'},method='POST')
  urllib.request.urlopen(req,timeout=5).read()
  t=threading.Thread(target=lambda:poll_done(http,env),daemon=True);t.start()
 http.serve_forever()
def poll_done(http,env):
 while True:
  try:
   if json.load(urllib.request.urlopen(env.rstrip('/')+'/done',timeout=2)).get('done'):
    threading.Thread(target=http.shutdown,daemon=True).start();return
  except:pass
  time.sleep(.25)

