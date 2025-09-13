FROM ubuntu:latest

RUN apt-get update
RUN apt-get install -y python3-virtualenv python3-pip git ffmpeg

COPY requirements.txt requirements.txt

RUN virtualenv .env
ENV PATH=".env/bin:$PATH"

RUN pip install -r requirements.txt
