set -xe
SOCKET=/tmp/ch.sock
echo "restoring VM"
time sudo ch-remote --api-socket $SOCKET \
     restore source_url=file:///tmp/snapshot-test
echo "resuming VM"
time sudo ch-remote --api-socket $SOCKET resume
echo "done"
