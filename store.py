import os, hashlib, zlib, time, re
from collections import deque

USERS={'adaiversen':(1000,'ada iversen','User'),'quinnkeller':(1001,'quinn keller','User'),'olgabauer':(1002,'olga bauer','User'),'faint-meadow-org':(1003,'faint meadow org','Organization')}
ZERO='0'*40
class Repo:
 def __init__(self,owner,name,id):
  self.owner=owner; self.name=name; self.id=id; self.private=False; self.description=''; self.default='main'; self.objects={}; self.refs={}; self.issues=[]; self.issue_by_number={}; self.pulls=[]; self.labels=[]; self._ancestry_cache={}; self._pack_cache={}
 def put(self,typ,data):
  raw=typ.encode()+b' '+str(len(data)).encode()+b'\0'+data; oid=hashlib.sha1(raw).hexdigest(); self.objects.setdefault(oid,(typ,data)); return oid
 def get(self,oid): return self.objects.get(oid)
 def commit(self,tree,parents,msg,author='ada iversen <adaiversen@example.invalid>',ts=1700000000):
  tz='+0000'; stamp=f'{ts} {tz}'; s=f'tree {tree}\n'+''.join(f'parent {p}\n' for p in parents)+f'author {author} {stamp}\ncommitter {author} {stamp}\n\n{msg.rstrip()}\n'; return self.put('commit',s.encode())
 def tree(self, files):
  root={}
  for path,blob in files.items():
   node=root; parts=path.split('/')
   for p in parts[:-1]: node=node.setdefault(p,{})
   node[parts[-1]]=blob
  def build(n):
   entries=[]
   for k,v in n.items(): entries.append((k,('40000',build(v)) if isinstance(v,dict) else ('100644',v)))
   entries.sort(key=lambda x:x[0]+('/' if x[1][0]=='40000' else ''))
   return self.put('tree',b''.join(mode.encode()+b' '+name.encode()+b'\0'+bytes.fromhex(oid) for name,(mode,oid) in entries))
  return build(root)
 def files_at(self,commit):
  obj=self.get(commit)
  if not obj:return {}
  tree=obj[1].split(b'\n',1)[0].split()[1].decode(); out={}
  def walk(oid,prefix):
   data=self.get(oid)
   if not data:return
   raw=data[1]; i=0
   while i<len(raw):
    j=raw.index(b'\0',i); mode,name=raw[i:j].split(b' ',1); child=raw[j+1:j+21].hex(); i=j+21; name=name.decode(); path=prefix+name
    if mode==b'40000':walk(child,path+'/')
    else:out[path]=child
  walk(tree,''); return out
 def parents(self,oid):
  obj=self.get(oid)
  return [p.decode() for p in re.findall(rb'(?m)^parent ([0-9a-f]{40})$',obj[1])] if obj and obj[0]=='commit' else []
 def walk_commits(self,tip):
  todo=deque([tip]); seen=set()
  while todo:
   c=todo.popleft()
   if c in seen:continue
   seen.add(c); obj=self.get(c)
   if obj and obj[0]=='commit':
    yield c,obj[1]
    todo.extend(self.parents(c))

class Store:
 def __init__(self): self.repos={}; self.nextid=1; self.nextissue=1; self.nextpull=1; self.nextcomment=1; self.clock=0; self._base=None
 def now(self):
  raw=os.getenv('MAKEFUN_TRIAL_NOW');
  if raw:
   if self._base is None:
    import datetime
    try: self._base=datetime.datetime.fromisoformat(raw.replace('Z','+00:00')).timestamp()
    except: self._base=1700000000
   self.clock+=1; return time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime(self._base+self.clock))
  return time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime())
 def create(self,o,n):
  if (o,n) in self.repos:return self.repos[o,n]
  r=Repo(o,n,self.nextid); self.nextid+=1; self.repos[o,n]=r; return r
store=Store()
