# Use Ubuntu as base image
FROM ubuntu:22.04

RUN apt-get update && apt-get install -y \
    build-essential \
    flex \
    bison \
    libssl-dev \
    libelf-dev \
    bc \
    wget \
    python3 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

COPY ./build_kernel.sh /build/
RUN chmod +x /build/build_kernel.sh

ENTRYPOINT ["/build/build_kernel.sh"]
