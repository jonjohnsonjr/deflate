.PHONY: build
build:
	GOOS=js GOARCH=wasm go build -o mos.deflate.wasm .

.PHONY: local
local: build
	python3 -m http.server 8080
