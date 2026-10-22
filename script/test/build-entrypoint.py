import atexit, shutil
from pathlib import Path
import tempfile,os,subprocess,json,shutil
r=Path(__file__).resolve().parents[2]; tmp=Path(tempfile.mkdtemp(prefix='helper space.')); bins=tmp/'bin'; bins.mkdir(); images=tmp/'repo space/images'; images.mkdir(parents=True)
atexit.register(shutil.rmtree, tmp)
shutil.copyfile(r/'images/build.sh',images/'build.sh')
# A fake host adapter for --host-helpers: the real one compiles Go, so the copied
# spaced checkout carries a script that records its argv and prints a context root.
(repo_script:=tmp/'repo space/script/build').mkdir(parents=True)
shutil.copyfile(r/'script/test/fixtures/recipe-contract-adapter.sh', repo_script/'recipe-contract.sh')
(repo_script/'recipe-contract.sh').chmod(0o755)
(bins/'docker').write_text('''#!/usr/bin/env python3
import os,sys,json
with open(os.environ['LOG'],'a') as f: f.write(json.dumps(sys.argv[1:])+'\\n')
if sys.argv[1:]==['context','show']: print('context-name'); sys.exit(int(os.environ.get('CONTEXT_STATUS','0')))
if sys.argv[1:]==['buildx','inspect','context-name']: print('Driver: '+os.environ.get('DRIVER','docker')); sys.exit(int(os.environ.get('INSPECT_STATUS','0')))
if sys.argv[1]=='build': sys.exit(int(os.environ.get('BUILD_STATUS','0')))
sys.exit(99)
'''); (bins/'docker').chmod(0o755)
(bins/'uname').write_text('#!/bin/sh\nprintf "%s\\n" "$ARCH"\n'); (bins/'uname').chmod(0o755)
env=dict(os.environ,PATH=str(bins)+':'+os.environ['PATH'],ARCH='arm64',LOG=str(tmp/'log'),HOST_LOG=str(tmp/'host-log'),HOST_CONTEXT=str(tmp/'context root'))
# The suite runs under contracts.sh, which exports one context for every
# suite; these controls own each reuse decision, so ambient state must not
# decide it for them.
env.pop('RECIPE_CONTRACT_CONTEXT', None)
def run(args,code,updates=None):
 (tmp/'log').write_text('')
 (tmp/'host-log').write_text('')
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
# --host-helpers: the fake adapter receives the validated platform and one named
# context reaches the Docker argv, before the caller's own options.
for recipe,tag in [('base','agent-vm-base:local'),('standard','agent-vm-standard:local')]:
 calls=run([recipe,'--host-helpers','--','--build-arg','X=1'],0,{'ARCH':'arm64'})
 host_calls=(tmp/'host-log').read_text().splitlines()
 assert host_calls==['--os linux --arch arm64'],host_calls
 argv=calls[-1]
 assert argv[:7]==['build','--builder','context-name','--platform','linux/arm64','-t',tag],argv
 assert argv.count('--build-context')==1,argv
 index=argv.index('--build-context')
 assert argv[index+1]=='recipe-contract-bin='+env['HOST_CONTEXT'],argv
 assert argv[index+2:]==['--build-arg','X=1',str(images)],argv
assert not run(['base','--host-helpers','--','--build-context','recipe-contract-bin=/elsewhere'],2)
assert not run(['base','--host-helpers','--','--build-context=recipe-contract-bin=/elsewhere'],2)
# Without --host-helpers the script injects nothing, so a caller who names the
# seam is using it rather than colliding with it: the raw path has to reach
# Docker unchanged (the only way to replace the stage without host Go).
for recipe, argv, tail in [
        ('base', ['--build-context','recipe-contract-bin=/elsewhere'], ['--build-context','recipe-contract-bin=/elsewhere']),
        ('standard', ['--build-context=recipe-contract-bin=/elsewhere'], ['--build-context=recipe-contract-bin=/elsewhere'])]:
    calls = run([recipe,'--']+argv, 0)
    assert calls[-1][-1-len(tail):-1] == tail, calls[-1]
    assert calls[-1].count('--build-context') + calls[-1].count('--build-context=recipe-contract-bin=/elsewhere') == 1, calls[-1]
assert not run(['base','--bogus-helpers'],2)
assert not run(['base','--host-helpers','--host-helpers'],2)
assert len(run(['base','--host-helpers'],45,{'HOST_STATUS':'45'}))==2
assert len(run(['base','--host-helpers'],1,{'ARCH':'unknown'}))==0

# --- invocations that name the script instead of its path (regression guard) ---
# ${BASH_SOURCE[0]%/*} yields the bare file name here, so `cd` failed with
# "build.sh: Not a directory" and the wrapper never reached Docker.
(tmp / 'log').write_text('')
relative = subprocess.run(['/bin/bash', 'build.sh', 'base'], cwd=str(images), env=dict(env, ARCH='arm64'),
                          text=True, capture_output=True)
assert relative.returncode == 0, (relative.returncode, relative.stdout, relative.stderr)
relative_calls = [json.loads(line) for line in (tmp / 'log').read_text().splitlines()]
assert relative_calls[-1][:7] == ['build', '--builder', 'context-name', '--platform', 'linux/arm64', '-t', 'agent-vm-base:local'], relative_calls
# A relative invocation resolves the same directory through the shell's
# logical cwd, which macOS reports as /private/tmp; compare the real paths.
assert relative_calls[-1][-2:] == [os.path.realpath(images / 'Dockerfile'), os.path.realpath(images)], relative_calls
print('relative invocation controls passed')

# --- the real host adapter's argv handling ---
# The defect these guard: `usage 2` wrote a file named 2 into the caller's cwd and
# did not exit, so an unknown option looped forever rewriting it.
adapter = r / 'script/build/recipe-contract.sh'
scratch = tmp / 'adapter-cwd'
scratch.mkdir()

def adapter_run(args, code):
    result = subprocess.run(['/bin/bash', str(adapter), *args], cwd=str(scratch), text=True,
                            capture_output=True, timeout=60)
    assert result.returncode == code, (args, result.returncode, result.stdout, result.stderr)
    assert not (scratch / '2').exists(), (args, 'the adapter wrote a file named 2')
    return result

for args in (['--bogus'], ['--os'], ['--arch'], ['--os', 'plan9'], ['--arch', 'riscv64'],
             ['--os=linux', '--os'], ['--help', 'x'], ['extra']):
    result = adapter_run(args, 2)
    assert 'usage: script/build/recipe-contract.sh' in result.stderr, (args, result.stderr)
    assert result.stdout == '', (args, result.stdout)
help_result = adapter_run(['--help'], 0)
assert help_result.stdout.startswith('usage: script/build/recipe-contract.sh'), help_result.stdout
assert help_result.stderr == ''
print('host adapter argv controls passed')

# --- the build recipe publishes atomically and replaces only its own output ---
recipe = r / 'images/recipe-contract/build.sh'
(tmp / 'fake-go').mkdir()
fake_go = tmp / 'fake-go/go'
fake_go.write_text('''#!/usr/bin/env python3
import json, os, sys, time
args = sys.argv[1:]
with open(os.environ['GO_LOG'], 'a') as log:
    log.write(json.dumps({'args': args, 'cwd': os.getcwd(),
                          'env': {k: os.environ.get(k) for k in
                                  ['GOOS', 'GOARCH', 'GOAMD64', 'GOARM64', 'CGO_ENABLED',
                                   'GOWORK', 'GOFLAGS', 'GOEXPERIMENT']}}) + '\\n')
if args[:1] != ['build']:
    sys.exit(99)
time.sleep(float(os.environ.get('GO_DELAY', '0')))
target = args[args.index('-o') + 1]
if os.environ.get('GO_STATUS', '0') != '0':
    sys.exit(int(os.environ['GO_STATUS']))
if os.environ.get('GO_NO_BINARY') != '1':
    with open(target, 'wb') as handle:
        handle.write(b'#!/bin/sh\\nexit 0\\n')
    os.chmod(target, 0o755)
''')
fake_go.chmod(0o755)
(tmp / 'fake-uname').mkdir()
(tmp / 'fake-uname/uname').write_text('#!/bin/sh\ncase "$1" in -s) echo Darwin ;; *) echo "${FAKE_UNAME:-arm64}" ;; esac\n')
(tmp / 'fake-uname/uname').chmod(0o755)
(tmp / 'go-log').write_text('')
recipe_env = dict(os.environ, PATH=str(tmp / 'fake-go') + ':' + os.environ['PATH'],
                  GO_LOG=str(tmp / 'go-log'))
unwritten = tmp / 'unwritten'
unwritten.mkdir()
(unwritten / 'caller-file').write_text('keep me')
refused = subprocess.run(['/bin/bash', str(recipe), str(unwritten)], env=recipe_env, text=True, capture_output=True)
assert refused.returncode == 2, (refused.returncode, refused.stderr)
assert 'refusing to replace' in refused.stderr, refused.stderr
assert (unwritten / 'caller-file').read_text() == 'keep me', 'an unowned directory was modified'
inside = subprocess.run(['/bin/bash', str(recipe), str(r / 'images/recipe-contract/out')], env=recipe_env,
                        text=True, capture_output=True)
assert inside.returncode == 2 and 'outside the Go module' in inside.stderr, (inside.returncode, inside.stderr)
owned = tmp / 'owned'
published = subprocess.run(['/bin/bash', str(recipe), str(owned)], env=recipe_env, text=True, capture_output=True)
assert published.returncode == 0, (published.returncode, published.stdout, published.stderr)
assert (owned / '.recipe-contract-composed').is_file(), sorted(p.name for p in owned.iterdir())
binary = owned / 'recipe-contract'
assert binary.is_file() and binary.stat().st_mode & 0o777 == 0o755, oct(binary.stat().st_mode)
failed = subprocess.run(['/bin/bash', str(recipe), str(owned)], env=dict(recipe_env, GO_STATUS='1'),
                        text=True, capture_output=True)
assert failed.returncode == 1, (failed.returncode, failed.stderr)
assert binary.is_file() and (owned / '.recipe-contract-composed').is_file(), 'a failed build removed the previous result'
assert not [entry.name for entry in tmp.iterdir() if '.stage.' in entry.name or '.previous.' in entry.name], \
    'a staging or previous directory leaked'
again = subprocess.run(['/bin/bash', str(recipe), str(owned)], env=recipe_env, text=True, capture_output=True)
assert again.returncode == 0, (again.returncode, again.stderr)
assert not [entry.name for entry in tmp.iterdir() if '.stage.' in entry.name or '.previous.' in entry.name]
print('build recipe publish controls passed')

# --- the host adapter compiles the way the builder stage does -------------------
# A scratch checkout keeps these controls off the shared target/ directory: the
# adapter derives its context root from its own location.
def scratch_repo(name):
    root = tmp / name
    (root / 'script/build').mkdir(parents=True)
    shutil.copyfile(r / 'script/build/recipe-contract.sh', root / 'script/build/recipe-contract.sh')
    module = root / 'images/recipe-contract'
    (module / 'cmd/recipe-contract').mkdir(parents=True)
    shutil.copyfile(recipe, module / 'build.sh')
    (module / 'go.mod').write_text('module example.test/recipe-contract\n\ngo 1.27.0\n')
    (module / 'cmd/recipe-contract/main.go').write_text('package main\n\nfunc main() {}\n')
    (module / 'run-report.sh').write_text('#!/bin/sh\n')
    (module / 'run-report.sh').chmod(0o644)
    (module / 'run-install.sh').write_text('#!/bin/sh\n')
    (module / 'run-install.sh').chmod(0o755)
    return root, module

repo, module = scratch_repo('scratch repo')
adapter = repo / 'script/build/recipe-contract.sh'
adapter_env = dict(recipe_env, PATH=str(tmp / 'fake-go') + ':' + str(tmp / 'fake-uname') + ':' + os.environ['PATH'])
adapter_env.pop('RECIPE_CONTRACT_CONTEXT', None)

def adapter_build(args, extra=None, script=None):
    (tmp / 'go-log').write_text('')
    env = dict(adapter_env, **(extra or {}))
    command = ['/bin/bash', str(adapter), *args]
    return subprocess.run([*script, *command] if script else command, cwd='/tmp', env=env,
                          text=True, capture_output=True)

def go_calls():
    return [json.loads(line) for line in (tmp / 'go-log').read_text().splitlines()]

# Defaults: --os linux, host architecture from uname, one context root printed.
defaults = adapter_build([])
assert defaults.returncode == 0, (defaults.returncode, defaults.stderr)
context = repo / 'target/recipe-contract/linux-arm64'
assert defaults.stdout == str(context) + '\n', defaults.stdout
assert not defaults.stderr, defaults.stderr
assert (context / 'out/recipe-contract').is_file(), 'the default build published no binary'

# The exact compiler environment and flags the Docker builder stage relies on:
# no ambient GOOS/GOARCH/feature/flag/workspace overrides survive, and the
# compiler owns the flags so the two adapters cannot diverge.
ambient = {'GOOS': 'plan9', 'GOARCH': '386', 'GOAMD64': 'v9', 'GOARM64': 'v9.9',
           'CGO_ENABLED': '1', 'GOWORK': '/nowhere', 'GOFLAGS': '-x', 'GOEXPERIMENT': 'none'}
explicit = adapter_build(['--os', 'linux', '--arch', 'amd64'], ambient)
assert explicit.returncode == 0, (explicit.returncode, explicit.stderr)
assert explicit.stdout == str(repo / 'target/recipe-contract/linux-amd64') + '\n', explicit.stdout
calls = go_calls()
assert len(calls) == 1, calls
call = calls[0]
target = call['args'][call['args'].index('-o') + 1]
assert call['args'] == ['build', '-mod=vendor', '-trimpath', '-buildvcs=false', '-o', target, './cmd/recipe-contract'], call['args']
assert '.stage.' in target and target.endswith('/recipe-contract'), target
# The build runs inside the staged canonical copy, not the checked-out module.
assert os.path.realpath(call['cwd']) == os.path.realpath(os.path.dirname(target)), call['cwd']
assert call['env'] == {'GOOS': 'linux', 'GOARCH': 'amd64', 'GOAMD64': 'v1', 'GOARM64': 'v8.0',
                       'CGO_ENABLED': '0', 'GOWORK': 'off', 'GOFLAGS': '', 'GOEXPERIMENT': None}, call['env']
print('host adapter compile environment controls passed')

# A failed compile, and a compiler that exits 0 without writing the binary, both
# leave no published success path: no new context, no stale binary presented as one.
clean_repo, _clean_module = scratch_repo('failed repo')
clean_adapter = clean_repo / 'script/build/recipe-contract.sh'
for extra in ({'GO_STATUS': '1'}, {'GO_NO_BINARY': '1'}):
    (tmp / 'go-log').write_text('')
    attempt = subprocess.run(['/bin/bash', str(clean_adapter), '--os', 'linux', '--arch', 'arm64'],
                             cwd='/tmp', env=dict(adapter_env, **extra), text=True, capture_output=True)
    assert attempt.returncode == 1, (extra, attempt.returncode, attempt.stderr)
    assert attempt.stdout == '', attempt.stdout
    absent = clean_repo / 'target/recipe-contract/linux-arm64/out'
    assert not absent.exists(), (extra, sorted(str(path) for path in absent.parent.rglob('*')))
print('host adapter failed-compile controls passed')

# umask 0077 in the caller must not leak into the published tree: the audit
# containers read it as numeric users. The reviewed helper modes survive too.
umasked = adapter_build(['--os', 'linux', '--arch', 'arm64'],
                        script=['/bin/sh', '-c', 'umask 077; exec "$@"', 'sh'])
assert umasked.returncode == 0, (umasked.returncode, umasked.stderr)
published_out = repo / 'target/recipe-contract/linux-arm64/out'

def mode_of(path):
    return path.stat().st_mode & 0o777

assert mode_of(published_out) == 0o755, oct(mode_of(published_out))
assert mode_of(published_out / 'recipe-contract') == 0o755, oct(mode_of(published_out / 'recipe-contract'))
assert mode_of(published_out / 'cmd') == 0o755, oct(mode_of(published_out / 'cmd'))
for name, want in [('run-report.sh', 0o644), ('run-install.sh', 0o755), ('go.mod', 0o644)]:
    assert mode_of(published_out / name) == want, (name, oct(mode_of(published_out / name)))
print('host adapter umask and mode controls passed')

# No stale artifacts: a rebuild replaces the whole directory, so a file an older
# recipe or a hand copy left behind cannot survive into the next mount.
(published_out / 'stale-artifact').write_text('old\n')
rerun = adapter_build(['--os', 'linux', '--arch', 'arm64'])
assert rerun.returncode == 0, (rerun.returncode, rerun.stderr)
assert not (published_out / 'stale-artifact').exists(), 'a stale artifact survived a rebuild'
assert not [path for path in published_out.parent.iterdir()
            if '.stage.' in path.name or '.previous.' in path.name or path.name.endswith('.publish-lock')]
print('host adapter stale-artifact controls passed')

# The composed directory is mounted into every audit container, so Python in it
# is refused rather than published (the repository-side guard may not have run).
(module / 'py').mkdir()
(module / 'py/helper.py').write_text('print("old helper")\n')
python_in_module = adapter_build(['--os', 'linux', '--arch', 'arm64'])
assert python_in_module.returncode == 2, (python_in_module.returncode, python_in_module.stderr)
assert 'refusing to publish Python' in python_in_module.stderr, python_in_module.stderr
assert 'py/helper.py' in python_in_module.stderr, python_in_module.stderr
assert mode_of(published_out / 'recipe-contract') == 0o755, 'a refused build damaged the published tree'
assert not (published_out / 'py').exists(), 'a refused build published the Python file'
(module / 'py/helper.py').unlink()
(module / 'py').rmdir()
print('host adapter Python-refusal controls passed')

# Two builds of one output run concurrently: a suite and a --host-helpers image
# build (or two suites) must both succeed and leave one complete directory. The
# swap is serialized, so neither can move the output out from under the other.
def concurrent(index):
    return subprocess.Popen(['/bin/bash', str(adapter), '--os', 'linux', '--arch', 'arm64'],
                            cwd='/tmp', env=dict(adapter_env, GO_DELAY=('1.0', '1.1')[index]),
                            text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)

# Repeat with a stale owner and two simultaneous waiters. The dead child pid
# cannot belong to an active publisher; no arbitrary host pid is assumed dead.
dead = subprocess.Popen(['/bin/sh', '-c', 'exit 0'])
dead.wait()
for stale in [False, True]:
    if stale:
        lock = Path(str(published_out) + '.publish-lock')
        lock.mkdir()
        (lock / 'pid').write_text(str(dead.pid) + '\n')
    processes = [concurrent(0), concurrent(1)]
    results = [(process.wait(timeout=120), process.communicate()) for process in processes]
    for status, (out, err) in results:
        assert status == 0, (stale, status, out, err)
    assert mode_of(published_out / 'recipe-contract') == 0o755
    assert (published_out / '.recipe-contract-composed').is_file()
    assert not [path for path in published_out.parent.iterdir()
                if '.stage.' in path.name or '.previous.' in path.name or path.name.endswith('.publish-lock')]
print('host adapter concurrent-build controls passed')


# Platform selection and parent reuse are adapter-owned, not caller-owned.
for args, expected in [(['--native'], 'darwin-arm64'),
                       (['--platform', 'linux/arm64/v8'], 'linux-arm64'),
                       (['--platform', 'linux/amd64'], 'linux-amd64')]:
    selected = adapter_build(args)
    assert selected.returncode == 0, selected.stderr
    assert selected.stdout.strip() == str(repo / 'target/recipe-contract' / expected)
for args in [['--platform'], ['--platform', 'linux/386'], ['--native', '--arch', 'arm64']]:
    assert adapter_build(args).returncode == 2, args
assert adapter_build(['--native'], {'FAKE_UNAME': 'mystery'}).returncode == 2
reused = adapter_build(['--platform', 'linux/arm64'], {'RECIPE_CONTRACT_CONTEXT': str(context), 'GO_STATUS': '99'})
assert reused.returncode == 0 and reused.stdout.strip() == str(context), reused.stderr
assert not go_calls(), 'matching parent context was rebuilt'
# One context is exported for a whole suite run, so a context that records another
# platform is ignored and the requested one is built; reusing it would audit a
# foreign binary.
assert (context / 'out/.recipe-contract-platform').read_text().strip() == 'linux-arm64'
other = adapter_build(['--platform', 'linux/amd64'], {'RECIPE_CONTRACT_CONTEXT': str(context)})
assert other.returncode == 0, (other.returncode, other.stdout, other.stderr)
assert other.stdout.strip() == str(repo / 'target/recipe-contract/linux-amd64'), other.stdout
assert go_calls(), 'a foreign-platform context was reused instead of rebuilt'
assert (repo / 'target/recipe-contract/linux-amd64/out/.recipe-contract-platform').read_text().strip() == 'linux-amd64'
# A fixture may supply a composed directory a real build never wrote (no platform
# record): it is then the caller's declaration, so it is reused as given.
undeclared = tmp / 'fixture-context'
(undeclared / 'out').mkdir(parents=True)
(undeclared / 'out/recipe-contract').write_text('fixture helper')
fixture_reuse = adapter_build(['--platform', 'linux/arm64'], {'RECIPE_CONTRACT_CONTEXT': str(undeclared)})
assert fixture_reuse.returncode == 0 and fixture_reuse.stdout.strip() == str(undeclared), fixture_reuse.stderr
assert not go_calls(), 'a supplied fixture context was rebuilt'
print('host adapter native/platform/reuse controls passed')

# Exercise standard-image's real argv/context setup without entering image
# audits. The v8 spelling is valid for Docker and the shared adapter alike.
standard_test = tmp / 'repo space/script/test/standard-image.sh'
standard_test.parent.mkdir(parents=True, exist_ok=True)
standard_setup, separator, _ = (r / 'script/test/standard-image.sh').read_text().partition('\nfail() {')
assert separator, 'standard-image argv setup boundary moved'
standard_test.write_text(standard_setup + '\n')
for platform, status in [('linux/arm64/v8', 0), ('linux/arm64', 0), ('linux/amd64', 0), ('linux/arm64/v7', 2)]:
    (tmp / 'host-log').write_text('')
    selected = subprocess.run(['/bin/bash', str(standard_test), 'base', 'standard', '--platform', platform],
                              env=env, text=True, capture_output=True)
    assert selected.returncode == status, (platform, selected.returncode, selected.stderr)
    calls = (tmp / 'host-log').read_text().splitlines()
    assert calls == (['--native', '--platform ' + platform] if status == 0 else []), calls
print('standard image platform alias controls passed')
