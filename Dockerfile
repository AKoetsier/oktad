FROM golang:1.8

RUN curl https://glide.sh/get | sh
RUN apt-get update && apt-get install -y libgnome-keyring-dev libglib2.0-dev && apt-get clean