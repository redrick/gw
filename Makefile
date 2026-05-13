BIN := gw
PREFIX := $(HOME)/Projects/golang/bin

build:
	go build -o $(BIN) .

install: build
	mkdir -p $(PREFIX)
	cp $(BIN) $(PREFIX)/$(BIN)

clean:
	rm -f $(BIN)

.PHONY: build install clean
