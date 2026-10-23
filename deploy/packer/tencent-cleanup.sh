#!/usr/bin/env bash
set -euo pipefail

cloud-init clean --logs --seed
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id /etc/ssh/ssh_host_*
rm -f /root/.ssh/authorized_keys /home/ubuntu/.ssh/authorized_keys
passwd -l ubuntu
