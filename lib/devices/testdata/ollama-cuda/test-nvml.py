#!/usr/bin/env python3
"""Test NVML GPU detection - matches what Ollama does internally."""
import ctypes
import os

def test_nvml():
    """Try to initialize NVML and detect GPUs."""
    # Try different library paths
    lib_paths = [
        "libnvidia-ml.so.1",
        "libnvidia-ml.so",
        "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
    ]
    
    nvml = None
    for path in lib_paths:
        try:
            nvml = ctypes.CDLL(path)
            print(f"✓ Loaded NVML from: {path}")
            break
        except OSError as e:
            print(f"✗ Failed to load {path}: {e}")
    
    if nvml is None:
        print("ERROR: Could not load NVML library")
        return False
    
    # Try to initialize
    try:
        ret = nvml.nvmlInit_v2()
        if ret != 0:
            print(f"✗ nvmlInit_v2 failed with code: {ret}")
            # Error codes: 1=uninitialized, 2=invalid argument, 3=not supported,
            # 9=driver not loaded, 12=library not found
            error_names = {
                1: "NVML_ERROR_UNINITIALIZED",
                2: "NVML_ERROR_INVALID_ARGUMENT",  
                3: "NVML_ERROR_NOT_SUPPORTED",
                9: "NVML_ERROR_DRIVER_NOT_LOADED",
                12: "NVML_ERROR_LIB_RM_VERSION_MISMATCH",
                255: "NVML_ERROR_UNKNOWN",
            }
            print(f"   Error name: {error_names.get(ret, 'UNKNOWN')}")
            return False
        print("✓ nvmlInit_v2 succeeded")
    except Exception as e:
        print(f"✗ nvmlInit_v2 exception: {e}")
        return False
    
    # Get device count
    try:
        count = ctypes.c_uint()
        ret = nvml.nvmlDeviceGetCount_v2(ctypes.byref(count))
        if ret != 0:
            print(f"✗ nvmlDeviceGetCount failed with code: {ret}")
            return False
        print(f"✓ Found {count.value} GPU(s)")
    except Exception as e:
        print(f"✗ nvmlDeviceGetCount exception: {e}")
        return False
    
    # Shutdown
    nvml.nvmlShutdown()
    return count.value > 0

if __name__ == "__main__":
    print("=== NVML GPU Detection Test ===")
    print(f"LD_LIBRARY_PATH: {os.environ.get('LD_LIBRARY_PATH', 'not set')}")
    print()
    
    # Check device nodes
    print("Device nodes:")
    for dev in ["/dev/nvidia0", "/dev/nvidiactl", "/dev/nvidia-uvm"]:
        exists = os.path.exists(dev)
        print(f"  {dev}: {'exists' if exists else 'MISSING'}")
    print()
    
    success = test_nvml()
    print()
    print("Result:", "GPU DETECTED" if success else "NO GPU FOUND")
    exit(0 if success else 1)


