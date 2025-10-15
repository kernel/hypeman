set -xe
sudo rm /tmp/cloud-hypervisor-restore.sock || true
sudo cloud-hypervisor \
	--api-socket path=/tmp/cloud-hypervisor-restore.sock

