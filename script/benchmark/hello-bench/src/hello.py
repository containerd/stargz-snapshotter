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
ESTARGZ_NOOPT_MODE = "estargz-noopt"
ESTARGZ_MODE = "estargz"
DEFAULT_OPTIMIZER = "ctr-remote image optimize --oci"
DEFAULT_PULLER = "nerdctl image pull"
DEFAULT_PUSHER = "nerdctl image push"
DEFAULT_TAGGER = "nerdctl image tag"
BENCHMARKOUT_MARK = "BENCHMARK_OUTPUT: "
CTR="ctr"
PODMAN="podman"

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

def genargs_for_optimization(arg):
    if arg == None or arg == "":
        return ""
    else:
        return '-args \'["%s"]\'' % arg.replace('"', '\\\"').replace('\'', '\'"\'"\'')

def format_repo(mode, repository, name):
    if mode == ESTARGZ_MODE:
        return "%s/%s-esgz" % (repository, name)
    elif mode == ESTARGZ_NOOPT_MODE:
        return "%s/%s-esgz-noopt" % (repository, name)
    else:
        return "%s/%s-org" % (repository, name)

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
                    'tomcat:10.0.0-jdk15-openjdk-buster': RunArgs(waitline='Server startup'),
                    'postgres:13.1': RunArgs(waitline='database system is ready to accept connections',
                                             env={'POSTGRES_PASSWORD': 'abc'}),
                    'mariadb:10.5': RunArgs(waitline='mysqld: ready for connections',
                                            env={'MYSQL_ROOT_PASSWORD': 'abc'}),
                    'wordpress:5.7': RunArgs(waitline='apache2 -D FOREGROUND'),
    }

    CMD_STDIN = {'php:7.3.8':  RunArgs(stdin='php -r "echo \\\"hello\\n\\\";"; exit\n'),
                 'gcc:10.2.0': RunArgs(stdin='cd /src; gcc main.c; ./a.out; exit\n',
                                mount=[('gcc', '/src')]),
                 'golang:1.12.9': RunArgs(stdin='cd /go/src; go run main.go; exit\n',
                                   mount=[('go', '/go/src')]),
                 'jruby:9.2.8.0': RunArgs(stdin='jruby -e "puts \\\"hello\\\""; exit\n'),
                 'r-base:3.6.1': RunArgs(stdin='sprintf("hello")\nq()\n', stdin_sh='R --no-save'),
    }

    CMD_ARG = {'perl:5.30': RunArgs(arg='perl -e \'print("hello\\n")\''),
               'python:3.9': RunArgs(arg='python -c \'print("hello")\''),
               'pypy:3.5': RunArgs(arg='pypy3 -c \'print("hello")\''),
               'node:13.13.0': RunArgs(arg='node -e \'console.log("hello")\''),
    }

    # complete listing
    ALL = dict([(b.name, b) for b in
                [Bench('alpine:3.10.2', 'distro'),
                 Bench('fedora:30', 'distro'),
                 Bench('rethinkdb:2.3.6', 'database'),
                 Bench('postgres:13.1', 'database'),
                 Bench('redis:5.0.5', 'database'),
                 Bench('mariadb:10.5', 'database'),
                 Bench('python:3.9', 'language'),
                 Bench('golang:1.12.9', 'language'),
                 Bench('gcc:10.2.0', 'language'),
                 Bench('jruby:9.2.8.0', 'language'),
                 Bench('perl:5.30', 'language'),
                 Bench('php:7.3.8', 'language'),
                 Bench('pypy:3.5', 'language'),
                 Bench('r-base:3.6.1', 'language'),
                 Bench('drupal:8.7.6'),
                 Bench('jenkins:2.60.3'),
                 Bench('node:13.13.0'),
                 Bench('tomcat:10.0.0-jdk15-openjdk-buster', 'web-server'),
                 Bench('wordpress:5.7', 'web-server'),
             ]])

    def __init__(self, repository='docker.io/library', srcrepository='docker.io/library', mode=LEGACY_MODE, optimizer=DEFAULT_OPTIMIZER, puller=DEFAULT_PULLER, pusher=DEFAULT_PUSHER, runtime="containerd"):
        if runtime == "containerd":
            self.controller = ContainerdController(mode == ESTARGZ_NOOPT_MODE or mode == ESTARGZ_MODE)
        elif runtime == "podman":
            self.controller = PodmanController()
        else:
            print 'Unknown runtime mode: '+runtime
            exit(1)
        self.repository = repository
        self.srcrepository = srcrepository
        self.mode = mode
        self.optimizer = optimizer
        self.puller = puller
        self.pusher = pusher

    def cleanup(self, cid, bench):
        self.controller.cleanup(cid, self.fully_qualify(bench.name))

    def fully_qualify(self, repo):
        return format_repo(self.mode, self.repository, repo)

    def run_task(self, cid):
        cmd = self.controller.task_start_cmd(cid)
        startrun = time.time()
        rc = os.system(cmd)
        runtime = time.time() - startrun
        assert(rc == 0)
        return runtime
        
    def run_echo_hello(self, image, cid):
        cmd = self.controller.create_echo_hello_cmd(image, cid)
        startcreate = time.time()
        rc = os.system(cmd)
        createtime = time.time() - startcreate
        assert(rc == 0)
        return createtime, self.run_task(cid)

    def run_cmd_arg(self, image, cid, runargs):
        assert(len(runargs.mount) == 0)
        cmd = self.controller.create_cmd_arg_cmd(image, cid, runargs)
        startcreate = time.time()
        rc = os.system(cmd)
        createtime = time.time() - startcreate
        assert(rc == 0)
        return createtime, self.run_task(cid)

    def run_cmd_arg_wait(self, image, cid, runargs):
        cmd = self.controller.create_cmd_arg_wait_cmd(image, cid, runargs)
        startcreate = time.time()
        rc = os.system(cmd)
        createtime = time.time() - startcreate
        assert(rc == 0)

        cmd = self.controller.task_start_cmd(cid)
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
                cmd = self.controller.task_kill_cmd(cid)
                rc = os.system(cmd)
                assert(rc == 0)
                break
        p.wait()
        return createtime, runtime

    def run_cmd_stdin(self, image, cid, runargs):
        cmd = self.controller.create_cmd_stdin_cmd(image, cid, runargs)
        startcreate = time.time()
        rc = os.system(cmd)
        createtime = time.time() - startcreate
        assert(rc == 0)

        cmd = self.controller.task_start_cmd(cid)
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
        image = self.fully_qualify(bench.name)

        print "Pulling the image..."
        pullcmd = self.controller.pull_cmd(image)
        startpull = time.time()
        rc = os.system(pullcmd)
        assert(rc == 0)
        pulltime = time.time() - startpull

        runtime = 0
        createtime = 0
        if name in BenchRunner.ECHO_HELLO:
            createtime, runtime = self.run_echo_hello(image=image, cid=cid)
        elif name in BenchRunner.CMD_ARG:
            createtime, runtime = self.run_cmd_arg(image=image, cid=cid, runargs=BenchRunner.CMD_ARG[name])
        elif name in BenchRunner.CMD_ARG_WAIT:
            createtime, runtime = self.run_cmd_arg_wait(image=image, cid=cid, runargs=BenchRunner.CMD_ARG_WAIT[name])
        elif name in BenchRunner.CMD_STDIN:
            createtime, runtime = self.run_cmd_stdin(image=image, cid=cid, runargs=BenchRunner.CMD_STDIN[name])
        else:
            print 'Unknown bench: '+name
            exit(1)

        return pulltime, createtime, runtime

    def convert_echo_hello(self, src, dest):
        period=10
        cmd = ('%s -cni -period %s -entrypoint \'["/bin/sh", "-c"]\' -args \'["echo hello"]\' %s %s' %
               (self.optimizer, period, src, dest))
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)

    def convert_cmd_arg(self, src, dest, runargs):
        period = 30
        assert(len(runargs.mount) == 0)
        entry = ""
        if runargs.arg != "": # FIXME: this is naive...
            entry = '-entrypoint \'["/bin/sh", "-c"]\''
        cmd = ('%s -cni -period %s %s %s %s %s' %
               (self.optimizer, period, entry, genargs_for_optimization(runargs.arg), src, dest))
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)

    def convert_cmd_arg_wait(self, src, dest, runargs):
        period = 90
        env = ' '.join(['-env %s=%s' % (k,v) for k,v in runargs.env.iteritems()])
        cmd = ('%s -cni -period %s %s %s %s %s' %
               (self.optimizer, period, env, genargs_for_optimization(runargs.arg), src, dest))
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)

    def convert_cmd_stdin(self, src, dest, runargs):
        mounts = ''
        for a,b in runargs.mount:
            a = os.path.join(os.path.dirname(os.path.abspath(__file__)), a)
            a = tmp_copy(a)
            mounts += '--mount type=bind,src=%s,dst=%s,options=rbind ' % (a,b)
        period = 60
        cmd = ('%s -i -cni -period %s %s -entrypoint \'["/bin/sh", "-c"]\' %s %s %s' %
               (self.optimizer, period, mounts, genargs_for_optimization(runargs.stdin_sh), src, dest))
        print cmd
        p = subprocess.Popen(cmd, shell=True, stdin=subprocess.PIPE, stdout=subprocess.PIPE)
        print runargs.stdin
        out,_ = p.communicate(runargs.stdin)
        print out
        p.wait()
        assert(p.returncode == 0)

    def push_img(self, dest):
        cmd = '%s %s' % (self.pusher, dest)
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)

    def pull_img(self, src):
        cmd = '%s %s' % (self.puller, src)
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)

    def copy_img(self, src, dest):
        cmd = 'crane copy %s %s' % (src, dest)
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)

    def convert_and_push_img(self, src, dest):
        self.pull_img(src)
        cmd = '%s --no-optimize %s %s' % (self.optimizer, src, dest)
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)
        self.push_img(dest)

    def optimize_img(self, name, src, dest):
        self.pull_img(src)
        if name in BenchRunner.ECHO_HELLO:
            self.convert_echo_hello(src=src, dest=dest)
        elif name in BenchRunner.CMD_ARG:
            self.convert_cmd_arg(src=src, dest=dest, runargs=BenchRunner.CMD_ARG[name])
        elif name in BenchRunner.CMD_ARG_WAIT:
            self.convert_cmd_arg_wait(src=src, dest=dest, runargs=BenchRunner.CMD_ARG_WAIT[name])
        elif name in BenchRunner.CMD_STDIN:
            self.convert_cmd_stdin(src=src, dest=dest, runargs=BenchRunner.CMD_STDIN[name])
        else:
            print 'Unknown bench: '+name
            exit(1)
        self.push_img(dest)

    def prepare(self, bench):
        name = bench.name
        src = '%s/%s' % (self.srcrepository, name)
        self.copy_img(src=src, dest=format_repo(LEGACY_MODE, self.repository, name))
        self.convert_and_push_img(src=src, dest=format_repo(ESTARGZ_NOOPT_MODE, self.repository, name))
        self.optimize_img(name=name, src=src, dest=format_repo(ESTARGZ_MODE, self.repository, name))

    def operation(self, op, bench, cid):
        if op == 'run':
            return self.run(bench, cid)
        elif op == 'prepare':
            self.prepare(bench)
            return 0, 0, 0
        else:
            print 'Unknown operation: '+op
            exit(1)

class ContainerdController:
    def __init__(self, is_lazypull=False):
        self.is_lazypull = is_lazypull

    def pull_cmd(self, image):
        base_cmd = "%s i pull" % CTR
        if self.is_lazypull:
            base_cmd = "ctr-remote i rpull"
        cmd = '%s %s' % (base_cmd, image)
        print cmd
        return cmd

    def create_echo_hello_cmd(self, image, cid):
        snapshotter_opt = ""
        if self.is_lazypull:
            snapshotter_opt = "--snapshotter=stargz"
        cmd = '%s c create --net-host %s -- %s %s echo hello' % (CTR, snapshotter_opt, image, cid)
        print cmd
        return cmd

    def create_cmd_arg_cmd(self, image, cid, runargs):
        snapshotter_opt = ""
        if self.is_lazypull:
            snapshotter_opt = "--snapshotter=stargz"
        cmd = '%s c create --net-host %s ' % (CTR, snapshotter_opt)
        cmd += '-- %s %s ' % (image, cid)
        cmd += runargs.arg
        print cmd
        return cmd

    def create_cmd_arg_wait_cmd(self, image, cid, runargs):
        snapshotter_opt = ""
        if self.is_lazypull:
            snapshotter_opt = "--snapshotter=stargz"
        env = ' '.join(['--env %s=%s' % (k,v) for k,v in runargs.env.iteritems()])
        cmd = ('%s c create --net-host %s %s -- %s %s %s' %
               (CTR, snapshotter_opt, env, image, cid, runargs.arg))
        print cmd
        return cmd

    def create_cmd_stdin_cmd(self, image, cid, runargs):
        snapshotter_opt = ""
        if self.is_lazypull:
            snapshotter_opt = "--snapshotter=stargz"
        cmd = '%s c create --net-host %s ' % (CTR, snapshotter_opt)
        for a,b in runargs.mount:
            a = os.path.join(os.path.dirname(os.path.abspath(__file__)), a)
            a = tmp_copy(a)
            cmd += '--mount type=bind,src=%s,dst=%s,options=rbind ' % (a,b)
        cmd += '-- %s %s ' % (image, cid)
        if runargs.stdin_sh:
            cmd += runargs.stdin_sh # e.g., sh -c

        print cmd
        return cmd

    def task_start_cmd(self, cid):
        cmd = '%s t start %s' % (CTR, cid)
        print cmd
        return cmd

    def task_kill_cmd(self, cid):
        cmd = '%s t kill -s 9 %s' % (CTR, cid)
        print cmd
        return cmd

    def cleanup(self, name, image):
        print "Cleaning up environment..."
        cmd = '%s t kill -s 9 %s' % (CTR, name)
        print cmd
        rc = os.system(cmd) # sometimes containers already exit. we ignore the failure.
        cmd = '%s c rm %s' % (CTR, name)
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)
        cmd = '%s image rm %s' % (CTR, image)
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)
        cmd = os.path.join(os.path.dirname(os.path.abspath(__file__)), '../reboot_containerd.sh')
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)

class PodmanController:
    def pull_cmd(self, image):
        cmd = '%s pull docker://%s' % (PODMAN, image)
        print cmd
        return cmd

    def create_echo_hello_cmd(self, image, cid):
        cmd = '%s create --name %s docker://%s echo hello' % (PODMAN, cid, image)
        print cmd
        return cmd

    def create_cmd_arg_cmd(self, image, cid, runargs):
        cmd = '%s create --name %s docker://%s ' % (PODMAN, cid, image)
        cmd += runargs.arg
        print cmd
        return cmd

    def create_cmd_arg_wait_cmd(self, image, cid, runargs):
        env = ' '.join(['--env %s=%s' % (k,v) for k,v in runargs.env.iteritems()])
        cmd = ('%s create %s --name %s docker://%s %s ' %
               (PODMAN, env, cid, image, runargs.arg))
        print cmd
        return cmd

    def create_cmd_stdin_cmd(self, image, cid, runargs):
        cmd = '%s create -i ' % PODMAN
        for a,b in runargs.mount:
            a = os.path.join(os.path.dirname(os.path.abspath(__file__)), a)
            a = tmp_copy(a)
            cmd += '--mount type=bind,src=%s,dst=%s ' % (a,b)
        cmd += '--name %s docker://%s ' % (cid, image)
        if runargs.stdin_sh:
            cmd += runargs.stdin_sh # e.g., sh -c

        print cmd
        return cmd

    def task_start_cmd(self, cid):
        cmd = '%s start -a %s' % (PODMAN, cid)
        print cmd
        return cmd

    def task_kill_cmd(self, cid):
        cmd = '%s kill -s 9 %s' % (PODMAN, cid)
        print cmd
        return cmd

    def cleanup(self, name, image):
        print "Cleaning up environment..."
        cmd = '%s kill -s 9 %s' % (PODMAN, name)
        print cmd
        rc = os.system(cmd) # sometimes containers already exit. we ignore the failure.
        cmd = '%s rm %s' % (PODMAN, name)
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)
        cmd = '%s image rm %s' % (PODMAN, image)
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)
        cmd = os.path.join(os.path.dirname(os.path.abspath(__file__)), '../reboot_store.sh')
        print cmd
        rc = os.system(cmd)
        assert(rc == 0)

def main():
    if len(sys.argv) == 1:
        print 'Usage: bench.py [OPTIONS] [BENCHMARKS]'
        print 'OPTIONS:'
        print '--repository=<repository>'
        print '--srcrepository=<repository>'
        print '--all'
        print '--list'
        print '--list-json'
        print '--experiments'
        print '--runtime'
        print '--op=(prepare|run)'
        print '--mode=(%s|%s|%s)' % (LEGACY_MODE, ESTARGZ_NOOPT_MODE, ESTARGZ_MODE)
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
        cid = '%s_bench_%d' % (bench.name.replace(':', '-').replace('/', '-'), random.randint(1,1000000))

        elapsed_times = []
        pull_times = []
        create_times = []
        run_times = []

        for i in range(trytimes):
            start = time.time()
            pulltime, createtime, runtime = runner.operation(op, bench, cid)
            elapsed = time.time() - start
            if op == "run":
                runner.cleanup(cid, bench)
            elapsed_times.append(elapsed)
            pull_times.append(pulltime)
            create_times.append(createtime)
            run_times.append(runtime)
            print 'ITERATION %s:' % i
            print 'elapsed %s' % elapsed
            print 'pull    %s' % pulltime
            print 'create  %s' % createtime
            print 'run     %s' % runtime

        row = {'mode':'%s' % runner.mode, 'repo':bench.name, 'bench':bench.name, 'elapsed':sum(elapsed_times) / len(elapsed_times), 'elapsed_pull':sum(pull_times) / len(pull_times), 'elapsed_create':sum(create_times) / len(create_times), 'elapsed_run':sum(run_times) / len(run_times)}
        js = json.dumps(row)
        print '%s%s,' % (BENCHMARKOUT_MARK, js)
        sys.stdout.flush()

if __name__ == '__main__':
    main()
    exit(0)
