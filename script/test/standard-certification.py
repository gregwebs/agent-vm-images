import atexit, shutil
from pathlib import Path
import os,subprocess,sys,tempfile,importlib.util
# Importing the fixture module must not leave a __pycache__ in the source tree.
sys.dont_write_bytecode=True
r=Path(__file__).resolve().parents[2]
# The real verifier runs check-tool-access.py over host paths, which requires every
# audited ancestor to be traversable by all. The release build points TMPDIR under
# /home/runner (mode 750), so the fixture must live under a world-traversable root
# rather than the ambient TMPDIR. It also runs children under umask 077: the 0700
# mkdtemp root was not the only victim, because every nested mkdir inherited 0700
# too, so pin the umask to the image's 022 before creating any fixture directory.
os.umask(0o022)
tmp=Path(tempfile.mkdtemp(prefix='gate.', dir='/tmp')); tmp.chmod(0o755)
atexit.register(shutil.rmtree, tmp)
records=tmp/'records'; records.mkdir(); tools=tmp/'tools'; commands=tmp/'bin'; commands.mkdir()
names=['dsh','pi','pi-claude-bridge','codex','opencode','claude','copilot']; verifiers=['dsh','pi','codex','opencode','claude','copilot']
def writeexe(p,s): p.parent.mkdir(parents=True,exist_ok=True); p.write_text(s); p.chmod(0o755)
# Native fixtures are copied or appended byte-for-byte; variable paths/versions
# travel as environment data instead of being interpolated into program bodies.
FIXTURES=r/'script/test/fixtures/pnpm'
def copyexe(p,src): p.parent.mkdir(parents=True,exist_ok=True); shutil.copyfile(src,p); p.chmod(0o755)
def appendexe(p,s,src): p.write_bytes(s.encode()+Path(src).read_bytes()); p.chmod(0o755)
for name in names: (records/name).write_text('installed\n')
for tool in verifiers:
 writeexe(tools/tool/f'verify-{tool}.sh',f'#!/bin/sh\nset -eu\ntest -z "$AGENT_INSTALL_SOFT_FAIL"\nprintf "{tool}\\n" >> "$CALLS"\n')
for name in ['codex','opencode','claude','pi']: writeexe(commands/name,'#!/bin/sh\nexit 0\n')
prefix=tmp/'prefix'
for name,folder in [('codex','.local'),('claude','.local'),('opencode','.opencode')]:
 p=prefix/folder/'bin'/name; p.parent.mkdir(parents=True,exist_ok=True); p.symlink_to(commands/name)
env=dict(os.environ,AGENT_VM_CONTRACT_DIR=str(r/'images/recipe-contract'),AGENT_VM_TOOL_SOURCE_DIR=str(tools),AGENT_VM_INSTALL_STATUS_DIR=str(records),AGENT_INSTALL_SOFT_FAIL='1',AGENT_VM_CODEX_PREFIX=str(prefix),AGENT_VM_OPENCODE_PREFIX=str(prefix),AGENT_VM_CLAUDE_PREFIX=str(prefix),AGENT_VM_PI_WRAPPER=str(commands/'pi'),CALLS=str(tmp/'calls'),PATH=str(commands)+':'+os.environ['PATH'],AGENT_VM_VERSION_CODEX='rust-v0.159.3')
spec=importlib.util.spec_from_file_location('pnpm_fixture',r/'script/test/fixtures/pnpm/verifier-fixture.py')
fixture_module=importlib.util.module_from_spec(spec); spec.loader.exec_module(fixture_module)
fixture=tmp/'pnpm-fixture'; fixture.mkdir()
source=fixture_module.stage(fixture,'11.11.0')
env.update(AGENT_VM_FIXTURE_ROOT=str(tmp),PNPM_FIXTURE_DESTINATION=str(fixture),PNPM_SOURCE_DIR=str(source),PNPM_FIXTURE_VERSION='11.11.0',PNPM_MUTATION_TARGET=str(fixture/'usr/local/bin/pnpm'),PATH=str(fixture/'usr/local/bin')+':'+env['PATH'])
count=0
def run(ok, e=None,label='gate'):
 global count
 result=subprocess.run(['sh',str(r/'images/standard/verify-standard.sh')],env=e or env,text=True,capture_output=True)
 assert (result.returncode==0)==ok,(label,result.returncode,result.stdout,result.stderr)
 count+=1
 print(f'{label}: expected {"pass" if ok else "nonzero"}, status={result.returncode}')
 return result
run(True,label='installed + all six gates, inherited soft input cleared')
assert (tmp/'calls').read_text().splitlines()==verifiers
for name in names:
 for data in [None,b'pending\n',b'absent-pi\n',b'absent-transport 60\n',b'installed',b'installed\ninstalled\n',b'installed\x00\n']:
  p=records/name
  if data is None: p.unlink()
  else: p.write_bytes(data)
  run(False,label=f'{name} record {data!r}')
  p.write_text('installed\n')
shadow=tmp/'shadow'; shadow.mkdir()
for name in ['codex','opencode','claude','pi']:
 p=shadow/name; writeexe(p,'#!/bin/sh\nprintf "wrong-version\\n"\n')
 e=dict(env,PATH=str(shadow)+':'+env['PATH'])
 result=run(False,e,label=f'distinct readable {name} shadow'); assert str(p) in result.stderr
 p.unlink(); p.symlink_to(commands/name); run(True,e,label=f'{name} samefile symlink')
 p.unlink(); p.symlink_to(shadow/'missing'); run(False,e,label=f'{name} dangling shadow')
 p.unlink(); p.symlink_to(p); run(False,e,label=f'{name} cyclic shadow')
 p.unlink(); writeexe(p,'#!/bin/sh\nexit 0\n'); p.chmod(0o644); run(False,e,label=f'{name} nonexecutable shadow'); p.unlink()
# A raw same-version pnpm is not the reviewed launcher.
p=shadow/'pnpm'; copyexe(p,FIXTURES/'shadow-pnpm.sh')
run(False,dict(env,PATH=str(shadow)+':'+env['PATH'],PNPM_SHADOW_VERSION='11.11.0'),label='same-version raw pnpm shadow')
p.unlink()
# Post-verifier record and identity checks must detect late mutations.
v=tools/'copilot/verify-copilot.sh'; original=v.read_text()
writeexe(v,original+'printf "pending\\n" > "$AGENT_VM_INSTALL_STATUS_DIR/pi-claude-bridge"\n')
run(False,label='late bridge record mutation'); (records/'pi-claude-bridge').write_text('installed\n')
writeexe(v,original+'rm "$AGENT_VM_PI_WRAPPER"\n')
run(False,label='late PATH target removal'); writeexe(commands/'pi','#!/bin/sh\nexit 0\n')
writeexe(v,original+'echo plausible report\nexit 1\n'); run(False,label='verifier output then failure'); writeexe(v,original)
pnpm_launcher=fixture/'usr/local/bin/pnpm'; pnpm_original=pnpm_launcher.read_bytes()
appendexe(v,original,FIXTURES/'launcher-mutation.sh')
run(False,label='late pnpm launcher mutation'); pnpm_launcher.write_bytes(pnpm_original); writeexe(v,original)
# Real codex fixed-path verifier alone accepts a readable wrong-version PATH shadow;
# final identity check must reject that same fixture before other fake verifiers.
writeexe(commands/'codex',"#!/bin/sh\nprintf 'codex-cli 0.159.3\\n'\n")
writeexe(shadow/'codex',"#!/bin/sh\nprintf 'codex-cli 0.0.1\\n'\n")
e=dict(env,PATH=str(shadow)+':'+env['PATH'])
real=subprocess.run(['sh',str(r/'images/tools/codex/verify-codex.sh')],env=e,text=True,capture_output=True)
assert real.returncode==0,(real.stdout,real.stderr)
print('real Codex fixed-path verifier with shadow: pass (demonstrates identity gap)')
(tools/'codex/verify-codex.sh').write_bytes((r/'images/tools/codex/verify-codex.sh').read_bytes())
result=run(False,e,label='real final gate rejects same Codex shadow'); assert str(shadow/'codex') in result.stderr
print(f'{count} final-gate assertions passed; fixture {tmp}')
