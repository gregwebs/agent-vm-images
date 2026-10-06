import atexit, shutil
from pathlib import Path
import tempfile,os,subprocess,json,shutil
r=Path(__file__).resolve().parents[2]; tmp=Path(tempfile.mkdtemp(prefix='helper space.')); bins=tmp/'bin'; bins.mkdir(); images=tmp/'repo space/images'; images.mkdir(parents=True)
atexit.register(shutil.rmtree, tmp)
shutil.copyfile(r/'images/build.sh',images/'build.sh')
(bins/'docker').write_text('''#!/usr/bin/env python3
import os,sys,json
with open(os.environ['LOG'],'a') as f: f.write(json.dumps(sys.argv[1:])+'\\n')
if sys.argv[1:]==['context','show']: print('context-name'); sys.exit(int(os.environ.get('CONTEXT_STATUS','0')))
if sys.argv[1:]==['buildx','inspect','context-name']: print('Driver: '+os.environ.get('DRIVER','docker')); sys.exit(int(os.environ.get('INSPECT_STATUS','0')))
if sys.argv[1]=='build': sys.exit(int(os.environ.get('BUILD_STATUS','0')))
sys.exit(99)
'''); (bins/'docker').chmod(0o755)
(bins/'uname').write_text('#!/bin/sh\nprintf "%s\\n" "$ARCH"\n'); (bins/'uname').chmod(0o755)
env=dict(os.environ,PATH=str(bins)+':'+os.environ['PATH'],ARCH='arm64',LOG=str(tmp/'log'))
def run(args,code,updates=None):
 (tmp/'log').write_text('')
 result=subprocess.run(['/bin/bash',str(images/'build.sh')]+args,cwd='/tmp',env=dict(env,**(updates or {})),text=True,capture_output=True)
 assert result.returncode==code,(args,result.returncode,result.stdout,result.stderr)
 lines=[json.loads(s) for s in (tmp/'log').read_text().splitlines()]
 print(f'{args!r} {updates or ""}: status={result.returncode}, calls={lines!r}')
 return lines
for args in [[],['wat'],['base','--bad'],['standard','--help'],['--help','x']]: assert not run(args,2)
assert not run(['--help'],0)
for arch,platform in [('arm64','linux/arm64'),('aarch64','linux/arm64'),('x86_64','linux/amd64'),('amd64','linux/amd64')]:
 calls=run(['standard','--','--build-arg','BASE_IMAGE=a b','--tag','quoted tag'],0,{'ARCH':arch})
 assert calls==[['context','show'],['buildx','inspect','context-name'],['build','--builder','context-name','--platform',platform,'-t','agent-vm-standard:local','-f',str(images/'standard/Dockerfile'),'--build-arg','BASE_IMAGE=a b','--tag','quoted tag',str(images)]]
assert not run(['base'],1,{'ARCH':'unknown'})
assert len(run(['base'],1,{'DRIVER':'docker-container'}))==2
assert len(run(['base'],43,{'CONTEXT_STATUS':'43'}))==1
assert len(run(['base'],44,{'INSPECT_STATUS':'44'}))==2
assert run(['base'],47,{'BUILD_STATUS':'47'})[-1][0]=='build'
unavailable=tmp/'unavailable'; unavailable.mkdir()
shutil.copyfile(bins/'uname',unavailable/'uname'); (unavailable/'uname').chmod(0o755)
for command in ['dirname', 'grep']:
 (unavailable/command).symlink_to(shutil.which(command))
assert not run(['base'],127,{'PATH':str(unavailable)})
print('Bash 3.2 subprocess helper controls passed')
