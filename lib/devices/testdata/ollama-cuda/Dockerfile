# Minimal CUDA image for GPU inference testing
# 
# IMPORTANT: NVIDIA guest drivers must be pre-installed in the image.
# hypeman does NOT inject drivers - the guest image is responsible for
# including matching NVIDIA drivers for vGPU/GPU passthrough.
#
# This image includes the NVIDIA driver userspace libraries from the CUDA
# runtime image plus nvidia-utils for nvidia-smi.

FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04

# Install NVIDIA driver utilities and dependencies
# The nvidia-utils package provides nvidia-smi and matches the driver version
# needed for the vGPU profile assigned by hypeman.
RUN apt-get update && \
    apt-get install -y curl ca-certificates python3 nvidia-utils-550 && \
    curl -fsSL https://ollama.com/install.sh | sh && \
    rm -rf /var/lib/apt/lists/*

# Add test scripts for verifying GPU access
COPY test-nvml.py /usr/local/bin/test-nvml.py
COPY test-cuda.py /usr/local/bin/test-cuda.py
RUN chmod +x /usr/local/bin/test-nvml.py /usr/local/bin/test-cuda.py

# Ensure libraries are in the path
ENV LD_LIBRARY_PATH=/usr/local/cuda/lib64:/usr/lib/x86_64-linux-gnu:${LD_LIBRARY_PATH}
ENV PATH=/usr/local/cuda/bin:/usr/bin:${PATH}

EXPOSE 11434
CMD ["ollama", "serve"]
