BIN := gw

build:
	go build -o $(BIN) .

install: build
	go install .
	@echo "installed $$(go env GOPATH)/bin/$(BIN)"
	@echo "rebuilt ./$(BIN)"

clean:
	rm -f $(BIN)

.PHONY: build install clean
