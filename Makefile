all: orstedgz

orsted: main.go
	go build -o orsted main.go

orstedgz: orsted
	gzip -f -9 -k orsted

clean:
	rm -f orsted orsted.gz
