set -xe
sudo rm /tmp/cloud-hypervisor-restore.sock || true
sudo cloud-hypervisor \
        --kernel ./hypervisor-fw \
        --cpus boot=4 \
        --memory size=1024M \
  	--restore "source_url=file:///tmp/snapshot-test" \
	--disk path=focal-server-cloudimg-amd64.raw path=/tmp/ubuntu-cloudinit.img \
	--api-socket path=/tmp/cloud-hypervisor-restore.sock \
	# --console pty \
  	# --serial pty
	
