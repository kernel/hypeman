sudo cloud-hypervisor \
	--kernel ./hypervisor-fw \
	--disk path=focal-server-cloudimg-amd64.raw path=/tmp/ubuntu-cloudinit.img \
	--cpus boot=4 \
	--memory size=1024M \
	--api-socket path=/tmp/cloud-hypervisor-test.sock \
	--net "tap=,mac=,ip=,mask="
