set -xe
echo "restoring VM"
time sudo ch-remote --api-socket /tmp/cloud-hypervisor-restore.sock \
     restore source_url=file:///tmp/snapshot-test
echo "resuming VM"
time sudo ch-remote --api-socket /tmp/cloud-hypervisor-restore.sock resume
echo "done"
