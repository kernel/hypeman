# Minimal CUDA image for GPU inference testing
# 
# This image includes CUDA runtime libraries but NOT kernel drivers.
# Kernel drivers are installed at runtime via DKMS, which works because
# hypeman automatically installs kernel headers on boot.
#
# The test installs nvidia-driver-550-server at runtime, which:
# 1. Includes nvidia-smi and userspace libraries
# 2. Uses DKMS to build kernel modules for hypeman's kernel

FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04

# Install dependencies for DKMS module building and ollama
# DO NOT install nvidia drivers here - they're installed at runtime
RUN apt-get update && \
    apt-get install -y \
        curl \
        ca-certificates \
        python3 \
        dkms \
        build-essential \
        kmod \
    && curl -fsSL https://ollama.com/install.sh | sh \
    && rm -rf /var/lib/apt/lists/*

# Add test scripts for verifying GPU access
COPY test-nvml.py /usr/local/bin/test-nvml.py
COPY test-cuda.py /usr/local/bin/test-cuda.py
RUN chmod +x /usr/local/bin/test-nvml.py /usr/local/bin/test-cuda.py

# Ensure libraries are in the path
ENV LD_LIBRARY_PATH=/usr/local/cuda/lib64:/usr/lib/x86_64-linux-gnu:${LD_LIBRARY_PATH}
ENV PATH=/usr/local/cuda/bin:/usr/bin:${PATH}

EXPOSE 11434
CMD ["ollama", "serve"]
