set -xe
SOCKET=/tmp/ch.sock
echo "cleaning up"
rm -rf /tmp/snapshot-test || true
mkdir -p /tmp/snapshot-test
echo "Pausing..."
sudo ch-remote --api-socket $SOCKET pause
echo "Snap shotting..."
sudo ch-remote --api-socket $SOCKET snapshot file:///tmp/snapshot-test
echo "Resuming..."
sudo ch-remote --api-socket $SOCKET resume
echo "Done"
