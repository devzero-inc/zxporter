FROM ubuntu:22.04

RUN apt update && apt install -y build-essential libncurses-dev bison flex libssl-dev libelf-dev wget

WORKDIR /linux

COPY linux_build.sh /linux/linux_build.sh

RUN chmod +x /linux/linux_build.sh

CMD ["./linux_build.sh"]