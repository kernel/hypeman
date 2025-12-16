# Minimal CUDA image for GPU inference testing
# 
# NO NVIDIA DRIVER INSTALLATION NEEDED!
# hypeman automatically injects the matching driver libraries at VM boot time.
# See lib/devices/GPU.md for documentation on driver injection.
#
# This image demonstrates that standard CUDA runtime images work out of the box
# with hypeman's GPU passthrough - no driver version matching required.

FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04

# Install dependencies and Ollama
# Note: We use the runtime image (not devel) since we don't need CUDA compilation tools
RUN apt-get update && \
    apt-get install -y curl ca-certificates python3 && \
    curl -fsSL https://ollama.com/install.sh | sh && \
    rm -rf /var/lib/apt/lists/*

# Add test scripts for verifying GPU access
COPY test-nvml.py /usr/local/bin/test-nvml.py
COPY test-cuda.py /usr/local/bin/test-cuda.py
RUN chmod +x /usr/local/bin/test-nvml.py /usr/local/bin/test-cuda.py

# Ensure libraries are in the path (hypeman injects to /usr/lib/x86_64-linux-gnu)
ENV LD_LIBRARY_PATH=/usr/local/cuda/lib64:/usr/lib/x86_64-linux-gnu:${LD_LIBRARY_PATH}
ENV PATH=/usr/local/cuda/bin:/usr/bin:${PATH}

EXPOSE 11434
CMD ["ollama", "serve"]
