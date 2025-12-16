#!/usr/bin/env python3
"""Test basic CUDA operations."""
import ctypes
import os
import sys

def test_cuda():
    """Try to use the CUDA driver API."""
    print("=== CUDA Driver Test ===")
    print(f"LD_LIBRARY_PATH: {os.environ.get('LD_LIBRARY_PATH', 'not set')}")
    
    # Try loading libcuda
    try:
        cuda = ctypes.CDLL("libcuda.so")
        print("✓ Loaded libcuda.so")
    except OSError as e:
        print(f"✗ Failed to load libcuda.so: {e}")
        return False
    
    # Initialize CUDA
    ret = cuda.cuInit(0)
    if ret != 0:
        print(f"✗ cuInit failed with code: {ret}")
        return False
    print("✓ cuInit succeeded")
    
    # Get device count
    count = ctypes.c_int()
    ret = cuda.cuDeviceGetCount(ctypes.byref(count))
    if ret != 0:
        print(f"✗ cuDeviceGetCount failed with code: {ret}")
        return False
    print(f"✓ Found {count.value} CUDA device(s)")
    
    if count.value == 0:
        return False
    
    # Get device name
    device = ctypes.c_int()
    ret = cuda.cuDeviceGet(ctypes.byref(device), 0)
    if ret != 0:
        print(f"✗ cuDeviceGet failed: {ret}")
        return False
    
    name = ctypes.create_string_buffer(256)
    ret = cuda.cuDeviceGetName(name, 256, device)
    if ret == 0:
        print(f"✓ Device 0: {name.value.decode()}")
    
    # Get total memory
    total_mem = ctypes.c_size_t()
    ret = cuda.cuDeviceTotalMem_v2(ctypes.byref(total_mem), device)
    if ret == 0:
        print(f"✓ Total memory: {total_mem.value / (1024**3):.1f} GB")
    
    return True

if __name__ == "__main__":
    success = test_cuda()
    print()
    print("Result:", "CUDA WORKS" if success else "CUDA FAILED")
    sys.exit(0 if success else 1)

