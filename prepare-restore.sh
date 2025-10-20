set -xe
SOCKET=/tmp/ch.sock
sudo rm $SOCKET || true
sudo cloud-hypervisor \
	--api-socket path=$SOCKET

