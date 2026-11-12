"""Exercise the patched vfio_load_buffer body with bounded mocked I/O."""
import pathlib
import subprocess
import tempfile
import sys

source = (pathlib.Path(sys.argv[1]) / 'hw/vfio/migration.c').read_text()
start = source.index('static int vfio_load_buffer(')
end = source.index('\nint vfio_save_device_config_state', start)
function = source[start:end]
prefix = r'''
#include <glib.h>
#include <stdint.h>
#include <unistd.h>
#include <errno.h>
#include <string.h>
#include <assert.h>
#define VFIO_MIG_DEFAULT_DATA_BUFFER_SIZE (1024 * 1024)
typedef struct { unsigned char *data; size_t length, offset; int error; } QEMUFile;
typedef struct { int data_fd; } VFIOMigration;
typedef struct { VFIOMigration *migration; const char *name; } VFIODevice;
static unsigned char output[3 * 1024 * 1024];
static size_t used, calls, max_write;
static int interrupt_once, fail_errno, zero_once;
static size_t qemu_get_buffer(QEMUFile *f, uint8_t *buffer, size_t size) {
    size_t amount = MIN(size, f->length - f->offset);
    memcpy(buffer, f->data + f->offset, amount); f->offset += amount;
    return amount;
}
static int qemu_file_get_error(QEMUFile *f) { return f->error; }
static ssize_t fake_write(int fd, const void *data, size_t size) {
    (void)fd; calls++;
    if (interrupt_once) { interrupt_once = 0; errno = EINTR; return -1; }
    if (fail_errno) { errno = fail_errno; return -1; }
    if (zero_once) { zero_once = 0; return 0; }
    size = MIN(size, max_write);
    assert(used + size <= sizeof(output));
    memcpy(output + used, data, size); used += size; return size;
}
#define write fake_write
#define trace_vfio_load_state_device_data(...) ((void)0)
'''
suffix = r'''
static void reset(void) {
    used = calls = 0; max_write = sizeof(output);
    interrupt_once = fail_errno = zero_once = 0;
}
int main(void) {
    unsigned char *input = g_malloc(sizeof(output));
    for (size_t i = 0; i < sizeof(output); i++) input[i] = (i * 37) % 251;
    VFIOMigration migration = {.data_fd = 7};
    VFIODevice device = {.migration = &migration, .name = "test"};
    size_t sizes[] = {0, 1, 32768, 1048576, 1048593, sizeof(output)};
    for (size_t i = 0; i < G_N_ELEMENTS(sizes); i++) {
        reset(); QEMUFile f = {.data = input, .length = sizes[i]};
        assert(vfio_load_buffer(&f, &device, sizes[i]) == 0);
        assert(used == sizes[i]); assert(!memcmp(output, input, used));
        assert(calls == (sizes[i] + 1048575) / 1048576);
    }
    reset(); max_write = 73;
    QEMUFile partial = {.data = input, .length = 1048593};
    assert(vfio_load_buffer(&partial, &device, partial.length) == 0);
    assert(used == partial.length && !memcmp(output, input, used));
    reset(); interrupt_once = 1;
    QEMUFile interrupt = {.data = input, .length = 1048593};
    assert(vfio_load_buffer(&interrupt, &device, interrupt.length) == 0);
    assert(used == interrupt.length && !memcmp(output, input, used));
    reset(); zero_once = 1;
    QEMUFile zero = {.data = input, .length = 128};
    assert(vfio_load_buffer(&zero, &device, zero.length) == -EIO);
    reset(); fail_errno = ENOSPC;
    QEMUFile failure = {.data = input, .length = 128};
    assert(vfio_load_buffer(&failure, &device, failure.length) == -ENOSPC);
    reset(); QEMUFile truncated = {.data = input, .length = 127};
    assert(vfio_load_buffer(&truncated, &device, 128) == -EIO && used == 0);
    reset(); QEMUFile read_error = {.data = input, .length = 127, .error = -EINVAL};
    assert(vfio_load_buffer(&read_error, &device, 128) == -EINVAL && used == 0);
    g_free(input);
    return 0;
}
'''
with tempfile.TemporaryDirectory() as directory:
    root = pathlib.Path(directory)
    (root / 'test.c').write_text(prefix + function + suffix)
    flags = subprocess.check_output(['pkg-config', '--cflags', '--libs', 'glib-2.0'], text=True).split()
    subprocess.run(['cc', '-Wall', '-Wextra', '-Werror', '-fsanitize=address,undefined', '-g', str(root / 'test.c'), '-o', str(root / 'test'), *flags], check=True)
    subprocess.run([str(root / 'test')], check=True)
print('passed: exact batching, byte integrity, partial writes, EINTR, zero write, write error, truncated input and read error (ASan/UBSan)')
