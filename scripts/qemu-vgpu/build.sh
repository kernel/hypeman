#!/usr/bin/env bash
set -euo pipefail

version=11.1.3-vgpu.1
commit=005c61c0ef6f6ae615283440b4bb7be51b2c6ddd
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
output=$(realpath -m -- "${1:?usage: build.sh OUTPUT_DIRECTORY}")
if [[ -e "$output" ]]; then
  echo "refusing to replace an existing runtime directory: $output" >&2
  exit 1
fi
if [[ $(uname -s) != Linux || $(uname -m) != x86_64 ]]; then
  echo "the vGPU runtime requires Linux x86_64" >&2
  exit 1
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

git init -q "$work/source"
git -C "$work/source" remote add origin https://github.com/qemu/qemu.git
git -C "$work/source" fetch --depth=1 origin "$commit"
git -C "$work/source" checkout --detach FETCH_HEAD
test "$(git -C "$work/source" rev-parse HEAD)" = "$commit"
git -C "$work/source" apply "$script_dir/vfio-batched-load.patch"
python3 "$script_dir/test-batched-load.py" "$work/source"
mkdir "$work/build"
cd "$work/build"
"$work/source/configure" --prefix=/usr --target-list=x86_64-softmmu \
  --enable-kvm --disable-tcg --disable-docs --disable-tools --disable-user \
  --disable-werror --disable-gtk --disable-sdl --disable-vnc --disable-opengl \
  --disable-virglrenderer --enable-trace-backends=log
ninja -j"${JOBS:-4}" qemu-system-x86_64

mkdir -p -- "$(dirname -- "$output")"
stage=$(mktemp -d "${output}.tmp.XXXXXX")
trap 'rm -rf "$work" "$stage"' EXIT
install -m 0755 qemu-system-x86_64 "$stage/qemu-system-x86_64"
mkdir "$stage/firmware"
install -m 0644 "$work/source"/pc-bios/*.bin "$work/source"/pc-bios/*.rom "$stage/firmware/"
install -m 0644 "$work/source/COPYING" "$stage/COPYING"
tar --exclude=.git -czf "$stage/source.tar.gz" -C "$work/source" .
python3 - "$stage" "$version" "$commit" <<'PY'
import hashlib, json, pathlib, struct, sys
root = pathlib.Path(sys.argv[1])
firmware = hashlib.sha256()
for path in sorted((root / 'firmware').iterdir()):
    firmware.update(path.name.encode() + b'\0')
    firmware.update(struct.pack('<Q', path.stat().st_size))
    firmware.update(path.read_bytes())
(root / 'manifest.json').write_text(json.dumps({
    'version': sys.argv[2], 'source_commit': sys.argv[3],
    'binary_sha256': hashlib.sha256((root / 'qemu-system-x86_64').read_bytes()).hexdigest(),
    'firmware_sha256': firmware.hexdigest(),
}, indent=2) + '\n')
PY
mv -T --no-clobber "$stage" "$output"
test ! -e "$stage"
"$output/qemu-system-x86_64" --version
printf 'runtime package: %s\n' "$output"
