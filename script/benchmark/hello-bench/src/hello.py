#!/usr/bin/env python

#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

# The MIT License (MIT)
# 
# Copyright (c) 2015 Tintri
# 
# Permission is hereby granted, free of charge, to any person obtaining a copy
# of this software and associated documentation files (the "Software"), to deal
# in the Software without restriction, including without limitation the rights
# to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
# copies of the Software, and to permit persons to whom the Software is
# furnished to do so, subject to the following conditions:
# 
# The above copyright notice and this permission notice shall be included in all
# copies or substantial portions of the Software.
# 
# THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
# IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
# FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
# AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
# LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
# OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
# SOFTWARE.

import os, sys, subprocess, select, random, urllib2, time, json, tempfile, shutil

TMP_DIR = tempfile.mkdtemp()
LEGACY_MODE = "legacy"
STARGZ_MODE = "stargz"
ESTARGZ_MODE = "estargz"
DEFAULT_OPTIMIZER = "ctr-remote image optimize"
BENCHMARKOUT_MARK = "BENCHMARK_OUTPUT: "

def exit(status):
    # cleanup
    shutil.rmtree(TMP_DIR)
    sys.exit(status)

def tmp_dir():
    tmp_dir.nxt += 1
    return os.path.join(TMP_DIR, str(tmp_dir.nxt))
tmp_dir.nxt = 0

def tmp_copy(src):
    dst = tmp_dir()
    shutil.copytree(src, dst)
    return dst

def genargs(arg):
    if arg == None or arg == "":
        return ""
    else:
        return '-args \'["%s"]\'' % arg.replace('"', '\\\"').replace('\'', '\'"\'"\'')

class RunArgs:
    def __init__(self, env={}, arg='', stdin='', stdin_sh='sh', waitline='', mount=[]):
        self.env = env
        self.arg = arg
        self.stdin = stdin
        self.stdin_sh = stdin_sh
        self.waitline = waitline
        self.mount = mount

class Bench:
    def __init__(self, name, category='other'):
        self.name = name
        self.repo = name # TODO: maybe we'll eventually have multiple benches per repo
        self.category = category

    def __str__(self):
        return json.dumps(self.__dict__)

class BenchRunner:
    ECHO_HELLO = set(['alpine:3.10.2',
                      'fedora:30',])

    CMD_ARG_WAIT = {'rethinkdb:2.3.6': RunArgs(waitline='Server ready'),
                    'glassfish:4.1-jdk8': RunArgs(waitline='Running GlassFish'),
                    'drupal:8.7.6': RunArgs(waitline='apache2 -D FOREGROUND'),
                    'jenkins:2.60.3': RunArgs(waitline='Jenkins is fully up and running'),
                    'redis:5.0.5': RunArgs(waitline='Ready to accept connections'),
    }

    CMD_STDIN = {'php:7.3.8':  RunArgs(stdin='php -r "echo \\\"hello\\n\\\";"; exit\n'),
                 'gcc:9.2.0': RunArgs(stdin='cd /src; gcc main.c; ./a.out; exit\n',
                                mount=[('gcc', '/src')]),
                 'golang:1.12.9': RunArgs(stdin='cd /go/src; go run main.go; exit\n',
                                   mount=[('go', '/go/src')]),
                 'jruby:9.2.8.0': RunArgs(stdin='jruby -e "puts \\\"hello\\\""; exit\n'),
                 'r-base:3.6.1': RunArgs(stdin='sprintf("hello")\nq()\n', stdin_sh='R --no-save'),
    }

    CMD_ARG = {'perl:5.30': RunArgs(arg='perl -e \'print("hello\\n")\''),
               'python:3.7': RunArgs(arg='python -c \'print("hello")\''),
               'pypy:3.5': RunArgs(arg='pypy3 -c \'print("hello")\''),
               'node:13.13.0': RunArgs(arg='node -e \'console.log("hello")\''),
    }

    # complete listing
    ALL = dict([(b.name, b) for b in
                [Bench('alpine:3.10.2', 'distro'),
                 Bench('fedora:30', 'distro'),
                 Bench('rethinkdb:2.3.6', 'database'),
                 Bench('redis:5.0.5', 'database'),
                 Bench('python:3.7', 'language'),
                 Bench('golang:1.12.9', 'language'),
                 Bench('gcc:9.2.0', 'language'),
                 Bench('jruby:9.2.8.0', 'language'),
                 Bench('perl:5.30', 'language'),
                 Bench('php:7.3.8', 'language'),
                 Bench('pypy:3.5', 'language'),
                 Bench('r-base:3.6.1', 'language'),
                 Bench('glassfish:4.1-jdk8', 'web-server'),
                 Bench('drupal:8.7.6'),
                 Bench('jenkins:2.60.3'),
                 Bench('node:13.13.0'),
             ]])

    def __init__(self, user='library', mode=LEGACY_MODE, optimizer=DEFAULT_OPTIMIZER):
        self.docker = 'ctr'
        self.user = user
        self.registry = 'docker.io/%s/' % user
        self.mode = mode
        self.optimizer = optimizer

    def lazypull(self):
        if self.mode == STARGZ_MODE or self.mode == ESTARGZ_MODE:
            return True
        else:
            return False

    def cleanup(self, name, image):
        print "Cleaning up environment..."
        cmd = '%s t kill -s 9 %s' % (self.docker, name)
        print cmd
        rc = os.system(cmd) # sometimes containers already exit. we ignore the failure.
        cmd = '%s c rm %s' % (self.docker, name)
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)
        cmd = '%s image rm %s' % (self.docker, image)
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)
        cmd = os.path.join(os.path.dirname(os.path.abspath(__file__)), '../reboot_containerd.sh') # clear cache
        if not self.lazypull():
            cmd += ' -nosnapshotter'
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)

    def snapshotter_opt(self):
        if self.lazypull():
            return "--snapshotter=stargz"
        else:
            return ""

    def add_suffix(self, repo):
        if self.mode == ESTARGZ_MODE:
            return "%s-esgz" % repo
        elif self.mode == STARGZ_MODE:
            return "%s-sgz" % repo
        else:
            return "%s-org" % repo

    def pull_subcmd(self):
        if self.lazypull():
            return "rpull"
        else:
            return "pull"
        
    def docker_pullbin(self):
        if self.lazypull():
            return "ctr-remote"
        else:
            return "ctr"

    def run_task(self, cid):
        cmd = '%s t start %s' % (self.docker, cid)
        print cmd
        startrun = time.time()
        rc = os.system(cmd)
        runtime = time.time() - startrun
        assert(rc == 0)
        return runtime
        
    def run_echo_hello(self, repo, cid):
        cmd = ('%s c create --net-host %s -- %s%s %s echo hello' %
               (self.docker, self.snapshotter_opt(), self.registry, self.add_suffix(repo), cid))
        print cmd
        startcreate = time.time()
        rc = os.system(cmd)
        createtime = time.time() - startcreate
        assert(rc == 0)
        return createtime, self.run_task(cid)

    def run_cmd_arg(self, repo, cid, runargs):
        assert(len(runargs.mount) == 0)
        cmd = '%s c create --net-host %s ' % (self.docker, self.snapshotter_opt())
        cmd += '-- %s%s %s ' % (self.registry, self.add_suffix(repo), cid)
        cmd += runargs.arg
        print cmd
        startcreate = time.time()
        rc = os.system(cmd)
        createtime = time.time() - startcreate
        assert(rc == 0)
        return createtime, self.run_task(cid)

    def run_cmd_arg_wait(self, repo, cid, runargs):
        env = ' '.join(['--env %s=%s' % (k,v) for k,v in runargs.env.iteritems()])
        cmd = ('%s c create --net-host %s %s -- %s%s %s %s' %
               (self.docker, self.snapshotter_opt(), env, self.registry, self.add_suffix(repo), cid, runargs.arg))
        print cmd
        startcreate = time.time()
        rc = os.system(cmd)
        createtime = time.time() - startcreate
        assert(rc == 0)
        cmd = '%s t start %s' % (self.docker, cid)
        print cmd
        runtime = 0
        startrun = time.time()

        # line buffer output
        p = subprocess.Popen(cmd, shell=True, bufsize=1,
                             stderr=subprocess.STDOUT,
                             stdout=subprocess.PIPE)
        while True:
            l = p.stdout.readline()
            if l == '':
                continue
            print 'out: ' + l.strip()
            # are we done?
            if l.find(runargs.waitline) >= 0:
                runtime = time.time() - startrun
                # cleanup
                print 'DONE'
                cmd = '%s t kill -s 9 %s' % (self.docker, cid)
                rc = os.system(cmd)
                assert(rc == 0)
                break
        p.wait()
        return createtime, runtime

    def run_cmd_stdin(self, repo, cid, runargs):
        cmd = '%s c create --net-host %s ' % (self.docker, self.snapshotter_opt())
        for a,b in runargs.mount:
            a = os.path.join(os.path.dirname(os.path.abspath(__file__)), a)
            a = tmp_copy(a)
            cmd += '--mount type=bind,src=%s,dst=%s,options=rbind ' % (a,b)
        cmd += '-- %s%s %s ' % (self.registry, self.add_suffix(repo), cid)
        if runargs.stdin_sh:
            cmd += runargs.stdin_sh # e.g., sh -c

        print cmd
        startcreate = time.time()
        rc = os.system(cmd)
        createtime = time.time() - startcreate
        assert(rc == 0)
        cmd = '%s t start %s' % (self.docker, cid)
        print cmd
        startrun = time.time()

        p = subprocess.Popen(cmd, shell=True, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        print runargs.stdin
        out, _ = p.communicate(runargs.stdin)
        runtime = time.time() - startrun
        print out
        assert(p.returncode == 0)
        return createtime, runtime

    def run(self, bench, cid):
        name = bench.name
        print "Pulling the image..."
        startpull = time.time()
        cmd = ('%s images %s %s%s' %
               (self.docker_pullbin(), self.pull_subcmd(), self.registry, self.add_suffix(name)))
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)
        print "syncing..."
        rc = os.system("sync")
        assert(rc == 0)
        pulltime = time.time() - startpull

        runtime = 0
        createtime = 0
        if name in BenchRunner.ECHO_HELLO:
            createtime, runtime = self.run_echo_hello(repo=name, cid=cid)
        elif name in BenchRunner.CMD_ARG:
            createtime, runtime = self.run_cmd_arg(repo=name, cid=cid, runargs=BenchRunner.CMD_ARG[name])
        elif name in BenchRunner.CMD_ARG_WAIT:
            createtime, runtime = self.run_cmd_arg_wait(repo=name, cid=cid, runargs=BenchRunner.CMD_ARG_WAIT[name])
        elif name in BenchRunner.CMD_STDIN:
            createtime, runtime = self.run_cmd_stdin(repo=name, cid=cid, runargs=BenchRunner.CMD_STDIN[name])
        else:
            print 'Unknown bench: '+name
            exit(1)

        return pulltime, createtime, runtime

    def convert_echo_hello(self, repo):
        self.mode = ESTARGZ_MODE
        period=10
        cmd = ('%s -period %s -entrypoint \'["/bin/sh", "-c"]\' -args \'["echo hello"]\' %s %s/%s' %
               (self.optimizer, period, repo, self.user, self.add_suffix(repo)))
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)

    def convert_cmd_arg(self, repo, runargs):
        self.mode = ESTARGZ_MODE
        period = 30
        assert(len(runargs.mount) == 0)
        entry = ""
        if runargs.arg != "": # FIXME: this is naive...
            entry = '-entrypoint \'["/bin/sh", "-c"]\''
        cmd = ('%s -period %s %s %s %s %s/%s' %
               (self.optimizer, period, entry, genargs(runargs.arg), repo, self.user, self.add_suffix(repo)))
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)

    def convert_cmd_arg_wait(self, repo, runargs):
        self.mode = ESTARGZ_MODE
        period = 90
        env = ' '.join(['-env %s=%s' % (k,v) for k,v in runargs.env.iteritems()])
        cmd = ('%s -period %s %s %s %s %s/%s' %
               (self.optimizer, period, env, genargs(runargs.arg), repo, self.user, self.add_suffix(repo)))
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)

    def convert_cmd_stdin(self, repo, runargs):
        self.mode = ESTARGZ_MODE
        period = 60
        cmd = ('%s -period %s -entrypoint \'["/bin/sh", "-c"]\' %s %s %s/%s' %
               (self.optimizer, period, genargs(runargs.stdin_sh), repo, self.user, self.add_suffix(repo)))
        print cmd
        p = subprocess.Popen(cmd, shell=True, stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        print runargs.stdin
        out,_ = p.communicate(runargs.stdin)
        print out
        p.wait()
        assert(p.returncode == 0)

    def copy_img(self, repo):
        self.mode = LEGACY_MODE
        cmd = 'crane copy %s %s/%s' % (repo, self.user, self.add_suffix(repo))
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)
        self.mode = STARGZ_MODE
        cmd = '%s --stargz-only %s %s/%s' % (self.optimizer, repo, self.user, self.add_suffix(repo))
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)
    
    def prepare(self, bench):
        name = bench.name
        if name in BenchRunner.ECHO_HELLO:
            self.convert_echo_hello(repo=name)
        elif name in BenchRunner.CMD_ARG:
            self.convert_cmd_arg(repo=name, runargs=BenchRunner.CMD_ARG[name])
        elif name in BenchRunner.CMD_ARG_WAIT:
            self.convert_cmd_arg_wait(repo=name, runargs=BenchRunner.CMD_ARG_WAIT[name])
        elif name in BenchRunner.CMD_STDIN:
            self.convert_cmd_stdin(repo=name, runargs=BenchRunner.CMD_STDIN[name])
        else:
            print 'Unknown bench: '+name
            exit(1)
        self.copy_img(name)

    def operation(self, op, bench, cid):
        if op == 'run':
            return self.run(bench, cid)
        elif op == 'prepare':
            self.prepare(bench)
            return 0, 0, 0
        else:
            print 'Unknown operation: '+op
            exit(1)

def main():
    if len(sys.argv) == 1:
        print 'Usage: bench.py [OPTIONS] [BENCHMARKS]'
        print 'OPTIONS:'
        print '--user=<user>'
        print '--all'
        print '--list'
        print '--list-json'
        print '--experiments'
        print '--op=(prepare|run)'
        print '--mode=(%s|%s|%s)' % (LEGACY_MODE, STARGZ_MODE, ESTARGZ_MODE)
        exit(1)

    benches = []
    kvargs = {}
    # parse args
    for arg in sys.argv[1:]:
        if arg.startswith('--'):
            parts = arg[2:].split('=')
            if len(parts) == 2:
                kvargs[parts[0]] = parts[1]
            elif parts[0] == 'all':
                benches.extend(BenchRunner.ALL.values())
            elif parts[0] == 'list':
                template = '%-16s\t%-20s'
                print template % ('CATEGORY', 'NAME')
                for b in sorted(BenchRunner.ALL.values(), key=lambda b:(b.category, b.name)):
                    print template % (b.category, b.name)
            elif parts[0] == 'list-json':
                print json.dumps([b.__dict__ for b in BenchRunner.ALL.values()])
        else:
            benches.append(BenchRunner.ALL[arg])

    op = kvargs.pop('op', 'run')
    trytimes = int(kvargs.pop('experiments', '1'))
    if not op == "run":
        trytimes = 1

    # run benchmarks
    runner = BenchRunner(**kvargs)
    for bench in benches:
        cid = '%s_bench_%d' % (bench.repo.replace(':', '-').replace('/', '-'), random.randint(1,1000000))

        elapsed_times = []
        pull_times = []
        create_times = []
        run_times = []

        for i in range(trytimes):
            start = time.time()
            pulltime, createtime, runtime = runner.operation(op, bench, cid)
            elapsed = time.time() - start
            if op == "run":
                runner.cleanup(cid, '%s%s' % (runner.registry, bench.repo))
            elapsed_times.append(elapsed)
            pull_times.append(pulltime)
            create_times.append(createtime)
            run_times.append(runtime)
            print 'ITERATION %s:' % i
            print 'elapsed %s' % elapsed
            print 'pull    %s' % pulltime
            print 'create  %s' % createtime
            print 'run     %s' % runtime

        row = {'mode':'%s' % runner.mode, 'repo':bench.repo, 'bench':bench.name, 'elapsed':sum(elapsed_times) / len(elapsed_times), 'elapsed_pull':sum(pull_times) / len(pull_times), 'elapsed_create':sum(create_times) / len(create_times), 'elapsed_run':sum(run_times) / len(run_times)}
        js = json.dumps(row)
        print '%s%s,' % (BENCHMARKOUT_MARK, js)
        sys.stdout.flush()

if __name__ == '__main__':
    main()
    exit(0)
