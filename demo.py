import base64, os, subprocess, tempfile, time, urllib.request, json, threading, sys
root=os.path.dirname(os.path.abspath(__file__))
tmp=tempfile.TemporaryDirectory();port='18763'; env=os.environ.copy();env['PORT']=port;env['MAKEFUN_SHOP_TOKEN']='dev-token'
server=subprocess.Popen([sys.executable,os.path.join(root,'main.py')],env=env,stdout=None,stderr=None)
base='http://127.0.0.1:'+port; headers={'Authorization':'Bearer dev-token','Content-Type':'application/json'}
def req(path,method='GET',data=None):
 b=None if data is None else json.dumps(data).encode();r=urllib.request.Request(base+path,data=b,headers=headers,method=method);return json.load(urllib.request.urlopen(r))
try:
 for _ in range(100):
  try:urllib.request.urlopen(base+'/healthz',timeout=.2);break
  except:time.sleep(.05)
 req('/api/v3/user/repos','POST',{'name':'demo'})
 work=os.path.join(tmp.name,'work');os.mkdir(work);subprocess.run(['git','init','-b','main',work],check=True,stdout=subprocess.DEVNULL);open(os.path.join(work,'hello.txt'),'w').write('hello forge\n')
 subprocess.run(['git','-C',work,'config','user.name','Ada'],check=True);subprocess.run(['git','-C',work,'config','user.email','ada@example.invalid'],check=True);subprocess.run(['git','-C',work,'add','.'],check=True);subprocess.run(['git','-C',work,'commit','-m','demo'],check=True,stdout=subprocess.DEVNULL)
 subprocess.run(['git','-C',work,'push','http://dev-token@127.0.0.1:'+port+'/adaiversen/demo.git','main'],check=True,stdout=subprocess.DEVNULL)
 clone=os.path.join(tmp.name,'clone');subprocess.run(['git','-c','protocol.version=0','clone','http://dev-token@127.0.0.1:'+port+'/adaiversen/demo.git',clone],check=True,stdout=None,stderr=None)
 assert open(os.path.join(clone,'hello.txt')).read()=='hello forge\n';print('PASS')
finally:server.terminate();server.wait(timeout=5);tmp.cleanup()
