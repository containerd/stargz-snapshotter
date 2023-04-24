#   Copyright The containerd Authors.
#
#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

Vagrant.configure("2") do |config|
  config.vm.box = "ubuntu/jammy64"
  memory = 4096
  cpus = 2
  disk_size = 60
  config.vm.provider :virtualbox do |v, o|
    v.memory = memory
    v.cpus = cpus
    # Needs env var VAGRANT_EXPERIMENTAL="disks"
    o.vm.disk :disk, size: "#{disk_size}GB", primary: true
  end
  config.vm.provider :libvirt do |v|
    v.memory = memory
    v.cpus = cpus
    v.machine_virtual_size = disk_size
  end
  config.vm.synced_folder ".", "/vagrant", type: "rsync"
  config.vm.provision "shell", path: "./script/cri-o/vagrant/setup.sh"
end
