BIN := gw

build:
	go build -o $(BIN) .

install:
	go install .

clean:
	rm -f $(BIN)

.PHONY: build install clean
