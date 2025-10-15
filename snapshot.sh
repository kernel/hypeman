set -xe
sudo ch-remote --api-socket /tmp/cloud-hypervisor-test.sock pause
rm -rf /tmp/snapshot-test || true
mkdir -p /tmp/snapshot-test
sudo ch-remote --api-socket /tmp/cloud-hypervisor-test.sock snapshot file:///tmp/snapshot-test
sudo ch-remote --api-socket /tmp/cloud-hypervisor-test.sock resume
