.PHONY: build server admin run run-admin standin test clean

build: server admin

server:
	CGO_ENABLED=1 go build -o bin/server ./cmd/server

# The dashboard needs neither cgo nor x264.
admin:
	CGO_ENABLED=0 go build -o bin/admin ./cmd/admin

standin: media/standin.mp4

media/standin.mp4:
	./scripts/make-standin.sh $@

run: server standin
	./bin/server $(ARGS)

run-admin: admin
	./bin/admin $(ARGS)

test:
	go test ./...

clean:
	rm -rf bin

# Shared simulcast source for testing: ffmpeg encodes media/standin.mp4 into
# three aligned H.264 renditions and sends them as MPEG-TS over UDP to
# 127.0.0.1:5001-5003. Run `make run ARGS="--simulcast high=127.0.0.1:5001@2500k,mid=127.0.0.1:5002@800k,low=127.0.0.1:5003@250k"` next to it.
renditions: standin
	./scripts/simulcast-renditions.sh media/standin.mp4
